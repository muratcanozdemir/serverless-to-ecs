package cost

import (
	"fmt"
	"math"

	"serverless-to-ecs/internal/model"
)

// Estimate holds the full cost breakdown: current serverless vs. projected ECS.
type Estimate struct {
	Region   string `json:"region"`
	Accuracy string `json:"accuracy"` // e.g. "±30% (heuristic defaults)" or "±15% (sidecar data)"

	Serverless ServerlessCost `json:"serverless"`
	ECS        ECSCost        `json:"ecs"`
	Savings    SavingsSummary `json:"savings"`
}

// ServerlessCost is the itemized current monthly spend.
type ServerlessCost struct {
	Lambda         []LambdaCost `json:"lambda"`
	APIGateway     []ItemCost   `json:"api_gateway"`
	StepFunctions  []ItemCost   `json:"step_functions"`
	EventBridge    []ItemCost   `json:"eventbridge"`
	SQS            []ItemCost   `json:"sqs"`
	SNS            []ItemCost   `json:"sns"`
	DynamoDB       []ItemCost   `json:"dynamodb"`
	S3             []ItemCost   `json:"s3"`
	Kinesis        []ItemCost   `json:"kinesis"`
	EFS            []ItemCost   `json:"efs"`
	SecretsManager []ItemCost   `json:"secrets_manager"`
	Total          float64      `json:"total"`
}

// LambdaCost is the cost breakdown for one Lambda function.
type LambdaCost struct {
	LogicalID    string  `json:"logical_id"`
	FunctionName string  `json:"function_name"`
	RequestCost  float64 `json:"request_cost"`
	ComputeCost  float64 `json:"compute_cost"`
	Total        float64 `json:"total"`
	Invocations  int     `json:"invocations"`
	GBSeconds    float64 `json:"gb_seconds"`
}

// ItemCost is a generic single-line cost item.
type ItemCost struct {
	LogicalID string  `json:"logical_id"`
	Name      string  `json:"name"`
	Total     float64 `json:"total"`
	Detail    string  `json:"detail"`
}

// ECSCost is the projected monthly spend after migration.
type ECSCost struct {
	Services []ECSServiceCost `json:"services"`
	ALB      float64          `json:"alb"`
	// Resources that stay serverless after migration (DynamoDB, SQS, SNS, etc.)
	Retained float64 `json:"retained"`
	Total    float64 `json:"total"`
}

// ECSServiceCost is the cost of one ECS Fargate service.
type ECSServiceCost struct {
	Name       string  `json:"name"`
	VCPUs      float64 `json:"vcpus"`
	MemoryGB   float64 `json:"memory_gb"`
	Tasks      int     `json:"tasks"`
	VCPUCost   float64 `json:"vcpu_cost"`
	MemoryCost float64 `json:"memory_cost"`
	Total      float64 `json:"total"`
}

// SavingsSummary compares current vs. projected costs.
type SavingsSummary struct {
	CurrentMonthly   float64 `json:"current_monthly"`
	ProjectedMonthly float64 `json:"projected_monthly"`
	DeltaAbsolute    float64 `json:"delta_absolute"`
	DeltaPercent     float64 `json:"delta_percent"`
}

// EstimateCosts produces the full cost comparison from a parsed graph and usage profile.
func EstimateCosts(g *model.Graph, usage *UsageProfile, prices *RegionalPrices, region string) *Estimate {
	est := &Estimate{
		Region:   region,
		Accuracy: classifyAccuracy(usage),
	}

	est.Serverless = estimateServerless(g, usage, prices)
	est.ECS = estimateECS(g, usage, prices)

	est.Savings = SavingsSummary{
		CurrentMonthly:   est.Serverless.Total,
		ProjectedMonthly: est.ECS.Total,
		DeltaAbsolute:    est.Serverless.Total - est.ECS.Total,
	}
	if est.Serverless.Total > 0 {
		est.Savings.DeltaPercent = (est.Savings.DeltaAbsolute / est.Serverless.Total) * 100.0
	}

	return est
}

