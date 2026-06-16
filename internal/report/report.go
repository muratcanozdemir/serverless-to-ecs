package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/model"
)

// Options configures the report generator.
type Options struct {
	LLMEndpoint string // OpenAI-compatible base URL (e.g. http://localhost:8080/v1)
	LLMModel    string // model name (empty = server default)
	OutputDir   string
	Region      string
}

// Context is the structured data payload sent to the LLM.
type Context struct {
	Stack       StackOverview          `json:"stack"`
	Lambdas     []LambdaDetail         `json:"lambdas"`
	APIs        []APIDetail            `json:"apis"`
	StepFuncs   []StepFuncDetail       `json:"step_functions"`
	Rules       []RuleDetail           `json:"eventbridge_rules"`
	Queues      []QueueDetail          `json:"sqs_queues"`
	Tables      []TableDetail          `json:"dynamodb_tables"`
	Cost        *cost.Estimate         `json:"cost_estimate"`
	Groups      []cost.ServiceGroup    `json:"service_groups"`
	Unsupported []model.UnsupportedResource `json:"unsupported_resources"`
}

type StackOverview struct {
	Description string `json:"description"`
	IsSAM       bool   `json:"is_sam"`
	Region      string `json:"region"`
	Counts      map[string]int `json:"resource_counts"`
}

type LambdaDetail struct {
	LogicalID    string            `json:"logical_id"`
	FunctionName string            `json:"function_name"`
	Runtime      string            `json:"runtime"`
	MemoryMB     int               `json:"memory_mb"`
	TimeoutSec   int               `json:"timeout_sec"`
	EnvVars      map[string]string `json:"env_vars,omitempty"`
}

type APIDetail struct {
	LogicalID string `json:"logical_id"`
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Routes    []RouteDetail `json:"routes"`
}

type RouteDetail struct {
	Path   string `json:"path"`
	Method string `json:"method"`
	Target string `json:"target_lambda"`
}

type StepFuncDetail struct {
	LogicalID   string                 `json:"logical_id"`
	Name        string                 `json:"name"`
	Pattern     string                 `json:"pattern"`
	StateCount  int                    `json:"state_count"`
	TaskTargets []string               `json:"task_targets"`
	Definition  map[string]interface{} `json:"asl_definition,omitempty"`
}

type RuleDetail struct {
	LogicalID string `json:"logical_id"`
	Name      string `json:"name"`
	Schedule  string `json:"schedule"`
	Targets   []string `json:"targets"`
}

type QueueDetail struct {
	LogicalID string `json:"logical_id"`
	QueueName string `json:"queue_name"`
	FIFO      bool   `json:"fifo"`
}

type TableDetail struct {
	LogicalID   string `json:"logical_id"`
	TableName   string `json:"table_name"`
	BillingMode string `json:"billing_mode"`
	HashKey     string `json:"hash_key"`
	RangeKey    string `json:"range_key,omitempty"`
	GSICount    int    `json:"gsi_count"`
}

