package cost

import (
	"math"
	"strings"
	"testing"

	"serverless-to-ecs/internal/model"
)

func testPrices(t *testing.T) *RegionalPrices {
	t.Helper()
	db, err := LoadPricing()
	if err != nil {
		t.Fatalf("LoadPricing: %v", err)
	}
	p, err := db.ForRegion("eu-central-1")
	if err != nil {
		t.Fatalf("ForRegion: %v", err)
	}
	return p
}

func TestLambdaCost(t *testing.T) {
	p := testPrices(t)
	fn := &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 1024}
	u := &LambdaUsage{MonthlyInvocations: 1_000_000, AvgDurationMs: 200}

	lc := lambdaCost(fn, u, p)

	wantGBSeconds := 1_000_000.0 * (1024.0 / 1024.0) * (200.0 / 1000.0)
	if lc.GBSeconds != wantGBSeconds {
		t.Errorf("GBSeconds = %v, want %v", lc.GBSeconds, wantGBSeconds)
	}
	wantRequestCost := 1_000_000.0 / 1_000_000.0 * p.Lambda.RequestPerMillion
	if lc.RequestCost != wantRequestCost {
		t.Errorf("RequestCost = %v, want %v", lc.RequestCost, wantRequestCost)
	}
	wantComputeCost := wantGBSeconds * p.Lambda.GBSecondX86
	if lc.ComputeCost != wantComputeCost {
		t.Errorf("ComputeCost = %v, want %v", lc.ComputeCost, wantComputeCost)
	}
	if lc.Total != lc.RequestCost+lc.ComputeCost {
		t.Errorf("Total = %v, want RequestCost+ComputeCost = %v", lc.Total, lc.RequestCost+lc.ComputeCost)
	}
}

func TestFargateTaskSize(t *testing.T) {
	tests := []struct {
		lambdaMB   int
		wantVCPU   float64
		wantMemory float64
	}{
		{128, 0.25, 0.5},
		{512, 0.25, 0.5},
		{513, 0.5, 1.0},
		{1024, 0.5, 1.0},
		{2048, 1.0, 2.0},
		{4096, 2.0, 4.0},
		{4097, 4.0, 8.0},
		{10240, 4.0, 8.0},
	}
	for _, tt := range tests {
		vcpu, mem := fargateTaskSize(tt.lambdaMB)
		if vcpu != tt.wantVCPU || mem != tt.wantMemory {
			t.Errorf("fargateTaskSize(%d) = (%v, %v), want (%v, %v)",
				tt.lambdaMB, vcpu, mem, tt.wantVCPU, tt.wantMemory)
		}
	}
}

func TestProjectECSService_TaskScaling(t *testing.T) {
	p := testPrices(t)
	g := model.NewGraph()
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256}

	tests := []struct {
		name        string
		groupType   string
		invocations int
		wantTasks   int
	}{
		{"api service, low traffic", "api", 1_000, 2},
		{"queue processor, low traffic", "queue-processor", 1_000, 1},
		{"scheduled, low traffic", "scheduled", 1_000, 1},
		{"api service, high traffic scales to 3", "api", 2_000_000, 3},
		{"api service, very high traffic scales to 5", "api", 6_000_000, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usage := &UsageProfile{Lambdas: map[string]*LambdaUsage{
				"Fn": {MonthlyInvocations: tt.invocations},
			}}
			group := ServiceGroup{Name: "svc", Type: tt.groupType, LambdaIDs: []string{"Fn"}}
			got := projectECSService(g, usage, group, p)
			if got.Tasks != tt.wantTasks {
				t.Errorf("Tasks = %d, want %d", got.Tasks, tt.wantTasks)
			}
			if got.Total != got.VCPUCost+got.MemoryCost {
				t.Errorf("Total = %v, want VCPUCost+MemoryCost = %v", got.Total, got.VCPUCost+got.MemoryCost)
			}
		})
	}
}

func TestProjectECSService_SizesByMaxGroupMemberMemory(t *testing.T) {
	p := testPrices(t)
	g := model.NewGraph()
	g.Lambdas["Small"] = &model.Lambda{LogicalID: "Small", MemoryMB: 128}
	g.Lambdas["Big"] = &model.Lambda{LogicalID: "Big", MemoryMB: 3000}
	usage := &UsageProfile{Lambdas: map[string]*LambdaUsage{}}
	group := ServiceGroup{Name: "svc", Type: "api", LambdaIDs: []string{"Small", "Big"}}

	got := projectECSService(g, usage, group, p)

	wantVCPU, wantMem := fargateTaskSize(3000)
	if got.VCPUs != wantVCPU || got.MemoryGB != wantMem {
		t.Errorf("expected sizing driven by the largest member (3000MB): got (%v, %v), want (%v, %v)",
			got.VCPUs, got.MemoryGB, wantVCPU, wantMem)
	}
}

