# serverless-to-ecs

[![CI](https://github.com/muratcanozdemir/serverless-to-ecs/actions/workflows/ci.yml/badge.svg)](https://github.com/muratcanozdemir/serverless-to-ecs/actions/workflows/ci.yml)

Reads an AWS serverless stack (CloudFormation / CDK / SAM templates), estimates
costs, and proposes a migration to ECS Fargate — complete with Terraform stubs
in a strangler deployment pattern and a migration report.

## What it does

1. **Parses** CFN/SAM templates (JSON or YAML) into a typed resource graph
   with edges representing inter-service relationships
2. **Estimates** current serverless monthly spend using embedded regional pricing
3. **Projects** ECS Fargate cost after migration
4. **Groups** Lambda functions into logical ECS services by trigger affinity,
   naming patterns, and shared dependencies
5. **Emits** Terraform files implementing the strangler pattern: ALB with
   path-based routing, per-service desired_count toggles, scheduled tasks,
   and queue pollers
6. **Generates** a markdown migration report (LLM-assisted or bare data dump)

## Build

```
go mod tidy
go build -o serverless-to-ecs .
```

To stamp a version (shown by `-version`), set it via `-ldflags` at build time —
this is what the release workflow does for tagged builds:

```
go build -ldflags "-X serverless-to-ecs/cmd.Version=v1.2.3" -o serverless-to-ecs .
```

### Releases

Pushing a tag matching `v*` (e.g. `v1.2.3`) triggers the release workflow,
which cross-compiles binaries for linux/darwin (amd64/arm64) and
windows/amd64, and publishes them to a GitHub Release with `sha256` checksums.

## Usage

```
# Full pipeline with data-only report
./serverless-to-ecs -template examples/synthetic-stack.yaml

# With LLM-generated report (any OpenAI-compatible endpoint)
./serverless-to-ecs \
  -template examples/synthetic-stack.yaml \
  -llm-endpoint http://localhost:8080/v1 \
  -llm-model qwen3-4b

# Override region (default: eu-central-1)
./serverless-to-ecs -template stack.yaml -region us-east-1

# Supply real usage data
./serverless-to-ecs -template stack.yaml -usage usage.json

# JSON dump of the cost estimate
./serverless-to-ecs -template stack.yaml -json
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `-template` | (required) | Path to CFN/SAM template file |
| `-region` | `eu-central-1` | AWS region for pricing lookup |
| `-usage` | | Usage profile sidecar JSON (optional) |
| `-output` | `output` | Directory for generated artifacts |
| `-llm-endpoint` | | OpenAI-compatible API base URL |
| `-llm-model` | | Model name for report generation |
| `-json` | `false` | Dump cost estimate as JSON and exit |
| `-version` | `false` | Print version and exit |

### Output structure

```
output/
├── report.md              # Migration report (LLM or data-only)
└── terraform/
    ├── main.tf            # Provider, IAM, security groups
    ├── variables.tf       # Inputs, container images, migration toggles
    ├── ecs.tf             # Cluster, task definitions, services
    ├── alb.tf             # ALB with strangler routing
    ├── scheduled.tf       # ECS scheduled tasks (replaces EventBridge cron)
    ├── sqs_pollers.tf     # Queue consumer documentation
    ├── kinesis_consumers.tf  # Kinesis stream consumer documentation
    ├── s3_event_processors.tf  # S3 event processor documentation
    └── outputs.tf         # ALB DNS, cluster ARN
```

## Architecture

```
 template.yaml ──→ Parser ──→ Resource Graph ──→ Cost Estimator ──→ Report
                      │              │                  │              │
                      │         (typed edges)      (pricing.json)     │
                      │              │                  │              ▼
                      │              ▼                  │         report.md
                      │        Service Grouper ────────→│
                      │              │                  │
                      │              ▼                  │
                      │      Terraform Emitter ────────→│
                      │              │                  │
                      ▼              ▼                  ▼
                  SAM Globals    output/terraform/    output/report.md
                    merge
```

### Supported resources

| CFN Type | Modeled | Edges detected |
|---|---|---|
| `AWS::Lambda::Function` | ✓ | env var refs → DynamoDB, SQS, SNS |
| `AWS::Serverless::Function` | ✓ | SAM Events (Api, SQS, SNS, Schedule, DynamoDB) |
| `AWS::ApiGateway::RestApi` | ✓ | Method/Integration → Lambda |
| `AWS::ApiGatewayV2::Api` | ✓ | Route → Lambda |
| `AWS::Serverless::Api` / `HttpApi` | ✓ | implicit routing |
| `AWS::StepFunctions::StateMachine` | ✓ | Task state → Lambda (pattern classified) |
| `AWS::Events::Rule` | ✓ | schedule/event-pattern → Lambda |
| `AWS::SQS::Queue` | ✓ | EventSourceMapping → Lambda |
| `AWS::SNS::Topic` | ✓ | Subscription → Lambda/SQS |
| `AWS::DynamoDB::Table` | ✓ | — |
| `AWS::Serverless::SimpleTable` | ✓ | — |
| `AWS::S3::Bucket` | ✓ | NotificationConfiguration → Lambda |
| `AWS::Kinesis::Stream` | ✓ | EventSourceMapping → Lambda |
| `AWS::EFS::FileSystem` / `AWS::EFS::AccessPoint` | ✓ | Lambda FileSystemConfigs → AccessPoint |
| `AWS::SecretsManager::Secret` | ✓ | Lambda env var (Ref/GetAtt or `{{resolve:secretsmanager:...}}`) → Secret |
| `AWS::SSM::Parameter` | ✓ | Lambda env var (Ref/GetAtt or `{{resolve:ssm:...}}`) → Parameter |
| Lambda `VpcConfig` | ✓ | documented on the ECS service, not auto-wired (see below) |

Wiring resources (IAM roles, Lambda permissions, log groups, API Gateway
stages/deployments) are used for edge detection but not modeled as inventory.
Unsupported resource types are captured and flagged in the report.

**Out of scope by design:** API Gateway is left in place rather than migrated
to an ALB/NLB — if Route53 already points at it, that's left alone too. Fronting
ECS with a load balancer means rewriting DNS, which is a deliberate operator
decision this tool doesn't make for you.

**VPC config, EFS, and Secrets Manager/SSM are documented, not auto-wired:**
- A Lambda's `VpcConfig` (subnets/security groups) is almost always expressed
  via `Ref`/`Fn::ImportValue` to values that don't resolve to a literal ID at
  parse time, so the emitter adds a comment on the ECS service noting the
  original subnet/security-group refs for manual verification rather than
  guessing at equivalent networking.
- EFS access points are wired into the ECS task as `efs_volume_configuration`
  + `mountPoints`, with `platform_version` bumped to `1.4.0` (required for EFS
  on Fargate) — but the file system/access point IDs are left as required
  Terraform variables (no default) since they're existing infrastructure this
  tool doesn't create.
- Secrets Manager/SSM-backed env vars are detected (both via direct
  Ref/GetAtt and via CloudFormation's `{{resolve:...}}` dynamic-reference
  syntax) and rendered through ECS's native `secrets` container field
  (`valueFrom` an ARN variable) instead of being inlined into `environment`
  — the ARN variable defaults to a `TODO_REPLACE_WITH_ARN` placeholder.
- Kinesis and S3 can't invoke a long-running ECS container the way they
  invoke Lambda, so `kinesis_consumers.tf` / `s3_event_processors.tf`
  document the reconfiguration needed (KCL-based consumer, or an
  EventBridge/SQS relay for S3) rather than generating infrastructure that
  would silently change the bucket's/stream's existing configuration.

### Strangler migration pattern

The generated Terraform implements a coexistence architecture:

1. API Gateway continues serving all routes initially
2. ALB is deployed with path-based rules matching API Gateway routes
3. Each ECS service starts with `desired_count = 0` (dormant)
4. Migration proceeds per-service: scale up ECS, shift DNS/CloudFront, verify
5. When a route is confirmed on ECS, decommission the Lambda
6. EventBridge schedules switch to `ecs:RunTask` targets
7. SQS consumers become long-running ECS pollers

### Usage profile sidecar

Default usage estimates are topology-derived (API request counts flow through
edges to Lambda invocations, schedules are parsed into monthly counts). Override
with a JSON file for real numbers:

```json
{
  "lambdas": {
    "CreateOrderFn": {
      "monthly_invocations": 500000,
      "avg_duration_ms": 150
    }
  },
  "api_gateways": {
    "OrderApi": { "monthly_requests": 1000000 }
  },
  "sqs_queues": {
    "OrderQueue": { "monthly_messages": 250000 }
  }
}
```

## Project structure

```
.
├── main.go
├── cmd/
│   └── analyze.go                   # CLI flags and output
├── internal/
│   ├── model/
│   │   └── model.go                 # Resource types, edges, graph
│   ├── parser/
│   │   ├── parser.go                # Template loading, YAML intrinsics, SAM Globals
│   │   ├── extract.go               # Per-resource-type extraction
│   │   ├── edges.go                 # Reference resolution, edge building
│   │   └── parser_test.go           # Tests against synthetic stack
│   ├── cost/
│   │   ├── pricing.go               # Embedded pricing loader (go:embed)
│   │   ├── pricing.json             # Regional rates (eu-central-1, us-east-1)
│   │   ├── usage.go                 # Usage profiles (defaults + sidecar)
│   │   ├── estimate.go              # Serverless vs. ECS cost comparison
│   │   └── grouping.go              # Service grouping heuristic
│   ├── emit/
│   │   ├── emit.go                  # Terraform template rendering
│   │   └── templates/               # text/template .tmpl files
│   └── report/
│       └── report.go                # LLM report generation + fallback
├── examples/
│   └── synthetic-stack.yaml         # E-commerce order system
└── go.mod
```

## Dependencies

- `gopkg.in/yaml.v3` — YAML parsing with intrinsic function tag preservation

No other external dependencies. LLM integration uses `net/http` against any
OpenAI-compatible endpoint.

## Known limitations

- SAM `Globals` handled for `Function` only (Api, HttpApi, SimpleTable globals not merged)
- Cost estimates exclude CloudWatch Logs, NAT Gateway, and data transfer
- Step Functions auto-conversion is out of scope; patterns are classified and
  the LLM generates illustrative pseudocode
- Lambda@Edge, CloudFront, Cognito resources are detected but not modeled
- No live AWS mode (CloudWatch metrics, Cost Explorer) — file-only