func classifyAccuracy(usage *UsageProfile) string {
	sidecar := 0
	total := 0
	for _, u := range usage.Lambdas {
		total++
		if u.Source == "sidecar" {
			sidecar++
		}
	}
	if total == 0 {
		return "±30% (heuristic defaults)"
	}
	ratio := float64(sidecar) / float64(total)
	if ratio > 0.5 {
		return "±15% (sidecar data for >50% of functions)"
	}
	if ratio > 0 {
		return "±20% (partial sidecar data)"
	}
	return "±30% (heuristic defaults)"
}

// --- Serverless cost calculation ---

func estimateServerless(g *model.Graph, usage *UsageProfile, p *RegionalPrices) ServerlessCost {
	var sc ServerlessCost

	// Lambda.
	for _, id := range sortedKeys(g.Lambdas) {
		fn := g.Lambdas[id]
		u := usage.Lambdas[id]
		if u == nil {
			continue
		}
		lc := lambdaCost(fn, u, p)
		sc.Lambda = append(sc.Lambda, lc)
		sc.Total += lc.Total
	}

	// API Gateway.
	for _, id := range sortedKeys(g.APIs) {
		api := g.APIs[id]
		u := usage.APIs[id]
		if u == nil {
			continue
		}
		rate := p.APIGateway.RESTPerMillion
		if api.Protocol == "HTTP" {
			rate = p.APIGateway.HTTPPerMillion
		}
		cost := float64(u.MonthlyRequests) / 1_000_000.0 * rate
		sc.APIGateway = append(sc.APIGateway, ItemCost{
			LogicalID: id,
			Name:      api.Name,
			Total:     cost,
			Detail:    api.Protocol + " API",
		})
		sc.Total += cost
	}

	// Step Functions.
	for _, id := range sortedKeys(g.StepFuncs) {
		sf := g.StepFuncs[id]
		u := usage.StepFuncs[id]
		if u == nil {
			continue
		}
		transitions := float64(u.MonthlyExecutions * u.AvgTransitionsPerExec)
		cost := (transitions / 1000.0) * p.StepFunctions.StandardPer1KTransitions
		sc.StepFunctions = append(sc.StepFunctions, ItemCost{
			LogicalID: id,
			Name:      sf.Name,
			Total:     cost,
			Detail:    formatInt(int(transitions)) + " state transitions",
		})
		sc.Total += cost
	}

	// EventBridge rules.
	for _, id := range sortedKeys(g.Rules) {
		rule := g.Rules[id]
		u := usage.Rules[id]
		if u == nil {
			continue
		}
		rate := p.EventBridge.CustomPerMillion
		if rule.Schedule != "" {
			rate = p.EventBridge.ScheduledPerMillion
			// Scheduled rules are free for invocation; cost is in the target Lambda.
			// But we still record the line item for the report.
		}
		cost := float64(u.MonthlyInvocations) / 1_000_000.0 * rate
		sc.EventBridge = append(sc.EventBridge, ItemCost{
			LogicalID: id,
			Name:      rule.Name,
			Total:     cost,
			Detail:    rule.Schedule,
		})
		sc.Total += cost
	}

	// SQS.
	for _, id := range sortedKeys(g.Queues) {
		q := g.Queues[id]
		u := usage.Queues[id]
		if u == nil {
			continue
		}
		rate := p.SQS.StandardPerMillion
		if q.FIFOQueue {
			rate = p.SQS.FIFOPerMillion
		}
		cost := float64(u.MonthlyMessages) / 1_000_000.0 * rate
		sc.SQS = append(sc.SQS, ItemCost{
			LogicalID: id,
			Name:      q.QueueName,
			Total:     cost,
			Detail:    formatInt(u.MonthlyMessages) + " messages",
		})
		sc.Total += cost
	}

	// SNS.
	for _, id := range sortedKeys(g.Topics) {
		u := usage.Topics[id]
		if u == nil {
			continue
		}
		cost := float64(u.MonthlyPublishes) / 1_000_000.0 * p.SNS.PublishPerMillion
		sc.SNS = append(sc.SNS, ItemCost{
			LogicalID: id,
			Total:     cost,
			Detail:    formatInt(u.MonthlyPublishes) + " publishes",
		})
		sc.Total += cost
	}

	// DynamoDB.
	for _, id := range sortedKeys(g.Tables) {
		table := g.Tables[id]
		u := usage.Tables[id]
		if u == nil {
			continue
		}
		var cost float64
		var detail string
		if table.BillingMode == "PROVISIONED" {
			// Provisioned: monthly cost = RCU * rate + WCU * rate * hours_in_month.
			hours := 730.0
			cost = float64(table.RCU)*p.DynamoDB.ProvisionedRCUMonth*hours +
				float64(table.WCU)*p.DynamoDB.ProvisionedWCUMonth*hours
			detail = formatInt(table.RCU) + " RCU, " + formatInt(table.WCU) + " WCU (provisioned)"
		} else {
			cost = float64(u.MonthlyReadUnits)/1_000_000.0*p.DynamoDB.OnDemandRRUPerMillion +
				float64(u.MonthlyWriteUnits)/1_000_000.0*p.DynamoDB.OnDemandWRUPerMillion
			detail = formatInt(u.MonthlyReadUnits) + " RRU, " + formatInt(u.MonthlyWriteUnits) + " WRU (on-demand)"
		}
		sc.DynamoDB = append(sc.DynamoDB, ItemCost{
			LogicalID: id,
			Name:      table.TableName,
			Total:     cost,
			Detail:    detail,
		})
		sc.Total += cost
	}

	// S3.
	for _, id := range sortedKeys(g.Buckets) {
		bucket := g.Buckets[id]
		u := usage.Buckets[id]
		if u == nil {
			continue
		}
		cost := s3Cost(u, p)
		sc.S3 = append(sc.S3, ItemCost{
			LogicalID: id,
			Name:      bucket.BucketName,
			Total:     cost,
			Detail:    fmt.Sprintf("%.0fGB, %s PUTs, %s GETs", u.StorageGB, formatInt(u.MonthlyPuts), formatInt(u.MonthlyGets)),
		})
		sc.Total += cost
	}

	// Kinesis.
	for _, id := range sortedKeys(g.Streams) {
		stream := g.Streams[id]
		u := usage.Streams[id]
		if u == nil {
			continue
		}
		cost := kinesisCost(stream, u, p)
		sc.Kinesis = append(sc.Kinesis, ItemCost{
			LogicalID: id,
			Name:      stream.StreamName,
			Total:     cost,
			Detail:    fmt.Sprintf("%d shards, %s PUT records", stream.ShardCount, formatInt(u.MonthlyPutRecords)),
		})
		sc.Total += cost
	}

	// EFS.
	for _, id := range sortedKeys(g.FileSystems) {
		u := usage.FileSystems[id]
		if u == nil {
			continue
		}
		cost := efsCost(u, p)
		sc.EFS = append(sc.EFS, ItemCost{
			LogicalID: id,
			Total:     cost,
			Detail:    fmt.Sprintf("%.0fGB storage", u.StorageGB),
		})
		sc.Total += cost
	}

	// Secrets Manager. SSM Standard parameters are free and aren't costed.
	for _, id := range sortedKeys(g.Secrets) {
		secret := g.Secrets[id]
		u := usage.Secrets[id]
		if u == nil {
			continue
		}
		cost := secretCost(u, p)
		sc.SecretsManager = append(sc.SecretsManager, ItemCost{
			LogicalID: id,
			Name:      secret.Name,
			Total:     cost,
			Detail:    formatInt(u.MonthlyAPICalls) + " API calls",
		})
		sc.Total += cost
	}

	return sc
}

