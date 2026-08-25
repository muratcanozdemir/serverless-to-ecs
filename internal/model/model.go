package model

import "fmt"

// EdgeType classifies the relationship between two resources.
type EdgeType string

const (
	EdgeInvokes      EdgeType = "invokes"      // API Gateway → Lambda
	EdgeTriggers     EdgeType = "triggers"     // SQS/SNS/EventBridge/S3/Kinesis → Lambda
	EdgeOrchestrates EdgeType = "orchestrates" // Step Functions → Lambda
	EdgePublishes    EdgeType = "publishes"    // Lambda → SNS/SQS
	EdgeReadsWrites  EdgeType = "reads_writes" // Lambda → DynamoDB
	EdgeSubscribes   EdgeType = "subscribes"   // SQS → SNS (subscription)
	EdgeMounts       EdgeType = "mounts"       // Lambda → EFS access point
	EdgeReadsSecret  EdgeType = "reads_secret" // Lambda → Secrets Manager secret / SSM parameter
)

// SFNPattern classifies a Step Functions state machine's structure.
type SFNPattern string

const (
	SFNSequential SFNPattern = "sequential"
	SFNParallel   SFNPattern = "parallel"
	SFNChoice     SFNPattern = "choice"
	SFNMap        SFNPattern = "map"
	SFNMixed      SFNPattern = "mixed"
	SFNUnknown    SFNPattern = "unknown"
)

// Lambda represents an AWS Lambda function.
type Lambda struct {
	LogicalID    string
	FunctionName string
	Runtime      string
	Handler      string
	MemoryMB     int
	TimeoutSec   int
	CodeURI      string            // SAM: S3 path or local path
	EnvVars      map[string]string // environment variable values (resolved where possible)

	// VPCConfig is non-nil if the function runs inside a VPC. Subnet/security
	// group refs are kept as raw (usually unresolvable) references — CFN
	// intrinsics referring to a VPC's networking are frequently imported or
	// parameterized, so this is documentation for the migration, not a
	// concrete value the emitter can wire in automatically.
	VPCConfig *VPCConfig

	// EFSMounts are the function's FileSystemConfigs (Lambda → EFS access point).
	EFSMounts []EFSMount

	// SecretRefs maps an environment variable key to the Secrets Manager
	// secret or SSM parameter its value resolves to, when detected — via a
	// Ref/GetAtt to a modeled resource, or via CloudFormation's dynamic
	// reference syntax ("{{resolve:secretsmanager:...}}" /
	// "{{resolve:ssm:...}}") embedded directly in the value. Keys present
	// here still have an entry in EnvVars; consumers that want to treat
	// secrets differently (e.g. ECS's native "secrets" container field
	// instead of "environment") should check SecretRefs first.
	SecretRefs map[string]SecretRef
}

// VPCConfig records that a Lambda runs inside a VPC.
type VPCConfig struct {
	SubnetRefs        []string
	SecurityGroupRefs []string
}

// EFSMount is one entry of a Lambda's FileSystemConfigs.
type EFSMount struct {
	AccessPointRef string // logical ID of the AWS::EFS::AccessPoint, if resolvable
	LocalMountPath string
}

// SecretRef describes where an environment variable's value comes from when
// it's backed by Secrets Manager or SSM Parameter Store.
type SecretRef struct {
	Kind      string // "secretsmanager" or "ssm"
	LogicalID string // logical ID of the referenced template resource, if resolved via Ref/GetAtt (else empty)
	RawRef    string // the raw dynamic-reference string, if that's how it was expressed (else empty)
}

// APIGateway represents a REST or HTTP API.
type APIGateway struct {
	LogicalID string
	Name      string
	Protocol  string // "REST" or "HTTP"
	Stage     string
}

// APIRoute represents a single route on an API Gateway.
// Stored separately because routes come from Method/Integration resources or SAM Events.
type APIRoute struct {
	APIID     string // logical ID of the parent API
	Path      string
	Method    string
	TargetRef string // logical ID of the target Lambda
}

// StepFunction represents a Step Functions state machine.
type StepFunction struct {
	LogicalID string
	Name      string
	// Raw ASL definition. Kept as generic map so the pattern classifier
	// and LLM pseudocode generator can inspect it in WBS 2/4.
	DefinitionRaw map[string]interface{}
	StateCount    int
	Pattern       SFNPattern
	// Logical IDs of Lambdas referenced in Task states.
	TaskTargets []string
}

// EventBridgeRule represents a scheduled or event-pattern rule.
type EventBridgeRule struct {
	LogicalID      string
	Name           string
	Schedule       string // cron() or rate() expression, empty if event-pattern rule
	EventPattern   map[string]interface{}
	TargetRefs     []string // logical IDs of targets
	TargetLiterals []string // ARN literals that couldn't be resolved to a logical ID
}

// SQSQueue represents an SQS queue.
type SQSQueue struct {
	LogicalID            string
	QueueName            string
	FIFOQueue            bool
	VisibilityTimeoutSec int
	MessageRetentionSec  int
	DelaySeconds         int
}

