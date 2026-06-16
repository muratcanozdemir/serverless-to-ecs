package emit

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/model"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// TerraformData is the top-level data passed to all templates.
type TerraformData struct {
	TemplatePath string
	GeneratedAt  string
	Region       string
	ProjectName  string

	Services       []ServiceData
	APIServices    []APIServiceData
	HasAPIServices bool

	Schedules    []ScheduleData
	QueuePollers []QueuePollerData

	EnvVarSummary []EnvVarHint
}

// ServiceData is a service group enriched with Terraform-specific fields.
type ServiceData struct {
	Name         string
	Type         string
	LambdaIDs    []string
	Reason       string
	CPU          int    // Fargate CPU units (256, 512, 1024, 2048, 4096)
	Memory       int    // Fargate memory in MB
	DesiredCount int
	EnvVars      []EnvVar
}

// APIServiceData extends ServiceData with ALB routing info.
type APIServiceData struct {
	Name     string
	Priority int
	Paths    []string
}

// ScheduleData represents an EventBridge cron migrated to ECS scheduled task.
type ScheduleData struct {
	Name          string
	Schedule      string
	SourceLambdas []string
	TaskDefRef    string
}

// QueuePollerData represents an SQS→Lambda mapping migrated to an ECS poller.
type QueuePollerData struct {
	QueueName     string
	ServiceName   string
	SourceLambdas []string
}

// EnvVar is a key-value pair for container environment.
type EnvVar struct {
	Key   string
	Value string
}

// EnvVarHint is used in the main.tf IAM TODO comments.
type EnvVarHint struct {
	FunctionName string
	Hint         string
}

// EmitTerraform generates all .tf files in the output directory.
func EmitTerraform(g *model.Graph, groups []cost.ServiceGroup, templatePath, region, outputDir string) error {
	data := assembleTerraformData(g, groups, templatePath, region)

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}

	funcMap := template.FuncMap{
		"tfident": tfIdent,
		"join":    strings.Join,
		"min":     minInt,
		"len":     strLen,
		"substr":  substr,
	}

	files := []struct {
		tmpl   string
		output string
	}{
		{"templates/main.tf.tmpl", "main.tf"},
		{"templates/variables.tf.tmpl", "variables.tf"},
		{"templates/ecs.tf.tmpl", "ecs.tf"},
		{"templates/alb.tf.tmpl", "alb.tf"},
		{"templates/scheduled.tf.tmpl", "scheduled.tf"},
		{"templates/sqs_pollers.tf.tmpl", "sqs_pollers.tf"},
		{"templates/outputs.tf.tmpl", "outputs.tf"},
	}

	for _, f := range files {
		tmplContent, err := templateFS.ReadFile(f.tmpl)
		if err != nil {
			return fmt.Errorf("read template %s: %w", f.tmpl, err)
		}

		tmpl, err := template.New(f.output).Funcs(funcMap).Parse(string(tmplContent))
		if err != nil {
			return fmt.Errorf("parse template %s: %w", f.tmpl, err)
		}

		outPath := filepath.Join(outputDir, f.output)
		out, err := os.Create(outPath)
		if err != nil {
			return fmt.Errorf("create %s: %w", outPath, err)
		}

		if err := tmpl.Execute(out, data); err != nil {
			out.Close()
			return fmt.Errorf("render %s: %w", f.tmpl, err)
		}
		out.Close()
	}

	return nil
}

// assembleTerraformData builds the template context from the graph and groupings.
func assembleTerraformData(g *model.Graph, groups []cost.ServiceGroup, templatePath, region string) *TerraformData {
	data := &TerraformData{
		TemplatePath: templatePath,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Region:       region,
		ProjectName:  deriveProjectName(g),
	}

	priority := 100
	for _, grp := range groups {
		svc := ServiceData{
			Name:         grp.Name,
			Type:         grp.Type,
			LambdaIDs:    grp.LambdaIDs,
			Reason:       grp.Reason,
			CPU:          fargateCPU(g, grp.LambdaIDs),
			Memory:       fargateMemory(g, grp.LambdaIDs),
			DesiredCount: defaultDesiredCount(grp.Type),
			EnvVars:      collectEnvVars(g, grp.LambdaIDs),
		}
		data.Services = append(data.Services, svc)

		if grp.Type == "api" {
			paths := collectAPIPaths(g, grp.LambdaIDs)
			if len(paths) == 0 {
				paths = []string{"/*"}
			}
			data.APIServices = append(data.APIServices, APIServiceData{
				Name:     grp.Name,
				Priority: priority,
				Paths:    paths,
			})
			priority += 10
			data.HasAPIServices = true
		}
	}

	// Schedules: from EventBridge rules and SAM schedule events.
	for ruleID, rule := range g.Rules {
		if rule.Schedule == "" {
			continue
		}
		lambdas := findTriggeredLambdas(g, ruleID)
		taskDef := findServiceForLambdas(groups, lambdas)
		data.Schedules = append(data.Schedules, ScheduleData{
			Name:          sanitize(rule.Name),
			Schedule:      rule.Schedule,
			SourceLambdas: lambdas,
			TaskDefRef:    taskDef,
		})
	}

	// Queue pollers: from SQS→Lambda edges.
	for queueID, q := range g.Queues {
		lambdas := findTriggeredLambdas(g, queueID)
		if len(lambdas) == 0 {
			continue
		}
		svcName := findServiceForLambdas(groups, lambdas)
		data.QueuePollers = append(data.QueuePollers, QueuePollerData{
			QueueName:     q.QueueName,
			ServiceName:   svcName,
			SourceLambdas: lambdas,
		})
	}

	// Env var hints for IAM TODOs.
	for _, fn := range g.Lambdas {
		if len(fn.EnvVars) > 0 {
			hints := make([]string, 0, len(fn.EnvVars))
			for k, v := range fn.EnvVars {
				hints = append(hints, k+"="+v)
			}
			data.EnvVarSummary = append(data.EnvVarSummary, EnvVarHint{
				FunctionName: fn.FunctionName,
				Hint:         strings.Join(hints, ", "),
			})
		}
	}

	return data
}