// s3Cost, kinesisCost, efsCost, and secretCost are shared between
// estimateServerless (itemized current cost) and estimateECS (retained
// cost — these resources aren't replaced by the migration, so the same
// monthly cost carries over) to avoid the formulas drifting apart.

func s3Cost(u *S3Usage, p *RegionalPrices) float64 {
	if u == nil {
		return 0
	}
	return u.StorageGB*p.S3.StandardStoragePerGBMonth +
		float64(u.MonthlyPuts)/1000.0*p.S3.PutRequestsPerThousand +
		float64(u.MonthlyGets)/1000.0*p.S3.GetRequestsPerThousand
}

func kinesisCost(stream *model.KinesisStream, u *KinesisUsage, p *RegionalPrices) float64 {
	if u == nil {
		return 0
	}
	shards := stream.ShardCount
	if shards < 1 {
		shards = 1
	}
	hoursPerMonth := 730.0
	return float64(shards)*p.Kinesis.ShardPerHour*hoursPerMonth +
		float64(u.MonthlyPutRecords)/1_000_000.0*p.Kinesis.PutPayloadUnitPerMillion
}

func efsCost(u *EFSUsage, p *RegionalPrices) float64 {
	if u == nil {
		return 0
	}
	return u.StorageGB * p.EFS.StandardStoragePerGBMonth
}

