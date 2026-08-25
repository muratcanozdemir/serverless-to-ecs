package cost

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"

	"serverless-to-ecs/internal/model"
)

// UsageProfile holds estimated monthly usage for every resource in the graph.
type UsageProfile struct {
	Lambdas   map[string]*LambdaUsage    `json:"lambdas"`
	APIs      map[string]*APIUsage       `json:"api_gateways"`
	StepFuncs map[string]*StepFuncUsage  `json:"step_functions"`
	Rules     map[string]*EventRuleUsage `json:"eventbridge_rules"`
	Queues    map[string]*SQSUsage       `json:"sqs_queues"`
	Topics    map[string]*SNSUsage       `json:"sns_topics"`
	Tables    map[string]*DynamoDBUsage  `json:"dynamodb_tables"`
}

type LambdaUsage struct {
	MonthlyInvocations int     `json:"monthly_invocations"`
	AvgDurationMs      float64 `json:"avg_duration_ms"`
	Source             string  `json:"source"` // "default", "sidecar", "derived"
}

type APIUsage struct {
	MonthlyRequests int    `json:"monthly_requests"`
	Source          string `json:"source"`
}

type StepFuncUsage struct {
	MonthlyExecutions     int    `json:"monthly_executions"`
	AvgTransitionsPerExec int    `json:"avg_transitions_per_execution"`
	Source                string `json:"source"`
}

type EventRuleUsage struct {
	MonthlyInvocations int    `json:"monthly_invocations"`
	Source             string `json:"source"`
}

type SQSUsage struct {
	MonthlyMessages int    `json:"monthly_messages"`
	Source          string `json:"source"`
}

type SNSUsage struct {
	MonthlyPublishes int    `json:"monthly_publishes"`
	Source           string `json:"source"`
}

type DynamoDBUsage struct {
	MonthlyReadUnits  int    `json:"monthly_read_units"`
	MonthlyWriteUnits int    `json:"monthly_write_units"`
	Source            string `json:"source"`
}

// DefaultProfile generates usage estimates from the resource graph topology.
// These are heuristic-based: good enough for ±30% ballpark, not for budgeting.
func DefaultProfile(g *model.Graph) *UsageProfile {
	p := &UsageProfile{
		Lambdas:   make(map[string]*LambdaUsage),
		APIs:      make(map[string]*APIUsage),
		StepFuncs: make(map[string]*StepFuncUsage),
		Rules:     make(map[string]*EventRuleUsage),
		Queues:    make(map[string]*SQSUsage),
		Topics:    make(map[string]*SNSUsage),
		Tables:    make(map[string]*DynamoDBUsage),
	}

	// API Gateways: estimate request volume from route count.
	for id, api := range g.APIs {
		routes := countRoutes(g, id)
		p.APIs[id] = &APIUsage{
			MonthlyRequests: baseAPIRequests(api, routes),
			Source:          "default",
		}
	}

	// EventBridge rules: derive from schedule expression.
	for id, rule := range g.Rules {
		p.Rules[id] = &EventRuleUsage{
			MonthlyInvocations: scheduleToMonthly(rule.Schedule),
			Source:             "default",
		}
	}

	// Step Functions: assume moderate execution rate.
	for id, sf := range g.StepFuncs {
		p.StepFuncs[id] = &StepFuncUsage{
			MonthlyExecutions:     10000,
			AvgTransitionsPerExec: max(sf.StateCount, 1),
			Source:                "default",
		}
	}

	// SQS queues: derive from downstream Lambda trigger edges.
	for id, q := range g.Queues {
		p.Queues[id] = &SQSUsage{
			MonthlyMessages: baseSQSMessages(q),
			Source:          "default",
		}
	}

	// SNS topics.
	for id := range g.Topics {
		p.Topics[id] = &SNSUsage{
			MonthlyPublishes: 50000,
			Source:           "default",
		}
	}

	// DynamoDB tables.
	for id, table := range g.Tables {
		p.Tables[id] = defaultDynamoUsage(table)
	}

	// Lambdas: derive from trigger sources.
	for id, fn := range g.Lambdas {
		p.Lambdas[id] = deriveLambdaUsage(g, p, id, fn)
	}

	return p
}

