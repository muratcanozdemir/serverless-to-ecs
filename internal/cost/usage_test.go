package cost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"serverless-to-ecs/internal/model"
)

func TestRateToMonthly(t *testing.T) {
	tests := []struct {
		expr string
		want int
	}{
		{"rate(1 minute)", 30 * 24 * 60},
		{"rate(5 minutes)", 30 * 24 * 60 / 5},
		{"rate(1 hour)", 30 * 24},
		{"rate(4 hours)", 30 * 24 / 4},
		{"rate(1 day)", 30},
		{"rate(2 days)", 15},
		{"rate(bogus)", 10_000}, // doesn't match the regex — fallback
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			got := rateToMonthly(tt.expr)
			if got != tt.want {
				t.Errorf("rateToMonthly(%q) = %d, want %d", tt.expr, got, tt.want)
			}
		})
	}
}

func TestCronToMonthly(t *testing.T) {
	tests := []struct {
		name string
		expr string
		want int
	}{
		{"every 15 minutes", "cron(*/15 * * * ? *)", 30 * 24 * 60 / 15},
		{"daily at specific time", "cron(0 6 * * ? *)", 30},
		{"hourly at specific minute", "cron(0 * * * ? *)", 30 * 24},
		{"too few fields falls back", "cron(bad)", 10_000},
		{"wildcard minute and hour falls back to daily", "cron(* * * * ? *)", 30},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cronToMonthly(tt.expr)
			if got != tt.want {
				t.Errorf("cronToMonthly(%q) = %d, want %d", tt.expr, got, tt.want)
			}
		})
	}
}

func TestScheduleToMonthly(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		want     int
	}{
		{"empty schedule (event pattern rule)", "", 10_000},
		{"rate expression", "rate(1 hour)", 30 * 24},
		{"cron expression", "cron(0 6 * * ? *)", 30},
		{"unrecognized format falls back", "at(2024-01-01T00:00:00Z)", 10_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scheduleToMonthly(tt.schedule)
			if got != tt.want {
				t.Errorf("scheduleToMonthly(%q) = %d, want %d", tt.schedule, got, tt.want)
			}
		})
	}
}

func TestBaseAPIRequests(t *testing.T) {
	tests := []struct {
		name       string
		api        *model.APIGateway
		routeCount int
		want       int
	}{
		{"low-traffic REST", &model.APIGateway{Protocol: "REST"}, 2, 100_000},
		{"high-traffic REST (many routes)", &model.APIGateway{Protocol: "REST"}, 4, 500_000},
		{"low-traffic HTTP doubles base", &model.APIGateway{Protocol: "HTTP"}, 2, 200_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := baseAPIRequests(tt.api, tt.routeCount)
			if got != tt.want {
				t.Errorf("baseAPIRequests(%+v, %d) = %d, want %d", tt.api, tt.routeCount, got, tt.want)
			}
		})
	}
}

func TestBaseSQSMessages(t *testing.T) {
	if got := baseSQSMessages(&model.SQSQueue{FIFOQueue: false}); got != 100_000 {
		t.Errorf("standard queue: got %d, want 100000", got)
	}
	if got := baseSQSMessages(&model.SQSQueue{FIFOQueue: true}); got != 50_000 {
		t.Errorf("FIFO queue: got %d, want 50000", got)
	}
}

func TestDefaultDynamoUsage(t *testing.T) {
	t.Run("provisioned tables get zero usage (cost comes from RCU/WCU)", func(t *testing.T) {
		u := defaultDynamoUsage(&model.DynamoDBTable{BillingMode: "PROVISIONED"})
		if u.MonthlyReadUnits != 0 || u.MonthlyWriteUnits != 0 {
			t.Errorf("expected zero usage for provisioned table, got %+v", u)
		}
	})
	t.Run("on-demand tables get moderate default usage", func(t *testing.T) {
		u := defaultDynamoUsage(&model.DynamoDBTable{BillingMode: "PAY_PER_REQUEST"})
		if u.MonthlyReadUnits == 0 || u.MonthlyWriteUnits == 0 {
			t.Errorf("expected non-zero default usage for on-demand table, got %+v", u)
		}
	})
}

