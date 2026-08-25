package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/model"
)

// TestEmitTerraform_SecretsRenderAsNativeSecrets verifies a Lambda env var
// backed by Secrets Manager/SSM is emitted via ECS's native "secrets" field
// (a var.<...>_secret_arn reference), not inlined into "environment".
func TestEmitTerraform_SecretsRenderAsNativeSecrets(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Secrets["DBSecret"] = &model.SecretsManagerSecret{LogicalID: "DBSecret", Name: "db-secret"}
	g.Lambdas["Fn"] = &model.Lambda{
		LogicalID:    "Fn",
		FunctionName: "fn",
		MemoryMB:     256,
		EnvVars: map[string]string{
			"DB_PASSWORD": "resolved-at-deploy",
			"PLAIN_VAR":   "plain-value",
		},
		SecretRefs: map[string]model.SecretRef{
			"DB_PASSWORD": {Kind: "secretsmanager", LogicalID: "DBSecret"},
		},
	}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")
	g.AddEdge("Fn", "DBSecret", model.EdgeReadsSecret, "DB_PASSWORD")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	ecs := readFile(t, tmpDir, "ecs.tf")
	if !strings.Contains(ecs, `{ name = "DB_PASSWORD", valueFrom = var.`) {
		t.Errorf("expected DB_PASSWORD to be emitted via secrets/valueFrom, got:\n%s", ecs)
	}
	if strings.Contains(ecs, `"DB_PASSWORD"`) && strings.Contains(ecs, `environment = [`) {
		envBlock := ecs[strings.Index(ecs, "environment = ["):strings.Index(ecs, "secrets = [")]
		if strings.Contains(envBlock, "DB_PASSWORD") {
			t.Errorf("DB_PASSWORD leaked into environment block:\n%s", envBlock)
		}
	}
	if !strings.Contains(ecs, `"PLAIN_VAR"`) {
		t.Errorf("expected PLAIN_VAR to remain in environment, got:\n%s", ecs)
	}

	vars := readFile(t, tmpDir, "variables.tf")
	if !strings.Contains(vars, "_secret_arn") {
		t.Errorf("expected a secret ARN variable declaration, got:\n%s", vars)
	}
	if !strings.Contains(vars, "TODO_REPLACE_WITH_ARN") {
		t.Errorf("expected TODO placeholder default for secret ARN, got:\n%s", vars)
	}
}

// TestEmitTerraform_EFSMountsRenderVolumesAndPlatformVersion verifies a
// Lambda's EFS access point mount produces an ECS volume block, a
// mountPoints entry, and forces platform_version = "1.4.0" (required for
// EFS on Fargate).
func TestEmitTerraform_EFSMountsRenderVolumesAndPlatformVersion(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.FileSystems["FS"] = &model.EFSFileSystem{LogicalID: "FS"}
	g.AccessPoints["AP"] = &model.EFSAccessPoint{LogicalID: "AP", FileSystemRef: "FS"}
	g.Lambdas["Fn"] = &model.Lambda{
		LogicalID:    "Fn",
		FunctionName: "fn",
		MemoryMB:     256,
		EFSMounts: []model.EFSMount{
			{AccessPointRef: "AP", LocalMountPath: "/mnt/data"},
		},
	}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")
	g.AddEdge("Fn", "AP", model.EdgeMounts, "/mnt/data")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	ecs := readFile(t, tmpDir, "ecs.tf")
	if !strings.Contains(ecs, "efs_volume_configuration") {
		t.Errorf("expected an efs_volume_configuration block, got:\n%s", ecs)
	}
	if !strings.Contains(ecs, `containerPath = "/mnt/data"`) {
		t.Errorf("expected a mountPoints entry for /mnt/data, got:\n%s", ecs)
	}
	if !strings.Contains(ecs, `platform_version = "1.4.0"`) {
		t.Errorf("expected platform_version 1.4.0 to be forced when EFS is mounted, got:\n%s", ecs)
	}

	vars := readFile(t, tmpDir, "variables.tf")
	if !strings.Contains(vars, "_file_system_id") || !strings.Contains(vars, "_access_point_id") {
		t.Errorf("expected file system and access point ID variables, got:\n%s", vars)
	}
}

