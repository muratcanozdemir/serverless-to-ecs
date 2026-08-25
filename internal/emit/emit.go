package emit

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	Schedules         []ScheduleData
	QueuePollers      []QueuePollerData
	KinesisConsumers  []KinesisConsumerData
	S3EventProcessors []S3EventProcessorData

	EnvVarSummary []EnvVarHint
}

// ServiceData is a service group enriched with Terraform-specific fields.
type ServiceData struct {
	Name         string
	Type         string
	LambdaIDs    []string
	Reason       string
	CPU          int // Fargate CPU units (256, 512, 1024, 2048, 4096)
	Memory       int // Fargate memory in MB
	DesiredCount int
	EnvVars      []EnvVar
	Secrets      []SecretEnvVar // env vars backed by Secrets Manager/SSM — go in ECS's native "secrets" field
	EFSMounts    []EFSMountData
	VPCNote      string // non-empty if a source Lambda ran in a VPC — documentation only, see vpcNote
}

// APIServiceData extends ServiceData with ALB routing info.
type APIServiceData struct {
	Name     string
	TGName   string // target group name, pre-truncated to AWS's 32-char limit
	Priority int
	Paths    []string
}

// ScheduleData represents an EventBridge cron migrated to ECS scheduled task.
type ScheduleData struct {
	Name          string
	Schedule      string
	SourceLambdas []string
	TaskDefRef    string
	Resolved      bool // false if TaskDefRef couldn't be matched to a generated task definition
}

// QueuePollerData represents an SQS→Lambda mapping migrated to an ECS poller.
type QueuePollerData struct {
	QueueName     string
	ServiceName   string
	SourceLambdas []string
	Resolved      bool // false if ServiceName couldn't be matched to a generated service group
}

// KinesisConsumerData represents a Kinesis stream→Lambda mapping migrated to
// an ECS consumer (the container polls the stream via the AWS SDK, the same
// way a QueuePollerData's container polls SQS).
type KinesisConsumerData struct {
	StreamName    string
	ServiceName   string
	SourceLambdas []string
	Resolved      bool
}

// S3EventProcessorData represents an S3 bucket→Lambda notification migrated
// to ECS. Unlike SQS/Kinesis, S3 can't invoke a long-running container
// directly — the bucket needs to be reconfigured to publish to EventBridge
// or SQS instead, which this file documents as a manual step.
type S3EventProcessorData struct {
	BucketName    string
	ServiceName   string
	SourceLambdas []string
	Resolved      bool
}

// EnvVar is a key-value pair for container environment. KeyQuoted/ValueQuoted
// hold the Go-quoted (strconv.Quote) form so templates can interpolate them
// into HCL string literals without needing to re-escape user-controlled
// content (quotes, backslashes, newlines) themselves.
type EnvVar struct {
	Key         string
	Value       string
	KeyQuoted   string
	ValueQuoted string
}

// SecretEnvVar is an environment variable whose value comes from Secrets
// Manager or SSM Parameter Store — rendered into ECS's native "secrets"
// container field (valueFrom an ARN) rather than "environment", so the
// value is injected at task start instead of baked into the task definition.
type SecretEnvVar struct {
	Key        string // container env var name
	KeyQuoted  string // Go/HCL-quoted form of Key
	VarName    string // Terraform variable identifier holding the secret's ARN
	Kind       string // "secretsmanager" or "ssm"
	SourceHint string // logical ID or raw dynamic-reference string, for documentation
}

