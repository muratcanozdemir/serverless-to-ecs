package cost

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed pricing.json
var pricingJSON []byte

// PricingDB holds all regional pricing data.
type PricingDB struct {
	Meta    PricingMeta                `json:"_meta"`
	Regions map[string]*RegionalPrices `json:"regions"`
}

type PricingMeta struct {
	DefaultRegion string   `json:"default_region"`
	Currency      string   `json:"currency"`
	LastVerified  string   `json:"last_verified"`
	Sources       []string `json:"sources"`
	Notes         string   `json:"notes"`
}

// RegionalPrices contains per-service pricing for one AWS region.
type RegionalPrices struct {
	Lambda         LambdaPricing         `json:"lambda"`
	APIGateway     APIGatewayPricing     `json:"api_gateway"`
	StepFunctions  StepFunctionsPricing  `json:"step_functions"`
	EventBridge    EventBridgePricing    `json:"eventbridge"`
	SQS            SQSPricing            `json:"sqs"`
	SNS            SNSPricing            `json:"sns"`
	DynamoDB       DynamoDBPricing       `json:"dynamodb"`
	Fargate        FargatePricing        `json:"fargate"`
	ALB            ALBPricing            `json:"alb"`
	S3             S3Pricing             `json:"s3"`
	Kinesis        KinesisPricing        `json:"kinesis"`
	EFS            EFSPricing            `json:"efs"`
	SecretsManager SecretsManagerPricing `json:"secrets_manager"`
}

type LambdaPricing struct {
	RequestPerMillion float64 `json:"request_per_million"`
	GBSecondX86       float64 `json:"gb_second_x86"`
	GBSecondARM       float64 `json:"gb_second_arm"`
}

type APIGatewayPricing struct {
	RESTPerMillion float64 `json:"rest_per_million"`
	HTTPPerMillion float64 `json:"http_per_million"`
}

type StepFunctionsPricing struct {
	StandardPer1KTransitions float64 `json:"standard_per_1k_transitions"`
	ExpressGBSecond          float64 `json:"express_gb_second"`
}

type EventBridgePricing struct {
	CustomPerMillion    float64 `json:"custom_per_million"`
	ScheduledPerMillion float64 `json:"scheduled_per_million"`
}

type SQSPricing struct {
	StandardPerMillion float64 `json:"standard_per_million"`
	FIFOPerMillion     float64 `json:"fifo_per_million"`
}

type SNSPricing struct {
	PublishPerMillion float64 `json:"publish_per_million"`
}

type DynamoDBPricing struct {
	OnDemandRRUPerMillion float64 `json:"on_demand_rru_per_million"`
	OnDemandWRUPerMillion float64 `json:"on_demand_wru_per_million"`
	ProvisionedRCUMonth   float64 `json:"provisioned_rcu_per_month"`
	ProvisionedWCUMonth   float64 `json:"provisioned_wcu_per_month"`
}

type FargatePricing struct {
	LinuxX86VCPUPerHour float64 `json:"linux_x86_vcpu_per_hour"`
	LinuxX86GBPerHour   float64 `json:"linux_x86_gb_per_hour"`
	LinuxARMVCPUPerHour float64 `json:"linux_arm_vcpu_per_hour"`
	LinuxARMGBPerHour   float64 `json:"linux_arm_gb_per_hour"`
}

type ALBPricing struct {
	PerHour    float64 `json:"per_hour"`
	LCUPerHour float64 `json:"lcu_per_hour"`
}

type S3Pricing struct {
	StandardStoragePerGBMonth float64 `json:"standard_storage_per_gb_month"`
	PutRequestsPerThousand    float64 `json:"put_requests_per_thousand"`
	GetRequestsPerThousand    float64 `json:"get_requests_per_thousand"`
}

type KinesisPricing struct {
	ShardPerHour             float64 `json:"shard_per_hour"`
	PutPayloadUnitPerMillion float64 `json:"put_payload_unit_per_million"`
}

type EFSPricing struct {
	StandardStoragePerGBMonth float64 `json:"standard_storage_per_gb_month"`
}

// SecretsManagerPricing covers Secrets Manager only — SSM Standard
// parameters are free, so there's no cost line item for AWS::SSM::Parameter
// (SSM Advanced parameters and the Parameter Store API-call-based pricing
// tier aren't modeled; this tool tracks Standard parameters).
type SecretsManagerPricing struct {
	PerSecretPerMonth      float64 `json:"per_secret_per_month"`
	APICallsPerTenThousand float64 `json:"api_calls_per_ten_thousand"`
}

// LoadPricing loads the embedded pricing database.
func LoadPricing() (*PricingDB, error) {
	var db PricingDB
	if err := json.Unmarshal(pricingJSON, &db); err != nil {
		return nil, fmt.Errorf("parse embedded pricing: %w", err)
	}
	return &db, nil
}

// ForRegion returns pricing for the given region, falling back to the default.
func (db *PricingDB) ForRegion(region string) (*RegionalPrices, error) {
	if region == "" {
		region = db.Meta.DefaultRegion
	}
	p, ok := db.Regions[region]
	if !ok {
		return nil, fmt.Errorf("no pricing data for region %q (available: %v)", region, db.RegionList())
	}
	return p, nil
}

// RegionList returns available region identifiers.
func (db *PricingDB) RegionList() []string {
	out := make([]string, 0, len(db.Regions))
	for r := range db.Regions {
		out = append(out, r)
	}
	return out
}