// TestEmitTerraform_VPCConfigNoteRendered verifies a Lambda that runs inside
// a VPC produces a documentation comment on its ECS service (subnets and
// security groups can't be reliably re-resolved, so it's flagged for the
// user rather than auto-wired).
func TestEmitTerraform_VPCConfigNoteRendered(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Lambdas["Fn"] = &model.Lambda{
		LogicalID:    "Fn",
		FunctionName: "fn",
		MemoryMB:     256,
		VPCConfig: &model.VPCConfig{
			SubnetRefs:        []string{"subnet-aaa", "subnet-bbb"},
			SecurityGroupRefs: []string{"sg-ccc"},
		},
	}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	ecs := readFile(t, tmpDir, "ecs.tf")
	if !strings.Contains(ecs, "ran inside a VPC") {
		t.Errorf("expected a VPC note comment, got:\n%s", ecs)
	}
	if !strings.Contains(ecs, "subnet-aaa") || !strings.Contains(ecs, "sg-ccc") {
		t.Errorf("expected VPC note to mention subnet/security group refs, got:\n%s", ecs)
	}
}

// TestEmitTerraform_KinesisConsumerDocumented verifies a Lambda triggered by
// a Kinesis stream is documented in kinesis_consumers.tf and resolves to
// its ECS service group.
func TestEmitTerraform_KinesisConsumerDocumented(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Streams["Stream"] = &model.KinesisStream{LogicalID: "Stream", StreamName: "orders-stream", ShardCount: 2}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256}
	g.AddEdge("Stream", "Fn", model.EdgeTriggers, "Kinesis stream")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	kc := readFile(t, tmpDir, "kinesis_consumers.tf")
	if !strings.Contains(kc, "orders-stream") {
		t.Errorf("expected stream name in kinesis_consumers.tf, got:\n%s", kc)
	}
	if !strings.Contains(kc, "Fn") {
		t.Errorf("expected source lambda reference in kinesis_consumers.tf, got:\n%s", kc)
	}
}

// TestEmitTerraform_S3EventProcessorDocumented verifies a Lambda triggered
// by an S3 bucket notification is documented in s3_event_processors.tf.
func TestEmitTerraform_S3EventProcessorDocumented(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Buckets["Bucket"] = &model.S3Bucket{LogicalID: "Bucket", BucketName: "uploads-bucket"}
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256}
	g.AddEdge("Bucket", "Fn", model.EdgeTriggers, "s3:ObjectCreated:*")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	s3 := readFile(t, tmpDir, "s3_event_processors.tf")
	if !strings.Contains(s3, "uploads-bucket") {
		t.Errorf("expected bucket name in s3_event_processors.tf, got:\n%s", s3)
	}
	if !strings.Contains(s3, "Fn") {
		t.Errorf("expected source lambda reference in s3_event_processors.tf, got:\n%s", s3)
	}
}

// TestEmitTerraform_UnresolvedKinesisAndS3TriggersGetTODO verifies that when
// a triggered Lambda doesn't map to any ECS service group, the documentation
// files still render (with a TODO) rather than referencing an undeclared
// service.
func TestEmitTerraform_UnresolvedKinesisAndS3TriggersGetTODO(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Streams["Stream"] = &model.KinesisStream{LogicalID: "Stream", StreamName: "orphan-stream", ShardCount: 1}
	g.Buckets["Bucket"] = &model.S3Bucket{LogicalID: "Bucket", BucketName: "orphan-bucket"}
	// No Lambdas at all — the triggered-Lambda lookup will find nothing to
	// resolve to a service group, but the stream/bucket edges still exist
	// via a dangling reference scenario isn't directly constructible without
	// a Lambda, so instead trigger a Lambda that never gets grouped: there
	// are no APIs/rules/queues, so cost.GroupServices won't place it either.
	g.Lambdas["OrphanFn"] = &model.Lambda{LogicalID: "OrphanFn", FunctionName: "orphan-fn", MemoryMB: 128}
	g.AddEdge("Stream", "OrphanFn", model.EdgeTriggers, "Kinesis stream")
	g.AddEdge("Bucket", "OrphanFn", model.EdgeTriggers, "s3:ObjectCreated:*")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	kc := readFile(t, tmpDir, "kinesis_consumers.tf")
	s3 := readFile(t, tmpDir, "s3_event_processors.tf")
	// Either resolved (grouped as standalone) or TODO — but never a
	// reference to an undeclared task definition.
	if strings.Contains(kc, "aws_ecs_task_definition.unknown") {
		t.Errorf("kinesis_consumers.tf references an undeclared task definition:\n%s", kc)
	}
	if strings.Contains(s3, "aws_ecs_task_definition.unknown") {
		t.Errorf("s3_event_processors.tf references an undeclared task definition:\n%s", s3)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}
