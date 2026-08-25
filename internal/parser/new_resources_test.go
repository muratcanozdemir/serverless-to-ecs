package parser

import (
	"testing"

	"serverless-to-ecs/internal/model"
)

func hasEdge(g *model.Graph, from, to string, et model.EdgeType) bool {
	for _, e := range g.Edges {
		if e.From == from && e.To == to && e.Type == et {
			return true
		}
	}
	return false
}

// --- S3 ---

func TestParseS3_NativeBucketNotification(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {"Type": "AWS::Lambda::Function", "Properties": {"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main"}},
			"MyBucket": {
				"Type": "AWS::S3::Bucket",
				"Properties": {
					"BucketName": "my-bucket",
					"NotificationConfiguration": {
						"LambdaConfigurations": [
							{"Function": {"Fn::GetAtt": ["MyFunc", "Arn"]}, "Event": "s3:ObjectCreated:*"}
						]
					}
				}
			}
		}
	}`)

	b, ok := g.Buckets["MyBucket"]
	if !ok {
		t.Fatal("missing Bucket: MyBucket")
	}
	if b.BucketName != "my-bucket" {
		t.Errorf("BucketName = %q, want my-bucket", b.BucketName)
	}
	if !hasEdge(g, "MyBucket", "MyFunc", model.EdgeTriggers) {
		t.Error("expected a triggers edge from MyBucket to MyFunc")
	}
}

func TestParseS3_SAMEvent(t *testing.T) {
	g := parseJSON(t, `{
		"Transform": "AWS::Serverless-2016-10-31",
		"Resources": {
			"MyBucket": {"Type": "AWS::S3::Bucket", "Properties": {"BucketName": "my-bucket"}},
			"MyFunc": {
				"Type": "AWS::Serverless::Function",
				"Properties": {
					"Handler": "main",
					"Events": {
						"BucketEvent": {"Type": "S3", "Properties": {"Bucket": {"Ref": "MyBucket"}, "Events": "s3:ObjectCreated:*"}}
					}
				}
			}
		}
	}`)

	if !hasEdge(g, "MyBucket", "MyFunc", model.EdgeTriggers) {
		t.Error("expected a triggers edge from MyBucket to MyFunc via SAM S3 event")
	}
}

// --- Kinesis ---

func TestParseKinesis_EventSourceMapping(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {"Type": "AWS::Lambda::Function", "Properties": {"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main"}},
			"MyStream": {"Type": "AWS::Kinesis::Stream", "Properties": {"Name": "my-stream", "ShardCount": 2}},
			"MyMapping": {
				"Type": "AWS::Lambda::EventSourceMapping",
				"Properties": {
					"FunctionName": {"Ref": "MyFunc"},
					"EventSourceArn": {"Fn::GetAtt": ["MyStream", "Arn"]},
					"StartingPosition": "LATEST"
				}
			}
		}
	}`)

	stream, ok := g.Streams["MyStream"]
	if !ok {
		t.Fatal("missing Stream: MyStream")
	}
	if stream.ShardCount != 2 {
		t.Errorf("ShardCount = %d, want 2", stream.ShardCount)
	}
	if !hasEdge(g, "MyStream", "MyFunc", model.EdgeTriggers) {
		t.Error("expected a triggers edge from MyStream to MyFunc")
	}
}

func TestParseKinesis_SAMEvent(t *testing.T) {
	g := parseJSON(t, `{
		"Transform": "AWS::Serverless-2016-10-31",
		"Resources": {
			"MyStream": {"Type": "AWS::Kinesis::Stream", "Properties": {"Name": "my-stream"}},
			"MyFunc": {
				"Type": "AWS::Serverless::Function",
				"Properties": {
					"Handler": "main",
					"Events": {
						"StreamEvent": {"Type": "Kinesis", "Properties": {"Stream": {"Fn::GetAtt": ["MyStream", "Arn"]}, "StartingPosition": "LATEST"}}
					}
				}
			}
		}
	}`)

	if !hasEdge(g, "MyStream", "MyFunc", model.EdgeTriggers) {
		t.Error("expected a triggers edge from MyStream to MyFunc via SAM Kinesis event")
	}
}

// --- VPC config ---

func TestParseLambda_VPCConfig(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {
				"Type": "AWS::Lambda::Function",
				"Properties": {
					"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main",
					"VpcConfig": {
						"SubnetIds": ["subnet-abc", {"Ref": "MySubnetParam"}],
						"SecurityGroupIds": ["sg-123"]
					}
				}
			}
		}
	}`)

	fn := g.Lambdas["MyFunc"]
	if fn.VPCConfig == nil {
		t.Fatal("expected VPCConfig to be set")
	}
	if len(fn.VPCConfig.SubnetRefs) != 2 || fn.VPCConfig.SubnetRefs[0] != "subnet-abc" {
		t.Errorf("SubnetRefs = %v, want [subnet-abc ${MySubnetParam}]", fn.VPCConfig.SubnetRefs)
	}
	if len(fn.VPCConfig.SecurityGroupRefs) != 1 || fn.VPCConfig.SecurityGroupRefs[0] != "sg-123" {
		t.Errorf("SecurityGroupRefs = %v, want [sg-123]", fn.VPCConfig.SecurityGroupRefs)
	}
}

func TestParseLambda_NoVPCConfigLeavesNil(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {"Type": "AWS::Lambda::Function", "Properties": {"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main"}}
		}
	}`)
	if g.Lambdas["MyFunc"].VPCConfig != nil {
		t.Error("expected nil VPCConfig for a function with no VpcConfig property")
	}
}

