package model

import "testing"

func TestNewGraph_InitializesAllMaps(t *testing.T) {
	g := NewGraph()

	if g.Lambdas == nil || g.APIs == nil || g.StepFuncs == nil || g.Rules == nil ||
		g.Queues == nil || g.Topics == nil || g.Tables == nil || g.HTTPIntegrations == nil ||
		g.Buckets == nil || g.Streams == nil || g.FileSystems == nil || g.AccessPoints == nil ||
		g.Secrets == nil || g.Parameters == nil {
		t.Fatal("NewGraph left one or more resource maps nil — callers assigning into them would panic")
	}

	// Assigning into a fresh graph's maps must not panic.
	g.Lambdas["Fn"] = &Lambda{LogicalID: "Fn"}
	g.HTTPIntegrations["Integration"] = "Fn"
	g.Buckets["Bucket"] = &S3Bucket{LogicalID: "Bucket"}
	g.Streams["Stream"] = &KinesisStream{LogicalID: "Stream"}
	g.FileSystems["FS"] = &EFSFileSystem{LogicalID: "FS"}
	g.AccessPoints["AP"] = &EFSAccessPoint{LogicalID: "AP"}
	g.Secrets["Secret"] = &SecretsManagerSecret{LogicalID: "Secret"}
	g.Parameters["Param"] = &SSMParameter{LogicalID: "Param"}
}

func TestGraph_AddEdge(t *testing.T) {
	g := NewGraph()
	g.AddEdge("A", "B", EdgeInvokes, "GET /x")

	if len(g.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(g.Edges))
	}
	got := g.Edges[0]
	want := Edge{From: "A", To: "B", Type: EdgeInvokes, Detail: "GET /x"}
	if got != want {
		t.Errorf("AddEdge produced %+v, want %+v", got, want)
	}
}

func TestGraph_Summary(t *testing.T) {
	g := NewGraph()
	g.Lambdas["Fn"] = &Lambda{LogicalID: "Fn"}
	g.APIs["Api"] = &APIGateway{LogicalID: "Api"}
	g.AddEdge("Api", "Fn", EdgeInvokes, "GET /x")
	g.Unsupported = append(g.Unsupported, UnsupportedResource{LogicalID: "Bucket", ResourceType: "AWS::S3::Bucket"})

	summary := g.Summary()

	want := "lambdas=1 apis=1 stepfuncs=0 rules=0 queues=0 topics=0 tables=0 " +
		"buckets=0 streams=0 filesystems=0 secrets=0 parameters=0 edges=1 unsupported=1"
	if summary != want {
		t.Errorf("Summary() = %q, want %q", summary, want)
	}
}