func secretCost(u *SecretUsage, p *RegionalPrices) float64 {
	if u == nil {
		return 0
	}
	return p.SecretsManager.PerSecretPerMonth +
		float64(u.MonthlyAPICalls)/10_000.0*p.SecretsManager.APICallsPerTenThousand
}

func lambdaCost(fn *model.Lambda, u *LambdaUsage, p *RegionalPrices) LambdaCost {
	invocations := float64(u.MonthlyInvocations)
	memoryGB := float64(fn.MemoryMB) / 1024.0
	durationSec := u.AvgDurationMs / 1000.0
	gbSeconds := invocations * memoryGB * durationSec

	reqCost := invocations / 1_000_000.0 * p.Lambda.RequestPerMillion
	computeCost := gbSeconds * p.Lambda.GBSecondX86

	return LambdaCost{
		LogicalID:    fn.LogicalID,
		FunctionName: fn.FunctionName,
		RequestCost:  reqCost,
		ComputeCost:  computeCost,
		Total:        reqCost + computeCost,
		Invocations:  u.MonthlyInvocations,
		GBSeconds:    gbSeconds,
	}
}

// --- ECS cost projection ---

func estimateECS(g *model.Graph, usage *UsageProfile, p *RegionalPrices) ECSCost {
	var ecs ECSCost

	groups := GroupServices(g)

	for _, group := range groups {
		svc := projectECSService(g, usage, group, p)
		ecs.Services = append(ecs.Services, svc)
		ecs.Total += svc.Total
	}

	// ALB: one load balancer running 24/7.
	hoursPerMonth := 730.0
	ecs.ALB = p.ALB.PerHour*hoursPerMonth + p.ALB.LCUPerHour*hoursPerMonth*estimateLCUs(g, usage)
	ecs.Total += ecs.ALB

	// Retained costs: DynamoDB, SQS, SNS stay the same after migration.
	// EventBridge scheduled rules are replaced by ECS scheduled tasks (no separate cost).
	// Step Functions are replaced by in-process orchestration (no separate cost).
	for id, table := range g.Tables {
		u := usage.Tables[id]
		if u == nil {
			continue
		}
		if table.BillingMode == "PROVISIONED" {
			ecs.Retained += float64(table.RCU)*p.DynamoDB.ProvisionedRCUMonth*730.0 +
				float64(table.WCU)*p.DynamoDB.ProvisionedWCUMonth*730.0
		} else {
			ecs.Retained += float64(u.MonthlyReadUnits)/1_000_000.0*p.DynamoDB.OnDemandRRUPerMillion +
				float64(u.MonthlyWriteUnits)/1_000_000.0*p.DynamoDB.OnDemandWRUPerMillion
		}
	}
	for id := range g.Queues {
		u := usage.Queues[id]
		if u == nil {
			continue
		}
		rate := p.SQS.StandardPerMillion
		if g.Queues[id].FIFOQueue {
			rate = p.SQS.FIFOPerMillion
		}
		ecs.Retained += float64(u.MonthlyMessages) / 1_000_000.0 * rate
	}
	for id := range g.Topics {
		u := usage.Topics[id]
		if u == nil {
			continue
		}
		ecs.Retained += float64(u.MonthlyPublishes) / 1_000_000.0 * p.SNS.PublishPerMillion
	}
	// S3, Kinesis, EFS, and Secrets Manager also aren't replaced by the
	// migration — buckets/streams/file systems/secrets stay exactly as they
	// are, just referenced by ECS tasks instead of Lambdas.
	for id := range g.Buckets {
		ecs.Retained += s3Cost(usage.Buckets[id], p)
	}
	for id, stream := range g.Streams {
		ecs.Retained += kinesisCost(stream, usage.Streams[id], p)
	}
	for id := range g.FileSystems {
		ecs.Retained += efsCost(usage.FileSystems[id], p)
	}
	for id := range g.Secrets {
		ecs.Retained += secretCost(usage.Secrets[id], p)
	}
	ecs.Total += ecs.Retained

	return ecs
}