func TestEstimateLCUs_FloorsAtOne(t *testing.T) {
	g := model.NewGraph()
	usage := &UsageProfile{APIs: map[string]*APIUsage{}}
	got := estimateLCUs(g, usage)
	if got != 1.0 {
		t.Errorf("expected LCU floor of 1.0 with no API traffic, got %v", got)
	}
}

func TestEstimateLCUs_ScalesWithTraffic(t *testing.T) {
	g := model.NewGraph()
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api"}
	usage := &UsageProfile{APIs: map[string]*APIUsage{
		"Api": {MonthlyRequests: 100_000_000},
	}}
	got := estimateLCUs(g, usage)
	rps := 100_000_000.0 / (30.0 * 24.0 * 3600.0)
	want := math.Max(rps/25.0, 1.0)
	if got != want {
		t.Errorf("estimateLCUs = %v, want %v", got, want)
	}
	if got <= 1.0 {
		t.Error("expected high traffic to produce more than the 1.0 floor")
	}
}

func TestClassifyAccuracy(t *testing.T) {
	tests := []struct {
		name    string
		usage   *UsageProfile
		wantHas string // substring expected in the accuracy string
	}{
		{"no lambdas at all", &UsageProfile{Lambdas: map[string]*LambdaUsage{}}, "heuristic"},
		{"all default", &UsageProfile{Lambdas: map[string]*LambdaUsage{
			"A": {Source: "default"}, "B": {Source: "default"},
		}}, "heuristic"},
		{"majority sidecar", &UsageProfile{Lambdas: map[string]*LambdaUsage{
			"A": {Source: "sidecar"}, "B": {Source: "sidecar"}, "C": {Source: "default"},
		}}, "sidecar data for >50%"},
		{"minority sidecar", &UsageProfile{Lambdas: map[string]*LambdaUsage{
			"A": {Source: "sidecar"}, "B": {Source: "default"}, "C": {Source: "default"},
		}}, "partial sidecar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyAccuracy(tt.usage)
			if !strings.Contains(got, tt.wantHas) {
				t.Errorf("classifyAccuracy() = %q, want substring %q", got, tt.wantHas)
			}
		})
	}
}