// --- EFS ---

func TestParseEFS_MountAndAccessPoint(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFileSystem": {"Type": "AWS::EFS::FileSystem", "Properties": {}},
			"MyAccessPoint": {"Type": "AWS::EFS::AccessPoint", "Properties": {"FileSystemId": {"Ref": "MyFileSystem"}}},
			"MyFunc": {
				"Type": "AWS::Lambda::Function",
				"Properties": {
					"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main",
					"FileSystemConfigs": [
						{"Arn": {"Fn::GetAtt": ["MyAccessPoint", "Arn"]}, "LocalMountPath": "/mnt/data"}
					]
				}
			}
		}
	}`)

	if _, ok := g.FileSystems["MyFileSystem"]; !ok {
		t.Fatal("missing FileSystem: MyFileSystem")
	}
	ap, ok := g.AccessPoints["MyAccessPoint"]
	if !ok {
		t.Fatal("missing AccessPoint: MyAccessPoint")
	}
	if ap.FileSystemRef != "MyFileSystem" {
		t.Errorf("AccessPoint.FileSystemRef = %q, want MyFileSystem", ap.FileSystemRef)
	}

	fn := g.Lambdas["MyFunc"]
	if len(fn.EFSMounts) != 1 {
		t.Fatalf("expected 1 EFS mount, got %d", len(fn.EFSMounts))
	}
	if fn.EFSMounts[0].AccessPointRef != "MyAccessPoint" || fn.EFSMounts[0].LocalMountPath != "/mnt/data" {
		t.Errorf("EFSMounts[0] = %+v, want {AccessPointRef:MyAccessPoint LocalMountPath:/mnt/data}", fn.EFSMounts[0])
	}
	if !hasEdge(g, "MyFunc", "MyAccessPoint", model.EdgeMounts) {
		t.Error("expected a mounts edge from MyFunc to MyAccessPoint")
	}
}

// --- Secrets Manager / SSM ---

func TestParseSecrets_RefToTemplateResource(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MySecret": {"Type": "AWS::SecretsManager::Secret", "Properties": {"Name": "my-secret"}},
			"MyParam": {"Type": "AWS::SSM::Parameter", "Properties": {"Name": "/my/param", "Type": "String"}},
			"MyFunc": {
				"Type": "AWS::Lambda::Function",
				"Properties": {
					"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main",
					"Environment": {"Variables": {
						"DB_PASSWORD": {"Ref": "MySecret"},
						"CONFIG_VALUE": {"Ref": "MyParam"}
					}}
				}
			}
		}
	}`)

	secret, ok := g.Secrets["MySecret"]
	if !ok || secret.Name != "my-secret" {
		t.Fatalf("expected Secret MySecret with name my-secret, got %+v (ok=%v)", secret, ok)
	}
	param, ok := g.Parameters["MyParam"]
	if !ok || param.Name != "/my/param" {
		t.Fatalf("expected Parameter MyParam with name /my/param, got %+v (ok=%v)", param, ok)
	}

	fn := g.Lambdas["MyFunc"]
	ref, ok := fn.SecretRefs["DB_PASSWORD"]
	if !ok || ref.Kind != "secretsmanager" || ref.LogicalID != "MySecret" {
		t.Errorf("SecretRefs[DB_PASSWORD] = %+v (ok=%v), want {Kind:secretsmanager LogicalID:MySecret}", ref, ok)
	}
	pref, ok := fn.SecretRefs["CONFIG_VALUE"]
	if !ok || pref.Kind != "ssm" || pref.LogicalID != "MyParam" {
		t.Errorf("SecretRefs[CONFIG_VALUE] = %+v (ok=%v), want {Kind:ssm LogicalID:MyParam}", pref, ok)
	}

	if !hasEdge(g, "MyFunc", "MySecret", model.EdgeReadsSecret) {
		t.Error("expected a reads_secret edge from MyFunc to MySecret")
	}
	if !hasEdge(g, "MyFunc", "MyParam", model.EdgeReadsSecret) {
		t.Error("expected a reads_secret edge from MyFunc to MyParam")
	}
}

func TestParseSecrets_DynamicReferenceSyntax(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {
				"Type": "AWS::Lambda::Function",
				"Properties": {
					"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main",
					"Environment": {"Variables": {
						"DB_PASSWORD": "{{resolve:secretsmanager:my-secret:SecretString:password}}",
						"API_KEY": "{{resolve:ssm:/my/api-key}}",
						"PLAIN": "not-a-secret"
					}}
				}
			}
		}
	}`)

	fn := g.Lambdas["MyFunc"]
	ref, ok := fn.SecretRefs["DB_PASSWORD"]
	if !ok || ref.Kind != "secretsmanager" || ref.LogicalID != "" {
		t.Errorf("SecretRefs[DB_PASSWORD] = %+v (ok=%v), want Kind=secretsmanager, no LogicalID (not a template resource)", ref, ok)
	}
	aref, ok := fn.SecretRefs["API_KEY"]
	if !ok || aref.Kind != "ssm" {
		t.Errorf("SecretRefs[API_KEY] = %+v (ok=%v), want Kind=ssm", aref, ok)
	}
	if _, ok := fn.SecretRefs["PLAIN"]; ok {
		t.Error("did not expect a SecretRef for a plain (non-dynamic-reference) value")
	}
}