// projectECSService sizes a Fargate task definition based on the Lambda functions in a service group.
func projectECSService(g *model.Graph, usage *UsageProfile, group ServiceGroup, p *RegionalPrices) ECSServiceCost {
	// Size the task: max memory across group members, with a floor.
	var maxMemoryMB int
	var totalInvocations int
	for _, fnID := range group.LambdaIDs {
		fn := g.Lambdas[fnID]
		if fn != nil && fn.MemoryMB > maxMemoryMB {
			maxMemoryMB = fn.MemoryMB
		}
		if u := usage.Lambdas[fnID]; u != nil {
			totalInvocations += u.MonthlyInvocations
		}
	}

	// Map Lambda memory to Fargate task size. Lambda memory includes CPU proportionally,
	// but Fargate decouples vCPU and memory. Use reasonable defaults.
	vcpus, memoryGB := fargateTaskSize(maxMemoryMB)

	// Task count: for API-serving services, 2 tasks minimum (HA).
	// For queue processors, 1 task is often sufficient.
	tasks := 2
	if group.Type == "queue-processor" || group.Type == "scheduled" {
		tasks = 1
	}
	// Scale up for high-traffic services.
	if totalInvocations > 1_000_000 {
		tasks = 3
	}
	if totalInvocations > 5_000_000 {
		tasks = 5
	}

	hoursPerMonth := 730.0
	vcpuCost := float64(tasks) * vcpus * p.Fargate.LinuxX86VCPUPerHour * hoursPerMonth
	memCost := float64(tasks) * memoryGB * p.Fargate.LinuxX86GBPerHour * hoursPerMonth

	return ECSServiceCost{
		Name:       group.Name,
		VCPUs:      vcpus,
		MemoryGB:   memoryGB,
		Tasks:      tasks,
		VCPUCost:   vcpuCost,
		MemoryCost: memCost,
		Total:      vcpuCost + memCost,
	}
}

// fargateTaskSize maps Lambda memory to an appropriate Fargate vCPU/memory config.
// Fargate has fixed vCPU/memory combinations.
func fargateTaskSize(lambdaMemoryMB int) (vcpus float64, memoryGB float64) {
	switch {
	case lambdaMemoryMB <= 512:
		return 0.25, 0.5
	case lambdaMemoryMB <= 1024:
		return 0.5, 1.0
	case lambdaMemoryMB <= 2048:
		return 1.0, 2.0
	case lambdaMemoryMB <= 4096:
		return 2.0, 4.0
	default:
		return 4.0, 8.0
	}
}

// estimateLCUs guesses ALB Load Capacity Units from request volume.
// 1 LCU ≈ 25 new connections/sec or 3K active connections or 1 GB/hour.
func estimateLCUs(g *model.Graph, usage *UsageProfile) float64 {
	var totalRequests int
	for id := range g.APIs {
		if u := usage.APIs[id]; u != nil {
			totalRequests += u.MonthlyRequests
		}
	}
	// Convert monthly requests to requests/second.
	rps := float64(totalRequests) / (30.0 * 24.0 * 3600.0)
	// Rough LCU estimate: 1 LCU ≈ 25 new conn/sec.
	lcus := math.Max(rps/25.0, 1.0)
	return lcus
}

func formatInt(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000.0)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000.0)
	}
	return fmt.Sprintf("%d", n)
}