func TestFormatInt(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{500, "500"},
		{1_500, "1.5K"},
		{2_500_000, "2.5M"},
	}
	for _, tt := range tests {
		if got := formatInt(tt.n); got != tt.want {
			t.Errorf("formatInt(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestEstimateCosts_EndToEnd(t *testing.T) {
	p := testPrices(t)
	g := model.NewGraph()
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")

	usage := DefaultProfile(g)
	est := EstimateCosts(g, usage, p, "eu-central-1")

	if est.Region != "eu-central-1" {
		t.Errorf("Region = %q, want eu-central-1", est.Region)
	}
	if est.Serverless.Total <= 0 {
		t.Error("expected positive serverless total")
	}
	if est.ECS.Total <= 0 {
		t.Error("expected positive ECS total")
	}
	if len(est.Serverless.Lambda) != 1 {
		t.Errorf("expected 1 Lambda cost line item, got %d", len(est.Serverless.Lambda))
	}
	if len(est.ECS.Services) != 1 {
		t.Errorf("expected 1 ECS service (one API-backed group), got %d", len(est.ECS.Services))
	}

	wantDelta := est.Serverless.Total - est.ECS.Total
	if est.Savings.DeltaAbsolute != wantDelta {
		t.Errorf("Savings.DeltaAbsolute = %v, want %v", est.Savings.DeltaAbsolute, wantDelta)
	}
	wantPct := wantDelta / est.Serverless.Total * 100.0
	if est.Savings.DeltaPercent != wantPct {
		t.Errorf("Savings.DeltaPercent = %v, want %v", est.Savings.DeltaPercent, wantPct)
	}
}

func TestS3Cost(t *testing.T) {
	p := testPrices(t)
	if got := s3Cost(nil, p); got != 0 {
		t.Errorf("s3Cost(nil, ...) = %v, want 0", got)
	}
	u := &S3Usage{StorageGB: 100, MonthlyPuts: 10_000, MonthlyGets: 50_000}
	got := s3Cost(u, p)
	want := 100*p.S3.StandardStoragePerGBMonth +
		10_000.0/1000.0*p.S3.PutRequestsPerThousand +
		50_000.0/1000.0*p.S3.GetRequestsPerThousand
	if got != want {
		t.Errorf("s3Cost = %v, want %v", got, want)
	}
}

func TestKinesisCost(t *testing.T) {
	p := testPrices(t)
	if got := kinesisCost(&model.KinesisStream{ShardCount: 2}, nil, p); got != 0 {
		t.Errorf("kinesisCost(..., nil, ...) = %v, want 0", got)
	}
	stream := &model.KinesisStream{ShardCount: 2}
	u := &KinesisUsage{MonthlyPutRecords: 2_000_000}
	got := kinesisCost(stream, u, p)
	want := 2*p.Kinesis.ShardPerHour*730.0 + 2_000_000.0/1_000_000.0*p.Kinesis.PutPayloadUnitPerMillion
	if got != want {
		t.Errorf("kinesisCost = %v, want %v", got, want)
	}
}

func TestKinesisCost_ZeroShardCountTreatedAsOne(t *testing.T) {
	p := testPrices(t)
	stream := &model.KinesisStream{ShardCount: 0}
	u := &KinesisUsage{MonthlyPutRecords: 0}
	got := kinesisCost(stream, u, p)
	want := 1 * p.Kinesis.ShardPerHour * 730.0
	if got != want {
		t.Errorf("kinesisCost with ShardCount=0 = %v, want %v (treated as 1 shard)", got, want)
	}
}

func TestEFSCost(t *testing.T) {
	p := testPrices(t)
	if got := efsCost(nil, p); got != 0 {
		t.Errorf("efsCost(nil, ...) = %v, want 0", got)
	}
	got := efsCost(&EFSUsage{StorageGB: 40}, p)
	want := 40 * p.EFS.StandardStoragePerGBMonth
	if got != want {
		t.Errorf("efsCost = %v, want %v", got, want)
	}
}

func TestSecretCost(t *testing.T) {
	p := testPrices(t)
	if got := secretCost(nil, p); got != 0 {
		t.Errorf("secretCost(nil, ...) = %v, want 0", got)
	}
	got := secretCost(&SecretUsage{MonthlyAPICalls: 20_000}, p)
	want := p.SecretsManager.PerSecretPerMonth + 20_000.0/10_000.0*p.SecretsManager.APICallsPerTenThousand
	if got != want {
		t.Errorf("secretCost = %v, want %v", got, want)
	}
}

func TestEstimateCosts_NewResourceTypesAreRetainedPostMigration(t *testing.T) {
	p := testPrices(t)
	g := model.NewGraph()
	g.Buckets["Bucket"] = &model.S3Bucket{LogicalID: "Bucket", BucketName: "bucket"}
	g.Streams["Stream"] = &model.KinesisStream{LogicalID: "Stream", StreamName: "stream", ShardCount: 1}
	g.FileSystems["FS"] = &model.EFSFileSystem{LogicalID: "FS"}
	g.Secrets["Secret"] = &model.SecretsManagerSecret{LogicalID: "Secret", Name: "secret"}

	usage := DefaultProfile(g)
	est := EstimateCosts(g, usage, p, "eu-central-1")

	if len(est.Serverless.S3) != 1 || est.Serverless.S3[0].Total <= 0 {
		t.Errorf("expected 1 priced S3 line item, got %+v", est.Serverless.S3)
	}
	if len(est.Serverless.Kinesis) != 1 || est.Serverless.Kinesis[0].Total <= 0 {
		t.Errorf("expected 1 priced Kinesis line item, got %+v", est.Serverless.Kinesis)
	}
	if len(est.Serverless.EFS) != 1 || est.Serverless.EFS[0].Total <= 0 {
		t.Errorf("expected 1 priced EFS line item, got %+v", est.Serverless.EFS)
	}
	if len(est.Serverless.SecretsManager) != 1 || est.Serverless.SecretsManager[0].Total <= 0 {
		t.Errorf("expected 1 priced Secrets Manager line item, got %+v", est.Serverless.SecretsManager)
	}

	// Same four resources should carry the identical cost into ECS.Retained,
	// since none of them are replaced by the migration.
	wantRetained := s3Cost(usage.Buckets["Bucket"], p) +
		kinesisCost(g.Streams["Stream"], usage.Streams["Stream"], p) +
		efsCost(usage.FileSystems["FS"], p) +
		secretCost(usage.Secrets["Secret"], p)
	if est.ECS.Retained != wantRetained {
		t.Errorf("ECS.Retained = %v, want %v", est.ECS.Retained, wantRetained)
	}
}

func TestEstimateCosts_ZeroServerlessTotalAvoidsDivideByZero(t *testing.T) {
	p := testPrices(t)
	g := model.NewGraph() // empty graph — nothing to cost
	usage := DefaultProfile(g)

	est := EstimateCosts(g, usage, p, "eu-central-1")

	if est.Serverless.Total != 0 {
		t.Fatalf("expected zero serverless total for an empty graph, got %v", est.Serverless.Total)
	}
	if est.Savings.DeltaPercent != 0 {
		t.Errorf("expected DeltaPercent to stay 0 (not NaN/Inf) when serverless total is 0, got %v", est.Savings.DeltaPercent)
	}
}