func TestEstimateDuration(t *testing.T) {
	tests := []struct {
		name string
		fn   *model.Lambda
		want float64
	}{
		{"low memory, ample timeout", &model.Lambda{MemoryMB: 256, TimeoutSec: 30}, 200.0},
		{"high memory, ample timeout", &model.Lambda{MemoryMB: 2048, TimeoutSec: 30}, 500.0},
		{"high memory but tight timeout caps the estimate", &model.Lambda{MemoryMB: 2048, TimeoutSec: 3}, 300.0}, // 10% of 3000ms
		{"tight timeout below base caps at 10% of timeout", &model.Lambda{MemoryMB: 128, TimeoutSec: 1}, 100.0},  // 10% of 1000ms = 100, below 200 base
		{"zero timeout floors at 50ms", &model.Lambda{MemoryMB: 128, TimeoutSec: 0}, 50.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateDuration(tt.fn)
			if got != tt.want {
				t.Errorf("estimateDuration(%+v) = %v, want %v", tt.fn, got, tt.want)
			}
		})
	}
}

func TestDefaultProfile(t *testing.T) {
	g := model.NewGraph()
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256, TimeoutSec: 10}
	g.Queues["Q"] = &model.SQSQueue{LogicalID: "Q", QueueName: "q"}
	g.AddEdge("Q", "Fn", model.EdgeTriggers, "SQS event source")

	p := DefaultProfile(g)

	fnUsage, ok := p.Lambdas["Fn"]
	if !ok {
		t.Fatal("expected usage entry for Fn")
	}
	if fnUsage.Source != "derived" {
		t.Errorf("expected Fn usage to be derived from its SQS trigger, got source=%q", fnUsage.Source)
	}
	if fnUsage.MonthlyInvocations != p.Queues["Q"].MonthlyMessages {
		t.Errorf("expected Fn invocations (%d) to match queue messages (%d)",
			fnUsage.MonthlyInvocations, p.Queues["Q"].MonthlyMessages)
	}
}

func TestDefaultProfile_NewResourceTypes(t *testing.T) {
	g := model.NewGraph()
	g.Buckets["Bucket"] = &model.S3Bucket{LogicalID: "Bucket", BucketName: "bucket"}
	g.Streams["Stream1Shard"] = &model.KinesisStream{LogicalID: "Stream1Shard", ShardCount: 1}
	g.Streams["Stream3Shard"] = &model.KinesisStream{LogicalID: "Stream3Shard", ShardCount: 3}
	g.FileSystems["FS"] = &model.EFSFileSystem{LogicalID: "FS"}
	g.Secrets["Secret"] = &model.SecretsManagerSecret{LogicalID: "Secret", Name: "secret"}

	p := DefaultProfile(g)

	if p.Buckets["Bucket"] == nil || p.Buckets["Bucket"].StorageGB <= 0 {
		t.Error("expected a default S3 usage entry with positive storage")
	}
	if p.Streams["Stream1Shard"].MonthlyPutRecords != 1_000_000 {
		t.Errorf("1-shard stream: MonthlyPutRecords = %d, want 1000000", p.Streams["Stream1Shard"].MonthlyPutRecords)
	}
	if p.Streams["Stream3Shard"].MonthlyPutRecords != 3_000_000 {
		t.Errorf("3-shard stream: MonthlyPutRecords = %d, want 3000000 (scaled by shard count)", p.Streams["Stream3Shard"].MonthlyPutRecords)
	}
	if p.FileSystems["FS"] == nil || p.FileSystems["FS"].StorageGB <= 0 {
		t.Error("expected a default EFS usage entry with positive storage")
	}
	if p.Secrets["Secret"] == nil || p.Secrets["Secret"].MonthlyAPICalls <= 0 {
		t.Error("expected a default Secrets Manager usage entry with positive API calls")
	}
}