// LoadSidecar reads usage overrides from a JSON file and merges them
// into the profile. Only non-zero fields in the sidecar overwrite defaults.
func (p *UsageProfile) LoadSidecar(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read usage sidecar: %w", err)
	}

	var override UsageProfile
	if err := json.Unmarshal(data, &override); err != nil {
		return fmt.Errorf("parse usage sidecar: %w", err)
	}

	for id, u := range override.Lambdas {
		if existing, ok := p.Lambdas[id]; ok {
			if u.MonthlyInvocations > 0 {
				existing.MonthlyInvocations = u.MonthlyInvocations
			}
			if u.AvgDurationMs > 0 {
				existing.AvgDurationMs = u.AvgDurationMs
			}
			existing.Source = "sidecar"
		}
	}
	for id, u := range override.APIs {
		if existing, ok := p.APIs[id]; ok && u.MonthlyRequests > 0 {
			existing.MonthlyRequests = u.MonthlyRequests
			existing.Source = "sidecar"
		}
	}
	for id, u := range override.StepFuncs {
		if existing, ok := p.StepFuncs[id]; ok {
			if u.MonthlyExecutions > 0 {
				existing.MonthlyExecutions = u.MonthlyExecutions
			}
			if u.AvgTransitionsPerExec > 0 {
				existing.AvgTransitionsPerExec = u.AvgTransitionsPerExec
			}
			existing.Source = "sidecar"
		}
	}
	for id, u := range override.Queues {
		if existing, ok := p.Queues[id]; ok && u.MonthlyMessages > 0 {
			existing.MonthlyMessages = u.MonthlyMessages
			existing.Source = "sidecar"
		}
	}
	for id, u := range override.Tables {
		if existing, ok := p.Tables[id]; ok {
			if u.MonthlyReadUnits > 0 {
				existing.MonthlyReadUnits = u.MonthlyReadUnits
			}
			if u.MonthlyWriteUnits > 0 {
				existing.MonthlyWriteUnits = u.MonthlyWriteUnits
			}
			existing.Source = "sidecar"
		}
	}

	return nil
}

// --- Heuristic helpers ---

func countRoutes(g *model.Graph, apiID string) int {
	n := 0
	for _, r := range g.Routes {
		if r.APIID == apiID {
			n++
		}
	}
	return n
}

// baseAPIRequests: REST APIs with more routes tend to serve higher traffic.
// Baseline 100K/month for a low-traffic API, scaled by route count.
func baseAPIRequests(api *model.APIGateway, routeCount int) int {
	base := 100_000
	if routeCount > 3 {
		base = 500_000
	}
	if api.Protocol == "HTTP" {
		base = base * 2 // HTTP APIs are typically higher-traffic (lower cost encourages volume)
	}
	return base
}

// scheduleToMonthly converts a CloudWatch/EventBridge schedule expression
// to approximate monthly invocations.
func scheduleToMonthly(schedule string) int {
	if schedule == "" {
		// Event-pattern rule with no schedule — assume 10K events/month.
		return 10_000
	}

	schedule = strings.TrimSpace(schedule)

	// rate(N unit) expressions.
	if strings.HasPrefix(schedule, "rate(") {
		return rateToMonthly(schedule)
	}

	// cron() expressions — extract frequency from the fields.
	if strings.HasPrefix(schedule, "cron(") {
		return cronToMonthly(schedule)
	}

	return 10_000 // fallback
}

var rateRegex = regexp.MustCompile(`rate\(\s*(\d+)\s+(minute|minutes|hour|hours|day|days)\s*\)`)

func rateToMonthly(expr string) int {
	m := rateRegex.FindStringSubmatch(expr)
	if len(m) < 3 {
		return 10_000
	}
	n, _ := strconv.Atoi(m[1])
	if n == 0 {
		n = 1
	}
	unit := m[2]

	minutesPerMonth := 30.0 * 24.0 * 60.0

	switch {
	case strings.HasPrefix(unit, "minute"):
		return int(minutesPerMonth / float64(n))
	case strings.HasPrefix(unit, "hour"):
		return int(minutesPerMonth / (float64(n) * 60.0))
	case strings.HasPrefix(unit, "day"):
		return int(30.0 / float64(n))
	}
	return 10_000
}