// EFSMountData is one EFS access point mounted into an ECS task. The file
// system itself isn't created by this Terraform (it's existing,
// retained infrastructure), so FileSystemVar/AccessPointVar are
// placeholder variables for the real IDs.
type EFSMountData struct {
	VolumeName     string
	FileSystemVar  string
	AccessPointVar string
	ContainerPath  string
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
		{"templates/kinesis_consumers.tf.tmpl", "kinesis_consumers.tf"},
		{"templates/s3_event_processors.tf.tmpl", "s3_event_processors.tf"},
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
			Secrets:      collectSecrets(g, grp.Name, grp.LambdaIDs),
			EFSMounts:    collectEFSMounts(g, grp.Name, grp.LambdaIDs),
			VPCNote:      vpcNote(g, grp.LambdaIDs),
		}
		data.Services = append(data.Services, svc)

		if grp.Type == "api" {
			paths := collectAPIPaths(g, grp.LambdaIDs)
			if len(paths) == 0 {
				paths = []string{"/*"}
			}
			data.APIServices = append(data.APIServices, APIServiceData{
				Name:     grp.Name,
				TGName:   truncateName(grp.Name, 32),
				Priority: priority,
				Paths:    paths,
			})
			priority += 10
			data.HasAPIServices = true
		}
	}

	// Schedules: from EventBridge rules and SAM schedule events.
	for _, ruleID := range sortedKeys(g.Rules) {
		rule := g.Rules[ruleID]
		if rule.Schedule == "" {
			continue
		}
		lambdas := findTriggeredLambdas(g, ruleID)
		taskDef, resolved := findServiceForLambdas(groups, lambdas)
		data.Schedules = append(data.Schedules, ScheduleData{
			Name:          sanitize(rule.Name),
			Schedule:      rule.Schedule,
			SourceLambdas: lambdas,
			TaskDefRef:    taskDef,
			Resolved:      resolved,
		})
	}

	// Queue pollers: from SQS→Lambda edges.
	for _, queueID := range sortedKeys(g.Queues) {
		q := g.Queues[queueID]
		lambdas := findTriggeredLambdas(g, queueID)
		if len(lambdas) == 0 {
			continue
		}
		svcName, resolved := findServiceForLambdas(groups, lambdas)
		data.QueuePollers = append(data.QueuePollers, QueuePollerData{
			QueueName:     q.QueueName,
			ServiceName:   svcName,
			SourceLambdas: lambdas,
			Resolved:      resolved,
		})
	}

	// Kinesis consumers: from stream→Lambda edges.
	for _, streamID := range sortedKeys(g.Streams) {
		stream := g.Streams[streamID]
		lambdas := findTriggeredLambdas(g, streamID)
		if len(lambdas) == 0 {
			continue
		}
		svcName, resolved := findServiceForLambdas(groups, lambdas)
		data.KinesisConsumers = append(data.KinesisConsumers, KinesisConsumerData{
			StreamName:    stream.StreamName,
			ServiceName:   svcName,
			SourceLambdas: lambdas,
			Resolved:      resolved,
		})
	}

	// S3 event processors: from bucket→Lambda notification edges.
	for _, bucketID := range sortedKeys(g.Buckets) {
		bucket := g.Buckets[bucketID]
		lambdas := findTriggeredLambdas(g, bucketID)
		if len(lambdas) == 0 {
			continue
		}
		svcName, resolved := findServiceForLambdas(groups, lambdas)
		data.S3EventProcessors = append(data.S3EventProcessors, S3EventProcessorData{
			BucketName:    bucket.BucketName,
			ServiceName:   svcName,
			SourceLambdas: lambdas,
			Resolved:      resolved,
		})
	}

	// Env var hints for IAM TODOs. These are emitted as raw text inside a
	// "#"-comment line in main.tf.tmpl, so any newline must be stripped —
	// otherwise a value containing "\n" could break out of the comment and
	// inject arbitrary Terraform.
	for _, fnID := range sortedKeys(g.Lambdas) {
		fn := g.Lambdas[fnID]
		if len(fn.EnvVars) > 0 {
			hints := make([]string, 0, len(fn.EnvVars))
			for _, k := range sortedKeys(fn.EnvVars) {
				hints = append(hints, singleLine(k)+"="+singleLine(fn.EnvVars[k]))
			}
			data.EnvVarSummary = append(data.EnvVarSummary, EnvVarHint{
				FunctionName: singleLine(fn.FunctionName),
				Hint:         strings.Join(hints, ", "),
			})
		}
	}

	return data
}

// --- Helpers ---

// sortedKeys returns a map's keys in sorted order — deterministic iteration
// is load-bearing here, since it determines the order resources are written
// to the generated .tf files. Without it, the exact same input template
// produces differently-ordered (though semantically equivalent) Terraform on
// every run, which is disruptive when the output is checked into version
// control and regenerated in CI.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// tfIdent converts a name to a valid Terraform identifier (underscores, no hyphens).
func tfIdent(s string) string {
	return strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(s)
}

