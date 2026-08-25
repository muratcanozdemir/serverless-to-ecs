package cost

import (
	"reflect"
	"sort"
	"testing"

	"serverless-to-ecs/internal/model"
)

func TestExtractPrefix(t *testing.T) {
	tests := []struct {
		id   string
		want string
	}{
		{"CreateOrderFn", "CreateOrder"},
		{"CreateOrderFunction", "CreateOrder"},
		{"CreateOrderLambda", "CreateOrder"},
		{"CreateOrderHandler", "CreateOrder"},
		{"CreateOrderWorker", "CreateOrder"},
		{"create-order-handler", "create"},
		{"create_order_handler", "create"},
		{"NoDelimiter", "NoDelimiter"},
	}
	for _, tt := range tests {
		t.Run(tt.id, func(t *testing.T) {
			got := extractPrefix(tt.id)
			if got != tt.want {
				t.Errorf("extractPrefix(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestGroupByPrefix_SingleID(t *testing.T) {
	got := groupByPrefix([]string{"OnlyFn"})
	want := map[string][]string{"OnlyFn": {"OnlyFn"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("groupByPrefix single ID = %v, want %v", got, want)
	}
}

func TestGroupServices_APIBacked(t *testing.T) {
	g := model.NewGraph()
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "MyAPI", Protocol: "REST"}
	g.Lambdas["Fn1"] = &model.Lambda{LogicalID: "Fn1", FunctionName: "fn1"}
	g.Lambdas["Fn2"] = &model.Lambda{LogicalID: "Fn2", FunctionName: "fn2"}
	g.Routes = append(g.Routes,
		model.APIRoute{APIID: "Api", Path: "/a", Method: "GET", TargetRef: "Fn1"},
		model.APIRoute{APIID: "Api", Path: "/b", Method: "POST", TargetRef: "Fn2"},
	)

	groups := GroupServices(g)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d: %+v", len(groups), groups)
	}
	got := groups[0]
	if got.Type != "api" {
		t.Errorf("expected type=api, got %q", got.Type)
	}
	sort.Strings(got.LambdaIDs)
	if !reflect.DeepEqual(got.LambdaIDs, []string{"Fn1", "Fn2"}) {
		t.Errorf("expected both lambdas grouped under the shared API, got %v", got.LambdaIDs)
	}
}

func TestGroupServices_QueueProcessor(t *testing.T) {
	g := model.NewGraph()
	g.Queues["Q"] = &model.SQSQueue{LogicalID: "Q", QueueName: "my-queue"}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn"}
	g.AddEdge("Q", "Fn", model.EdgeTriggers, "SQS event source")

	groups := GroupServices(g)
	if len(groups) != 1 || groups[0].Type != "queue-processor" {
		t.Fatalf("expected one queue-processor group, got %+v", groups)
	}
	if len(groups[0].LambdaIDs) != 1 || groups[0].LambdaIDs[0] != "Fn" {
		t.Errorf("expected Fn in the queue-processor group, got %v", groups[0].LambdaIDs)
	}
}

func TestGroupServices_KinesisConsumer(t *testing.T) {
	g := model.NewGraph()
	g.Streams["Stream"] = &model.KinesisStream{LogicalID: "Stream", ShardCount: 1}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn"}
	g.AddEdge("Stream", "Fn", model.EdgeTriggers, "Kinesis stream")

	groups := GroupServices(g)
	if len(groups) != 1 || groups[0].Type != "queue-processor" {
		t.Fatalf("expected one queue-processor-type group for a Kinesis consumer, got %+v", groups)
	}
	if len(groups[0].LambdaIDs) != 1 || groups[0].LambdaIDs[0] != "Fn" {
		t.Errorf("expected Fn in the Kinesis consumer group, got %v", groups[0].LambdaIDs)
	}
}

func TestGroupServices_S3EventProcessor(t *testing.T) {
	g := model.NewGraph()
	g.Buckets["Bucket"] = &model.S3Bucket{LogicalID: "Bucket", BucketName: "bucket"}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn"}
	g.AddEdge("Bucket", "Fn", model.EdgeTriggers, "S3 notification: s3:ObjectCreated:*")

	groups := GroupServices(g)
	if len(groups) != 1 || groups[0].Type != "queue-processor" {
		t.Fatalf("expected one queue-processor-type group for an S3 event processor, got %+v", groups)
	}
	if len(groups[0].LambdaIDs) != 1 || groups[0].LambdaIDs[0] != "Fn" {
		t.Errorf("expected Fn in the S3 event processor group, got %v", groups[0].LambdaIDs)
	}
}

func TestGroupServices_Scheduled(t *testing.T) {
	g := model.NewGraph()
	g.Rules["Rule"] = &model.EventBridgeRule{LogicalID: "Rule", Schedule: "rate(1 hour)"}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn"}
	g.AddEdge("Rule", "Fn", model.EdgeTriggers, "schedule: rate(1 hour)")

	groups := GroupServices(g)
	if len(groups) != 1 || groups[0].Type != "scheduled" || groups[0].Name != "scheduled-tasks" {
		t.Fatalf("expected one scheduled-tasks group, got %+v", groups)
	}
}

func TestGroupServices_Orchestrated(t *testing.T) {
	g := model.NewGraph()
	g.StepFuncs["SFN"] = &model.StepFunction{LogicalID: "SFN", Name: "MyWorkflow", TaskTargets: []string{"Fn"}}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn"}

	groups := GroupServices(g)
	if len(groups) != 1 || groups[0].Type != "orchestrated" {
		t.Fatalf("expected one orchestrated group, got %+v", groups)
	}
}

func TestGroupServices_StandaloneByPrefix(t *testing.T) {
	g := model.NewGraph()
	g.Lambdas["ReportFn"] = &model.Lambda{LogicalID: "ReportFn", FunctionName: "report"}

	groups := GroupServices(g)
	if len(groups) != 1 || groups[0].Type != "standalone" {
		t.Fatalf("expected one standalone group for an untriggered lambda, got %+v", groups)
	}
}

func TestGroupServices_PriorityOrderAssignsOnce(t *testing.T) {
	// A Lambda that's both API-routed AND SQS-triggered should end up in
	// exactly one group — the API group, since API-backed is priority 1.
	g := model.NewGraph()
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Queues["Q"] = &model.SQSQueue{LogicalID: "Q", QueueName: "q"}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Q", "Fn", model.EdgeTriggers, "SQS event source")

	groups := GroupServices(g)

	seen := 0
	for _, grp := range groups {
		for _, id := range grp.LambdaIDs {
			if id == "Fn" {
				seen++
				if grp.Type != "api" {
					t.Errorf("expected Fn to be claimed by the API group (higher priority), got group type %q", grp.Type)
				}
			}
		}
	}
	if seen != 1 {
		t.Errorf("expected Fn to appear in exactly one group, appeared in %d", seen)
	}
}

func TestGroupServices_Deterministic(t *testing.T) {
	g := model.NewGraph()
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Queues["Q1"] = &model.SQSQueue{LogicalID: "Q1", QueueName: "q1"}
	g.Queues["Q2"] = &model.SQSQueue{LogicalID: "Q2", QueueName: "q2"}
	g.Lambdas["A"] = &model.Lambda{LogicalID: "A", FunctionName: "a"}
	g.Lambdas["B"] = &model.Lambda{LogicalID: "B", FunctionName: "b"}
	g.Lambdas["C"] = &model.Lambda{LogicalID: "C", FunctionName: "c"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/a", Method: "GET", TargetRef: "A"})
	g.AddEdge("Q1", "B", model.EdgeTriggers, "SQS event source")
	g.AddEdge("Q2", "C", model.EdgeTriggers, "SQS event source")

	first := GroupServices(g)
	for i := 0; i < 20; i++ {
		got := GroupServices(g)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("GroupServices produced a different result on run %d:\nfirst: %+v\ngot:   %+v", i, first, got)
		}
	}
}