// Generate produces the migration report. Tries LLM if endpoint is configured,
// falls back to bare data dump otherwise.
func Generate(g *model.Graph, est *cost.Estimate, groups []cost.ServiceGroup, opts Options) error {
	ctx := buildContext(g, est, groups, opts.Region)

	if err := os.MkdirAll(opts.OutputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	outPath := filepath.Join(opts.OutputDir, "report.md")

	if opts.LLMEndpoint != "" {
		report, err := generateWithLLM(ctx, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: LLM generation failed: %v\nfalling back to data dump\n", err)
			return writeFallback(ctx, outPath)
		}
		return os.WriteFile(outPath, []byte(report), 0644)
	}

	return writeFallback(ctx, outPath)
}

// buildContext assembles the structured data payload from the graph.
func buildContext(g *model.Graph, est *cost.Estimate, groups []cost.ServiceGroup, region string) *Context {
	ctx := &Context{
		Stack: StackOverview{
			Description: g.Description,
			IsSAM:       g.IsSAM,
			Region:      region,
			Counts: map[string]int{
				"lambdas":     len(g.Lambdas),
				"apis":        len(g.APIs),
				"step_funcs":  len(g.StepFuncs),
				"rules":       len(g.Rules),
				"queues":      len(g.Queues),
				"topics":      len(g.Topics),
				"tables":      len(g.Tables),
				"unsupported": len(g.Unsupported),
			},
		},
		Cost:        est,
		Groups:      groups,
		Unsupported: g.Unsupported,
	}

	for _, fn := range g.Lambdas {
		ctx.Lambdas = append(ctx.Lambdas, LambdaDetail{
			LogicalID:    fn.LogicalID,
			FunctionName: fn.FunctionName,
			Runtime:      fn.Runtime,
			MemoryMB:     fn.MemoryMB,
			TimeoutSec:   fn.TimeoutSec,
			EnvVars:      fn.EnvVars,
		})
	}

	for apiID, api := range g.APIs {
		ad := APIDetail{
			LogicalID: apiID,
			Name:      api.Name,
			Protocol:  api.Protocol,
		}
		for _, route := range g.Routes {
			if route.APIID == apiID {
				ad.Routes = append(ad.Routes, RouteDetail{
					Path:   route.Path,
					Method: route.Method,
					Target: route.TargetRef,
				})
			}
		}
		ctx.APIs = append(ctx.APIs, ad)
	}

	for _, sf := range g.StepFuncs {
		ctx.StepFuncs = append(ctx.StepFuncs, StepFuncDetail{
			LogicalID:   sf.LogicalID,
			Name:        sf.Name,
			Pattern:     string(sf.Pattern),
			StateCount:  sf.StateCount,
			TaskTargets: sf.TaskTargets,
			Definition:  sf.DefinitionRaw,
		})
	}

	for _, rule := range g.Rules {
		ctx.Rules = append(ctx.Rules, RuleDetail{
			LogicalID: rule.LogicalID,
			Name:      rule.Name,
			Schedule:  rule.Schedule,
			Targets:   rule.TargetRefs,
		})
	}

	for _, q := range g.Queues {
		ctx.Queues = append(ctx.Queues, QueueDetail{
			LogicalID: q.LogicalID,
			QueueName: q.QueueName,
			FIFO:      q.FIFOQueue,
		})
	}

	for _, t := range g.Tables {
		ctx.Tables = append(ctx.Tables, TableDetail{
			LogicalID:   t.LogicalID,
			TableName:   t.TableName,
			BillingMode: t.BillingMode,
			HashKey:     t.HashKey,
			RangeKey:    t.RangeKey,
			GSICount:    t.GSICount,
		})
	}

	return ctx
}

// --- LLM generation ---

const systemPrompt = `You are a senior cloud infrastructure consultant writing a migration report for a client. Write in direct, technical prose. Use the provided analysis data — do not invent numbers. Format as markdown.`

const userPromptTemplate = `Write a serverless-to-ECS migration report based on this analysis:

<analysis>
%s
</analysis>

## Required sections

1. **Executive Summary** — 2-3 paragraphs. State the current architecture, the proposed target, estimated cost delta, and top risk.

2. **Current Architecture** — Describe the serverless stack: what services exist, how they connect, what patterns are used (API-driven, event-driven, orchestrated). Reference specific function names and resource counts.

3. **Cost Analysis** — Present the current serverless cost and projected ECS cost as markdown tables. Show per-service breakdown. Note the accuracy band and whether estimates are heuristic or data-backed.

4. **Proposed ECS Architecture** — Describe each proposed service group: what it contains, why those functions were grouped, what Fargate task size it maps to. Explain the strangler migration pattern.

5. **Step Functions Migration** — For each state machine, describe the detected pattern. Then write Go pseudocode showing how the same workflow would look as imperative code with explicit error handling. This is illustrative, not production-ready.

6. **Migration Plan** — Phased approach:
   - Phase 1: Deploy ECS infrastructure (dormant services)
   - Phase 2: Migrate API routes (one service group at a time)
   - Phase 3: Migrate queue processors
   - Phase 4: Migrate scheduled tasks
   - Phase 5: Decommission serverless resources
   For each phase, name the specific services and resources involved.

7. **Risk Assessment** — Flag: unsupported resources that need manual review, Step Functions complexity, IAM permission migration, cold start vs. warm container tradeoffs, operational monitoring gaps.

Do not include a title — the report framework adds one. Start directly with the Executive Summary.`

func generateWithLLM(ctx *Context, opts Options) (string, error) {
	ctxJSON, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal context: %w", err)
	}

	userPrompt := fmt.Sprintf(userPromptTemplate, string(ctxJSON))

	reqBody := map[string]interface{}{
		"messages": []map[string]string{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": 0.3,
		"max_tokens":  8192,
	}
	if opts.LLMModel != "" {
		reqBody["model"] = opts.LLMModel
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	endpoint := strings.TrimRight(opts.LLMEndpoint, "/") + "/chat/completions"
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("LLM request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("LLM returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode LLM response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", fmt.Errorf("LLM returned no choices")
	}

	header := fmt.Sprintf("# Serverless-to-ECS Migration Report\n\n"+
		"**Stack:** %s\n"+
		"**Region:** %s\n"+
		"**Generated:** %s\n"+
		"**Accuracy:** %s\n\n---\n\n",
		ctx.Stack.Description, ctx.Stack.Region,
		time.Now().UTC().Format("2006-01-02"), ctx.Cost.Accuracy)

	return header + result.Choices[0].Message.Content, nil
}

// --- Fallback: bare data dump ---

func writeFallback(ctx *Context, outPath string) error {
	var b strings.Builder

	b.WriteString("# Serverless-to-ECS Migration Report\n\n")
	b.WriteString(fmt.Sprintf("**Stack:** %s\n", ctx.Stack.Description))
	b.WriteString(fmt.Sprintf("**Region:** %s\n", ctx.Stack.Region))
	b.WriteString(fmt.Sprintf("**Generated:** %s\n", time.Now().UTC().Format("2006-01-02")))
	b.WriteString(fmt.Sprintf("**Accuracy:** %s\n", ctx.Cost.Accuracy))
	b.WriteString(fmt.Sprintf("**Mode:** data-only (no LLM endpoint configured)\n\n"))
	b.WriteString("---\n\n")

	// Inventory.
	b.WriteString("## Resource Inventory\n\n")
	b.WriteString("| Resource Type | Count |\n|---|---|\n")
	for k, v := range ctx.Stack.Counts {
		b.WriteString(fmt.Sprintf("| %s | %d |\n", k, v))
	}
	b.WriteString("\n")

	// Lambda details.
	if len(ctx.Lambdas) > 0 {
		b.WriteString("## Lambda Functions\n\n")
		b.WriteString("| Function | Runtime | Memory | Timeout |\n|---|---|---|---|\n")
		for _, fn := range ctx.Lambdas {
			b.WriteString(fmt.Sprintf("| %s | %s | %d MB | %d s |\n",
				fn.FunctionName, fn.Runtime, fn.MemoryMB, fn.TimeoutSec))
		}
		b.WriteString("\n")
	}

	// Step Functions.
	if len(ctx.StepFuncs) > 0 {
		b.WriteString("## Step Functions\n\n")
		for _, sf := range ctx.StepFuncs {
			b.WriteString(fmt.Sprintf("### %s\n\n", sf.Name))
			b.WriteString(fmt.Sprintf("Pattern: **%s** (%d states, %d task targets)\n\n",
				sf.Pattern, sf.StateCount, len(sf.TaskTargets)))
			if sf.Definition != nil {
				asl, _ := json.MarshalIndent(sf.Definition, "", "  ")
				b.WriteString("ASL definition:\n\n```json\n")
				b.Write(asl)
				b.WriteString("\n```\n\n")
			}
		}
	}

	// Cost comparison.
	b.WriteString("## Cost Comparison\n\n")
	b.WriteString("### Current Serverless (monthly)\n\n")
	if len(ctx.Cost.Serverless.Lambda) > 0 {
		b.WriteString("| Function | Invocations | GB-seconds | Cost |\n|---|---|---|---|\n")
		for _, lc := range ctx.Cost.Serverless.Lambda {
			b.WriteString(fmt.Sprintf("| %s | %d | %.0f | $%.2f |\n",
				lc.FunctionName, lc.Invocations, lc.GBSeconds, lc.Total))
		}
		b.WriteString("\n")
	}
	for _, label := range []struct {
		name  string
		items []cost.ItemCost
	}{
		{"API Gateway", ctx.Cost.Serverless.APIGateway},
		{"Step Functions", ctx.Cost.Serverless.StepFunctions},
		{"EventBridge", ctx.Cost.Serverless.EventBridge},
		{"SQS", ctx.Cost.Serverless.SQS},
		{"DynamoDB", ctx.Cost.Serverless.DynamoDB},
	} {
		if len(label.items) > 0 {
			b.WriteString(fmt.Sprintf("**%s:**\n\n", label.name))
			for _, item := range label.items {
				b.WriteString(fmt.Sprintf("- %s: $%.2f (%s)\n", item.Name, item.Total, item.Detail))
			}
			b.WriteString("\n")
		}
	}
	b.WriteString(fmt.Sprintf("**Total serverless: $%.2f/month**\n\n", ctx.Cost.Serverless.Total))

	b.WriteString("### Projected ECS (monthly)\n\n")
	if len(ctx.Cost.ECS.Services) > 0 {
		b.WriteString("| Service | Tasks | vCPU | Memory | Cost |\n|---|---|---|---|---|\n")
		for _, svc := range ctx.Cost.ECS.Services {
			b.WriteString(fmt.Sprintf("| %s | %d | %.2f | %.1f GB | $%.2f |\n",
				svc.Name, svc.Tasks, svc.VCPUs, svc.MemoryGB, svc.Total))
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("- ALB: $%.2f\n", ctx.Cost.ECS.ALB))
	b.WriteString(fmt.Sprintf("- Retained (DynamoDB, SQS, SNS): $%.2f\n", ctx.Cost.ECS.Retained))
	b.WriteString(fmt.Sprintf("\n**Total ECS: $%.2f/month**\n\n", ctx.Cost.ECS.Total))

	delta := ctx.Cost.Savings.DeltaAbsolute
	pct := ctx.Cost.Savings.DeltaPercent
	if delta > 0 {
		b.WriteString(fmt.Sprintf("**Estimated savings: $%.2f/month (%.1f%%)**\n\n", delta, pct))
	} else {
		b.WriteString(fmt.Sprintf("**Estimated cost increase: $%.2f/month (%.1f%%)**\n\n", -delta, -pct))
	}

	// Service groups.
	if len(ctx.Groups) > 0 {
		b.WriteString("## Proposed Service Grouping\n\n")
		b.WriteString("| Service | Type | Functions | Reason |\n|---|---|---|---|\n")
		for _, grp := range ctx.Groups {
			b.WriteString(fmt.Sprintf("| %s | %s | %s | %s |\n",
				grp.Name, grp.Type, strings.Join(grp.LambdaIDs, ", "), grp.Reason))
		}
		b.WriteString("\n")
	}

	// Unsupported.
	if len(ctx.Unsupported) > 0 {
		b.WriteString("## Unsupported Resources (manual review required)\n\n")
		for _, u := range ctx.Unsupported {
			b.WriteString(fmt.Sprintf("- **%s** (%s)\n", u.LogicalID, u.ResourceType))
		}
		b.WriteString("\n")
	}

	// Terraform manifest.
	b.WriteString("## Generated Terraform Files\n\n")
	b.WriteString("- `main.tf` — Provider, IAM roles, security groups\n")
	b.WriteString("- `variables.tf` — Input variables, migration toggles\n")
	b.WriteString("- `ecs.tf` — Cluster, task definitions, services\n")
	b.WriteString("- `alb.tf` — Application Load Balancer, strangler routing\n")
	b.WriteString("- `scheduled.tf` — ECS scheduled tasks (replaces EventBridge→Lambda)\n")
	b.WriteString("- `sqs_pollers.tf` — Queue consumer documentation\n")
	b.WriteString("- `outputs.tf` — ALB DNS, cluster ARN\n")

	return os.WriteFile(outPath, []byte(b.String()), 0644)
}