// truncateName cuts s to at most maxLen runes — used for AWS resource names
// with hard length limits (e.g. an ALB target group name is capped at 32
// chars). Truncates by rune, not byte, so multi-byte UTF-8 input isn't split
// mid-character.
func truncateName(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

// singleLine collapses newlines so a value is safe to embed in a single-line
// "#"-comment in generated Terraform, where a literal newline would otherwise
// terminate the comment and let the rest of the value become live HCL.
func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\r", " ")
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
// Secret-backed env vars (see fn.SecretRefs) are excluded — those go into
// ECS's native "secrets" container field instead; see collectSecrets.
func collectEnvVars(g *model.Graph, lambdaIDs []string) []EnvVar {
	seen := make(map[string]bool)
	var vars []EnvVar
	for _, id := range lambdaIDs {
		fn, ok := g.Lambdas[id]
		if !ok {
			continue
		}
		for _, k := range sortedKeys(fn.EnvVars) {
			if seen[k] {
				continue
			}
			seen[k] = true
			if _, isSecret := fn.SecretRefs[k]; isSecret {
				continue
			}
			v := fn.EnvVars[k]
			// Replace CFN references with Terraform-style placeholders.
			if strings.HasPrefix(v, "${") {
				v = "TODO_" + strings.TrimSuffix(strings.TrimPrefix(v, "${"), "}")
			}
			vars = append(vars, EnvVar{
				Key:         k,
				Value:       v,
				KeyQuoted:   strconv.Quote(k),
				ValueQuoted: strconv.Quote(v),
			})
		}
	}
	return vars
}

// collectSecrets gathers env vars across the group's Lambdas that are backed
// by Secrets Manager/SSM Parameter Store. Each gets a placeholder Terraform
// variable for its ARN, since the actual ARN isn't derivable from the CFN
// template alone (it's assigned at deploy time, or the secret/parameter
// isn't even a resource in this template — see the dynamic-reference case
// in detectDynamicSecretRefs).
func collectSecrets(g *model.Graph, serviceName string, lambdaIDs []string) []SecretEnvVar {
	seen := make(map[string]bool)
	var secrets []SecretEnvVar
	for _, id := range lambdaIDs {
		fn, ok := g.Lambdas[id]
		if !ok {
			continue
		}
		for _, k := range sortedKeys(fn.SecretRefs) {
			if seen[k] {
				continue
			}
			seen[k] = true
			ref := fn.SecretRefs[k]
			hint := ref.LogicalID
			if hint == "" {
				hint = ref.RawRef
			}
			secrets = append(secrets, SecretEnvVar{
				Key:        k,
				KeyQuoted:  strconv.Quote(k),
				VarName:    tfIdent(serviceName) + "_" + tfIdent(strings.ToLower(k)) + "_secret_arn",
				Kind:       ref.Kind,
				SourceHint: hint,
			})
		}
	}
	return secrets
}

// collectEFSMounts gathers unique (access point, mount path) pairs across the
// group's Lambdas and assigns each a stable ECS volume name, plus
// placeholder Terraform variables for the EFS file system/access point IDs
// — the file system isn't created by this tool (it's retained, pre-existing
// infrastructure), so the real IDs must be supplied.
func collectEFSMounts(g *model.Graph, serviceName string, lambdaIDs []string) []EFSMountData {
	type mountKey struct{ accessPoint, path string }
	seen := make(map[mountKey]bool)
	var mounts []EFSMountData
	idx := 0
	for _, id := range lambdaIDs {
		fn, ok := g.Lambdas[id]
		if !ok {
			continue
		}
		for _, m := range fn.EFSMounts {
			k := mountKey{m.AccessPointRef, m.LocalMountPath}
			if seen[k] {
				continue
			}
			seen[k] = true
			base := fmt.Sprintf("%s_efs_%d", tfIdent(serviceName), idx)
			mounts = append(mounts, EFSMountData{
				VolumeName:     fmt.Sprintf("efs-%d", idx),
				FileSystemVar:  base + "_file_system_id",
				AccessPointVar: base + "_access_point_id",
				ContainerPath:  m.LocalMountPath,
			})
			idx++
		}
	}
	return mounts
}

// vpcNote summarizes which Lambdas in a group ran inside a VPC, for a
// documentation comment on the generated ECS service. Subnet/security group
// refs are frequently unresolvable to concrete values from the CFN template
// alone (parameters, imports, etc.), so this can't be wired into Terraform
// automatically — it just tells the reader that VPC connectivity parity
// needs to be verified.
func vpcNote(g *model.Graph, lambdaIDs []string) string {
	var parts []string
	for _, id := range lambdaIDs {
		fn, ok := g.Lambdas[id]
		if !ok || fn.VPCConfig == nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (subnets: %s; security groups: %s)",
			id, strings.Join(fn.VPCConfig.SubnetRefs, ", "), strings.Join(fn.VPCConfig.SecurityGroupRefs, ", ")))
	}
	return strings.Join(parts, "; ")
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

// findServiceForLambdas looks up the generated service group name that owns
// any of the given Lambdas. The returned bool reports whether a match was
// found in groups — callers must not reference a Terraform resource built
// from this name unless it's true, since an unresolved name doesn't
// correspond to any task definition/service actually emitted.
func findServiceForLambdas(groups []cost.ServiceGroup, lambdaIDs []string) (string, bool) {
	idSet := make(map[string]bool)
	for _, id := range lambdaIDs {
		idSet[id] = true
	}
	for _, grp := range groups {
		for _, id := range grp.LambdaIDs {
			if idSet[id] {
				return grp.Name, true
			}
		}
	}
	return "unknown", false
}

func deriveProjectName(g *model.Graph) string {
	if g.Description != "" {
		words := strings.Fields(g.Description)
		if len(words) > 0 {
			name := truncateName(strings.ToLower(words[0]), 20)
			return sanitize(name)
		}
	}
	return "migrated-stack"
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	return strings.NewReplacer(" ", "-", "_", "-", ".", "-").Replace(s)
}