// --- Helpers ---

// tfIdent converts a name to a valid Terraform identifier (underscores, no hyphens).
func tfIdent(s string) string {
	return strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(s)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func strLen(s string) int {
	return len(s)
}

func substr(s string, start, end int) string {
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// fargateCPU returns Fargate CPU units based on the highest memory Lambda in the group.
func fargateCPU(g *model.Graph, lambdaIDs []string) int {
	maxMem := 0
	for _, id := range lambdaIDs {
		if fn, ok := g.Lambdas[id]; ok && fn.MemoryMB > maxMem {
			maxMem = fn.MemoryMB
		}
	}
	switch {
	case maxMem <= 512:
		return 256
	case maxMem <= 1024:
		return 512
	case maxMem <= 2048:
		return 1024
	case maxMem <= 4096:
		return 2048
	default:
		return 4096
	}
}

// fargateMemory returns Fargate memory in MB.
func fargateMemory(g *model.Graph, lambdaIDs []string) int {
	maxMem := 0
	for _, id := range lambdaIDs {
		if fn, ok := g.Lambdas[id]; ok && fn.MemoryMB > maxMem {
			maxMem = fn.MemoryMB
		}
	}
	switch {
	case maxMem <= 512:
		return 512
	case maxMem <= 1024:
		return 1024
	case maxMem <= 2048:
		return 2048
	case maxMem <= 4096:
		return 4096
	default:
		return 8192
	}
}

func defaultDesiredCount(serviceType string) int {
	switch serviceType {
	case "api":
		return 2
	case "queue-processor":
		return 1
	case "scheduled":
		return 0 // Scheduled tasks are run by EventBridge, not as a persistent service.
	default:
		return 1
	}
}

// collectEnvVars merges environment variables across all Lambdas in a group.
// Resource references (${...}) are replaced with placeholder comments.
func collectEnvVars(g *model.Graph, lambdaIDs []string) []EnvVar {
	seen := make(map[string]bool)
	var vars []EnvVar
	for _, id := range lambdaIDs {
		fn, ok := g.Lambdas[id]
		if !ok {
			continue
		}
		for k, v := range fn.EnvVars {
			if seen[k] {
				continue
			}
			seen[k] = true
			// Replace CFN references with Terraform-style placeholders.
			if strings.HasPrefix(v, "${") {
				v = "TODO_" + strings.TrimSuffix(strings.TrimPrefix(v, "${"), "}")
			}
			vars = append(vars, EnvVar{Key: k, Value: v})
		}
	}
	return vars
}

// collectAPIPaths finds all API Gateway route paths associated with the given Lambdas.
func collectAPIPaths(g *model.Graph, lambdaIDs []string) []string {
	idSet := make(map[string]bool)
	for _, id := range lambdaIDs {
		idSet[id] = true
	}

	seen := make(map[string]bool)
	var paths []string
	for _, route := range g.Routes {
		if !idSet[route.TargetRef] {
			continue
		}
		p := route.Path
		if p == "" {
			continue
		}
		// Add both exact and wildcard for path parameters.
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
		wild := p + "/*"
		if !seen[wild] {
			seen[wild] = true
			paths = append(paths, wild)
		}
	}
	return paths
}

func findTriggeredLambdas(g *model.Graph, sourceID string) []string {
	var lambdas []string
	for _, edge := range g.Edges {
		if edge.From == sourceID && (edge.Type == model.EdgeTriggers || edge.Type == model.EdgeOrchestrates) {
			if _, ok := g.Lambdas[edge.To]; ok {
				lambdas = append(lambdas, edge.To)
			}
		}
	}
	return lambdas
}

func findServiceForLambdas(groups []cost.ServiceGroup, lambdaIDs []string) string {
	idSet := make(map[string]bool)
	for _, id := range lambdaIDs {
		idSet[id] = true
	}
	for _, grp := range groups {
		for _, id := range grp.LambdaIDs {
			if idSet[id] {
				return grp.Name
			}
		}
	}
	if len(lambdaIDs) > 0 {
		return sanitize(lambdaIDs[0])
	}
	return "unknown"
}

func deriveProjectName(g *model.Graph) string {
	if g.Description != "" {
		words := strings.Fields(g.Description)
		if len(words) > 0 {
			name := strings.ToLower(words[0])
			if len(name) > 20 {
				name = name[:20]
			}
			return sanitize(name)
		}
	}
	return "migrated-stack"
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer(" ", "-", "_", "-", ".", "-").Replace(s)
}
