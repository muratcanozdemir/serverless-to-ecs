package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"serverless-to-ecs/internal/model"
)

func testdataPath(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "examples", name)
}

func TestParseSyntheticStack(t *testing.T) {
	g, err := ParseFile(testdataPath("synthetic-stack.yaml"))
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	// Template-level checks.
	if !g.IsSAM {
		t.Error("expected IsSAM=true")
	}
	if g.Description == "" {
		t.Error("expected non-empty Description")
	}

	// Lambda inventory.
	expectLambdas := []string{
		"CreateOrderFn", "ProcessOrderFn", "FulfillOrderFn",
		"NotifyCustomerFn", "DailyReportFn", "InventoryCheckerFn",
	}
	for _, name := range expectLambdas {
		if _, ok := g.Lambdas[name]; !ok {
			t.Errorf("missing Lambda: %s", name)
		}
	}
	if got := len(g.Lambdas); got != len(expectLambdas) {
		t.Errorf("Lambda count: got %d, want %d", got, len(expectLambdas))
	}

	// Spot-check Lambda properties.
	fn := g.Lambdas["CreateOrderFn"]
	if fn.FunctionName != "ecommerce-create-order" {
		t.Errorf("CreateOrderFn name: got %q", fn.FunctionName)
	}
	if fn.Runtime != "python3.12" {
		t.Errorf("CreateOrderFn runtime: got %q (should inherit from Globals.Function)", fn.Runtime)
	}

	// Globals merge: resource-level MemorySize (512) must win over Globals (256).
	if fn.MemoryMB != 512 {
		t.Errorf("CreateOrderFn memory: resource override lost, got %d want 512", fn.MemoryMB)
	}

	// Globals merge: STAGE env var from Globals.Function.Environment.Variables
	// should be present alongside the function's own env vars.
	if fn.EnvVars["STAGE"] != "production" {
		t.Errorf("CreateOrderFn env STAGE: got %q, want %q (from Globals)", fn.EnvVars["STAGE"], "production")
	}

	// Globals merge: function that doesn't override Timeout should get Globals default (30).
	inventoryFn := g.Lambdas["InventoryCheckerFn"]
	if inventoryFn.TimeoutSec != 30 {
		t.Errorf("InventoryCheckerFn timeout: got %d, want 30 (from Globals)", inventoryFn.TimeoutSec)
	}

	// API Gateway.
	if _, ok := g.APIs["OrderApi"]; !ok {
		t.Error("missing API: OrderApi")
	}
	if g.APIs["OrderApi"].Protocol != "REST" {
		t.Errorf("OrderApi protocol: got %q, want REST", g.APIs["OrderApi"].Protocol)
	}

	// Step Functions.
	sf, ok := g.StepFuncs["OrderWorkflow"]
	if !ok {
		t.Fatal("missing StepFunction: OrderWorkflow")
	}
	if sf.Name != "ecommerce-order-workflow" {
		t.Errorf("OrderWorkflow name: got %q", sf.Name)
	}
	if sf.StateCount != 5 {
		t.Errorf("OrderWorkflow states: got %d, want 5", sf.StateCount)
	}
	// The workflow has a Choice state + sequential Tasks → should classify as mixed or choice.
	if sf.Pattern != model.SFNChoice {
		t.Errorf("OrderWorkflow pattern: got %q, want %q", sf.Pattern, model.SFNChoice)
	}
	if len(sf.TaskTargets) != 4 {
		t.Errorf("OrderWorkflow task targets: got %d, want 4", len(sf.TaskTargets))
	}

	// SQS.
	if _, ok := g.Queues["OrderQueue"]; !ok {
		t.Error("missing Queue: OrderQueue")
	}
	if _, ok := g.Queues["OrderDLQ"]; !ok {
		t.Error("missing Queue: OrderDLQ")
	}

	// SNS.
	if _, ok := g.Topics["NotificationTopic"]; !ok {
		t.Error("missing Topic: NotificationTopic")
	}

	// DynamoDB.
	table, ok := g.Tables["OrderTable"]
	if !ok {
		t.Fatal("missing Table: OrderTable")
	}
	if table.BillingMode != "PAY_PER_REQUEST" {
		t.Errorf("OrderTable billing: got %q", table.BillingMode)
	}
	if table.HashKey != "orderId" {
		t.Errorf("OrderTable hash key: got %q", table.HashKey)
	}
	if table.GSICount != 1 {
		t.Errorf("OrderTable GSI count: got %d, want 1", table.GSICount)
	}

	// EventBridge.
	rule, ok := g.Rules["StaleOrderCleanupRule"]
	if !ok {
		t.Fatal("missing Rule: StaleOrderCleanupRule")
	}
	if rule.Schedule != "rate(4 hours)" {
		t.Errorf("StaleOrderCleanupRule schedule: got %q", rule.Schedule)
	}

	// Edge checks — verify key relationships exist.
	edgeExists := func(from, to string, et model.EdgeType) bool {
		for _, e := range g.Edges {
			if e.From == from && e.To == to && e.Type == et {
				return true
			}
		}
		return false
	}

	// API Gateway → Lambda (SAM events).
	if !edgeExists("OrderApi", "CreateOrderFn", model.EdgeInvokes) {
		t.Error("missing edge: OrderApi → CreateOrderFn (invokes)")
	}

	// SQS → Lambda (SAM SQS event).
	if !edgeExists("OrderQueue", "ProcessOrderFn", model.EdgeTriggers) {
		t.Error("missing edge: OrderQueue → ProcessOrderFn (triggers)")
	}

	// Step Functions → Lambda (task states).
	if !edgeExists("OrderWorkflow", "InventoryCheckerFn", model.EdgeOrchestrates) {
		t.Error("missing edge: OrderWorkflow → InventoryCheckerFn (orchestrates)")
	}
	if !edgeExists("OrderWorkflow", "FulfillOrderFn", model.EdgeOrchestrates) {
		t.Error("missing edge: OrderWorkflow → FulfillOrderFn (orchestrates)")
	}

	// EventBridge → Lambda.
	if !edgeExists("StaleOrderCleanupRule", "ProcessOrderFn", model.EdgeTriggers) {
		t.Error("missing edge: StaleOrderCleanupRule → ProcessOrderFn (triggers)")
	}

	// SNS → SQS subscription.
	if !edgeExists("OrderEventsTopic", "OrderQueue", model.EdgeSubscribes) {
		t.Error("missing edge: OrderEventsTopic → OrderQueue (subscribes)")
	}

	// Lambda → DynamoDB (env var ref).
	if !edgeExists("CreateOrderFn", "OrderTable", model.EdgeReadsWrites) {
		t.Error("missing edge: CreateOrderFn → OrderTable (reads_writes)")
	}

	// S3.
	bucket, ok := g.Buckets["OrderAssetsBucket"]
	if !ok {
		t.Fatal("missing Bucket: OrderAssetsBucket")
	}
	if bucket.BucketName != "ecommerce-order-assets" {
		t.Errorf("OrderAssetsBucket name: got %q", bucket.BucketName)
	}

	// Unsupported resources should still be detected (e.g. ElastiCache).
	if len(g.Unsupported) == 0 {
		t.Error("expected unsupported resources (e.g. ElastiCache cluster)")
	}
	foundCache := false
	for _, u := range g.Unsupported {
		if u.ResourceType == "AWS::ElastiCache::CacheCluster" {
			foundCache = true
		}
	}
	if !foundCache {
		t.Error("expected AWS::ElastiCache::CacheCluster in unsupported resources")
	}

	t.Logf("Graph summary: %s", g.Summary())
	t.Logf("Edges: %d total", len(g.Edges))
	for _, e := range g.Edges {
		t.Logf("  %s -[%s]-> %s (%s)", e.From, e.Type, e.To, e.Detail)
	}
}

func TestLoadJSON(t *testing.T) {
	// Minimal JSON template — verifies JSON path works.
	json := `{
		"AWSTemplateFormatVersion": "2010-09-09",
		"Description": "JSON test",
		"Resources": {
			"MyFunc": {
				"Type": "AWS::Lambda::Function",
				"Properties": {
					"FunctionName": "test-fn",
					"Runtime": "go1.x",
					"Handler": "main",
					"MemorySize": 256,
					"Timeout": 10
				}
			}
		}
	}`

	tmpFile := t.TempDir() + "/test.json"
	if err := writeFile(tmpFile, []byte(json)); err != nil {
		t.Fatal(err)
	}

	g, err := ParseFile(tmpFile)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(g.Lambdas) != 1 {
		t.Errorf("expected 1 Lambda, got %d", len(g.Lambdas))
	}
	fn := g.Lambdas["MyFunc"]
	if fn.FunctionName != "test-fn" {
		t.Errorf("name: got %q", fn.FunctionName)
	}
	if fn.MemoryMB != 256 {
		t.Errorf("memory: got %d", fn.MemoryMB)
	}
}

func writeFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0644)
}