// SNSTopic represents an SNS topic.
type SNSTopic struct {
	LogicalID string
	TopicName string
	FIFOTopic bool
}

// DynamoDBTable represents a DynamoDB table.
type DynamoDBTable struct {
	LogicalID   string
	TableName   string
	BillingMode string // "PAY_PER_REQUEST" or "PROVISIONED"
	RCU         int    // provisioned read capacity units (0 if on-demand)
	WCU         int    // provisioned write capacity units (0 if on-demand)
	HashKey     string
	RangeKey    string // empty if no sort key
	GSICount    int
}

// S3Bucket represents an S3 bucket.
type S3Bucket struct {
	LogicalID  string
	BucketName string
}

// KinesisStream represents a Kinesis data stream.
type KinesisStream struct {
	LogicalID  string
	StreamName string
	ShardCount int
}

// EFSFileSystem represents an EFS file system.
type EFSFileSystem struct {
	LogicalID string
}

// EFSAccessPoint represents an EFS access point — what a Lambda's
// FileSystemConfigs actually mounts (not the file system directly).
type EFSAccessPoint struct {
	LogicalID     string
	FileSystemRef string // logical ID of the parent EFSFileSystem, if resolvable
}

// SecretsManagerSecret represents a Secrets Manager secret.
type SecretsManagerSecret struct {
	LogicalID string
	Name      string
}

// SSMParameter represents an SSM Parameter Store parameter.
type SSMParameter struct {
	LogicalID string
	Name      string
	Type      string // "String", "StringList", "SecureString"
}

// Edge represents a directional relationship between two resources.
type Edge struct {
	From   string // source logical ID
	To     string // target logical ID
	Type   EdgeType
	Detail string // human-readable context, e.g. "POST /orders"
}

// Graph is the complete resource inventory with relationships.
type Graph struct {
	TemplateVersion string
	Description     string
	IsSAM           bool // true if template uses AWS::Serverless transform

	Lambdas      map[string]*Lambda
	APIs         map[string]*APIGateway
	Routes       []APIRoute
	StepFuncs    map[string]*StepFunction
	Rules        map[string]*EventBridgeRule
	Queues       map[string]*SQSQueue
	Topics       map[string]*SNSTopic
	Tables       map[string]*DynamoDBTable
	Buckets      map[string]*S3Bucket
	Streams      map[string]*KinesisStream
	FileSystems  map[string]*EFSFileSystem
	AccessPoints map[string]*EFSAccessPoint
	Secrets      map[string]*SecretsManagerSecret
	Parameters   map[string]*SSMParameter

	// HTTPIntegrations maps an AWS::ApiGatewayV2::Integration logical ID to
	// the Lambda logical ID it targets (resolved from IntegrationUri). Used
	// to link HTTP API (v2) Routes, which reference an Integration rather
	// than a Lambda directly, back to the Lambda they invoke.
	HTTPIntegrations map[string]string

	Edges []Edge

	// Resources we detected but don't model. Preserved for the report
	// so the user knows what the tool didn't analyze.
	Unsupported []UnsupportedResource
}

// UnsupportedResource records a CFN resource we recognized but don't handle.
type UnsupportedResource struct {
	LogicalID    string
	ResourceType string
}

// NewGraph returns an initialized empty graph.
func NewGraph() *Graph {
	return &Graph{
		Lambdas:          make(map[string]*Lambda),
		APIs:             make(map[string]*APIGateway),
		StepFuncs:        make(map[string]*StepFunction),
		Rules:            make(map[string]*EventBridgeRule),
		Queues:           make(map[string]*SQSQueue),
		Topics:           make(map[string]*SNSTopic),
		Tables:           make(map[string]*DynamoDBTable),
		Buckets:          make(map[string]*S3Bucket),
		Streams:          make(map[string]*KinesisStream),
		FileSystems:      make(map[string]*EFSFileSystem),
		AccessPoints:     make(map[string]*EFSAccessPoint),
		Secrets:          make(map[string]*SecretsManagerSecret),
		Parameters:       make(map[string]*SSMParameter),
		HTTPIntegrations: make(map[string]string),
	}
}

// AddEdge appends a relationship to the graph.
func (g *Graph) AddEdge(from, to string, edgeType EdgeType, detail string) {
	g.Edges = append(g.Edges, Edge{
		From:   from,
		To:     to,
		Type:   edgeType,
		Detail: detail,
	})
}

// Summary returns a one-line count of all resource types.
func (g *Graph) Summary() string {
	return fmt.Sprintf(
		"lambdas=%d apis=%d stepfuncs=%d rules=%d queues=%d topics=%d tables=%d "+
			"buckets=%d streams=%d filesystems=%d secrets=%d parameters=%d edges=%d unsupported=%d",
		len(g.Lambdas), len(g.APIs), len(g.StepFuncs), len(g.Rules),
		len(g.Queues), len(g.Topics), len(g.Tables),
		len(g.Buckets), len(g.Streams), len(g.FileSystems), len(g.Secrets), len(g.Parameters),
		len(g.Edges), len(g.Unsupported),
	)
}