func TestLoadSidecar_NewResourceTypes(t *testing.T) {
	g := model.NewGraph()
	g.Buckets["Bucket"] = &model.S3Bucket{LogicalID: "Bucket"}
	g.Streams["Stream"] = &model.KinesisStream{LogicalID: "Stream", ShardCount: 1}
	g.FileSystems["FS"] = &model.EFSFileSystem{LogicalID: "FS"}
	g.Secrets["Secret"] = &model.SecretsManagerSecret{LogicalID: "Secret"}
	p := DefaultProfile(g)

	sidecar := `{
		"s3_buckets": {"Bucket": {"storage_gb": 500, "monthly_puts": 1, "monthly_gets": 1}},
		"kinesis_streams": {"Stream": {"monthly_put_records": 999999}},
		"efs_filesystems": {"FS": {"storage_gb": 250}},
		"secrets": {"Secret": {"monthly_api_calls": 55555}}
	}`
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(path, []byte(sidecar), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if err := p.LoadSidecar(path); err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}

	if p.Buckets["Bucket"].StorageGB != 500 || p.Buckets["Bucket"].Source != "sidecar" {
		t.Errorf("Bucket usage not overridden: %+v", p.Buckets["Bucket"])
	}
	if p.Streams["Stream"].MonthlyPutRecords != 999999 || p.Streams["Stream"].Source != "sidecar" {
		t.Errorf("Stream usage not overridden: %+v", p.Streams["Stream"])
	}
	if p.FileSystems["FS"].StorageGB != 250 || p.FileSystems["FS"].Source != "sidecar" {
		t.Errorf("FileSystem usage not overridden: %+v", p.FileSystems["FS"])
	}
	if p.Secrets["Secret"].MonthlyAPICalls != 55555 || p.Secrets["Secret"].Source != "sidecar" {
		t.Errorf("Secret usage not overridden: %+v", p.Secrets["Secret"])
	}
}

func TestDefaultProfile_UntriggeredLambdaFallsBackToDefault(t *testing.T) {
	g := model.NewGraph()
	g.Lambdas["Orphan"] = &model.Lambda{LogicalID: "Orphan", FunctionName: "orphan", MemoryMB: 128, TimeoutSec: 5}

	p := DefaultProfile(g)

	u := p.Lambdas["Orphan"]
	if u.Source != "default" || u.MonthlyInvocations != 10_000 {
		t.Errorf("expected fallback default usage for untriggered lambda, got %+v", u)
	}
}

func TestLoadSidecar(t *testing.T) {
	g := model.NewGraph()
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256, TimeoutSec: 10}
	p := DefaultProfile(g)
	originalDuration := p.Lambdas["Fn"].AvgDurationMs

	sidecar := map[string]interface{}{
		"lambdas": map[string]interface{}{
			"Fn": map[string]interface{}{
				"monthly_invocations": 999_999,
				// avg_duration_ms intentionally omitted (zero value) — should not override.
			},
		},
	}
	data, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	if err := p.LoadSidecar(path); err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}

	if p.Lambdas["Fn"].MonthlyInvocations != 999_999 {
		t.Errorf("expected sidecar to override invocations, got %d", p.Lambdas["Fn"].MonthlyInvocations)
	}
	if p.Lambdas["Fn"].Source != "sidecar" {
		t.Errorf("expected source=sidecar after override, got %q", p.Lambdas["Fn"].Source)
	}
	if p.Lambdas["Fn"].AvgDurationMs != originalDuration {
		t.Errorf("expected zero-value avg_duration_ms in sidecar to leave default (%v) untouched, got %v",
			originalDuration, p.Lambdas["Fn"].AvgDurationMs)
	}
}

func TestLoadSidecar_UnknownLambdaIgnored(t *testing.T) {
	g := model.NewGraph()
	p := DefaultProfile(g)

	sidecar := `{"lambdas": {"DoesNotExist": {"monthly_invocations": 500}}}`
	path := filepath.Join(t.TempDir(), "usage.json")
	if err := os.WriteFile(path, []byte(sidecar), 0644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	if err := p.LoadSidecar(path); err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}
	if _, ok := p.Lambdas["DoesNotExist"]; ok {
		t.Error("expected LoadSidecar not to create entries for lambdas absent from the graph")
	}
}

func TestLoadSidecar_MissingFile(t *testing.T) {
	p := &UsageProfile{Lambdas: map[string]*LambdaUsage{}}
	if err := p.LoadSidecar(filepath.Join(t.TempDir(), "does-not-exist.json")); err == nil {
		t.Error("expected error for missing sidecar file, got nil")
	}
}
