package model

import "fmt"

// EdgeType classifies the relationship between two resources.
type EdgeType string

const (
	EdgeInvokes      EdgeType = "invokes"      // API Gateway → Lambda
	EdgeTriggers     EdgeType = "triggers"      // SQS/SNS/EventBridge → Lambda
	EdgeOrchestrates EdgeType = "orchestrates"  // Step Functions → Lambda
	EdgePublishes    EdgeType = "publishes"     // Lambda → SNS/SQS
	EdgeReadsWrites  EdgeType = "reads_writes"  // Lambda → DynamoDB
	EdgeSubscribes   EdgeType = "subscribes"    // SQS → SNS (subscription)
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

// Edge represents a directional relationship between two resources.
type Edge struct {
	From   string   // source logical ID
	To     string   // target logical ID
	Type   EdgeType
	Detail string   // human-readable context, e.g. "POST /orders"
}

// Graph is the complete resource inventory with relationships.
type Graph struct {
	TemplateVersion string
	Description     string
	IsSAM           bool // true if template uses AWS::Serverless transform

	Lambdas    map[string]*Lambda
	APIs       map[string]*APIGateway
	Routes     []APIRoute
	StepFuncs  map[string]*StepFunction
	Rules      map[string]*EventBridgeRule
	Queues     map[string]*SQSQueue
	Topics     map[string]*SNSTopic
	Tables     map[string]*DynamoDBTable

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
		"lambdas=%d apis=%d stepfuncs=%d rules=%d queues=%d topics=%d tables=%d edges=%d unsupported=%d",
		len(g.Lambdas), len(g.APIs), len(g.StepFuncs), len(g.Rules),
		len(g.Queues), len(g.Topics), len(g.Tables), len(g.Edges), len(g.Unsupported),
	)
}