func cronToMonthly(expr string) int {
	// cron(min hour dom month dow year)
	// Simple heuristic: if hour is specific, it runs daily.
	// If minute is */N, it runs every N minutes.
	inner := strings.TrimPrefix(expr, "cron(")
	inner = strings.TrimSuffix(inner, ")")
	fields := strings.Fields(inner)
	if len(fields) < 5 {
		return 10_000
	}

	minute := fields[0]
	hour := fields[1]

	// Every N minutes.
	if strings.HasPrefix(minute, "*/") {
		n, _ := strconv.Atoi(strings.TrimPrefix(minute, "*/"))
		if n > 0 {
			return int(30.0 * 24.0 * 60.0 / float64(n))
		}
	}

	// Specific minute + specific hour = once daily.
	if hour != "*" && minute != "*" {
		return 30
	}

	// Specific minute, every hour.
	if hour == "*" && minute != "*" {
		return 30 * 24
	}

	return 30 // daily fallback
}

func baseSQSMessages(q *model.SQSQueue) int {
	base := 100_000
	if q.FIFOQueue {
		base = 50_000 // FIFO queues tend to be lower-volume, higher-value
	}
	return base
}

func defaultDynamoUsage(table *model.DynamoDBTable) *DynamoDBUsage {
	if table.BillingMode == "PROVISIONED" {
		// For provisioned tables, cost is based on allocated capacity, not request volume.
		// We'll estimate from RCU/WCU in the cost estimator.
		return &DynamoDBUsage{
			Source: "default",
		}
	}
	// On-demand: assume moderate CRUD workload.
	return &DynamoDBUsage{
		MonthlyReadUnits:  500_000,
		MonthlyWriteUnits: 100_000,
		Source:            "default",
	}
}

// deriveLambdaUsage determines invocation count from the Lambda's trigger sources.
func deriveLambdaUsage(g *model.Graph, p *UsageProfile, logicalID string, fn *model.Lambda) *LambdaUsage {
	invocations := 0
	source := "default"

	for _, edge := range g.Edges {
		if edge.To != logicalID {
			continue
		}

		switch edge.Type {
		case model.EdgeInvokes:
			// Triggered by API Gateway — use API request count.
			if u, ok := p.APIs[edge.From]; ok {
				invocations += u.MonthlyRequests
				source = "derived"
			}

		case model.EdgeTriggers:
			// SQS, SNS, EventBridge, schedule.
			if u, ok := p.Queues[edge.From]; ok {
				invocations += u.MonthlyMessages
				source = "derived"
			} else if u, ok := p.Rules[edge.From]; ok {
				invocations += u.MonthlyInvocations
				source = "derived"
			} else if u, ok := p.Topics[edge.From]; ok {
				invocations += u.MonthlyPublishes
				source = "derived"
			} else if strings.HasPrefix(edge.From, "__schedule_") {
				// SAM implicit schedule — parse from edge detail.
				if strings.Contains(edge.Detail, "cron") || strings.Contains(edge.Detail, "rate") {
					sched := strings.TrimPrefix(edge.Detail, "SAM schedule: ")
					invocations += scheduleToMonthly(sched)
					source = "derived"
				}
			}

		case model.EdgeOrchestrates:
			// Called by Step Functions — derive from SFN executions × avg calls per execution.
			if u, ok := p.StepFuncs[edge.From]; ok {
				invocations += u.MonthlyExecutions
				source = "derived"
			}
		}
	}

	// Fallback: no trigger edges found.
	if invocations == 0 {
		invocations = 10_000
		source = "default"
	}

	// Duration heuristic: correlate with memory and timeout.
	avgDuration := estimateDuration(fn)

	return &LambdaUsage{
		MonthlyInvocations: invocations,
		AvgDurationMs:      avgDuration,
		Source:             source,
	}
}

// estimateDuration guesses average execution time from function configuration.
// Higher memory → typically CPU-intensive → longer average. Higher timeout → expects longer runs.
// These are rough: 200ms baseline, scaled by memory and capped well below timeout.
func estimateDuration(fn *model.Lambda) float64 {
	// Baseline 200ms for a typical API handler.
	base := 200.0

	// Memory scaling: functions above 512MB tend to be doing real work.
	if fn.MemoryMB >= 1024 {
		base = 500.0
	} else if fn.MemoryMB >= 512 {
		base = 300.0
	}

	// Timeout scaling: if the timeout is very high, the function
	// expects long runs. Use 10% of timeout as average, capped.
	timeoutAvg := float64(fn.TimeoutSec) * 1000.0 * 0.10
	avg := math.Min(base, timeoutAvg)

	// Floor at 50ms — nothing useful happens faster than that in Lambda.
	return math.Max(avg, 50.0)
}
