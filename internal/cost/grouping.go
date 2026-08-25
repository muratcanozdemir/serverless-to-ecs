package cost

import (
	"sort"
	"strings"

	"serverless-to-ecs/internal/model"
)

// ServiceGroup is a proposed ECS service consisting of one or more Lambdas.
type ServiceGroup struct {
	Name      string   `json:"name"`
	Type      string   `json:"type"` // "api", "queue-processor", "scheduled", "orchestrated", "standalone"
	LambdaIDs []string `json:"lambda_ids"`
	Reason    string   `json:"reason"` // why these were grouped
}

// GroupServices clusters Lambda functions into logical ECS services.
//
// Grouping heuristic (in priority order):
//  1. Shared API Gateway: all Lambdas behind the same API become one service.
//  2. Queue processors: Lambdas triggered by SQS/SNS group per queue/topic.
//  3. Scheduled: Lambdas triggered by EventBridge schedules group together.
//  4. Orchestrated: Lambdas called by the same Step Function group together.
//  5. Remaining: ungrouped Lambdas cluster by naming prefix similarity.
func GroupServices(g *model.Graph) []ServiceGroup {
	assigned := make(map[string]bool)
	var groups []ServiceGroup

	// 1. API-backed services.
	for _, apiID := range sortedKeys(g.APIs) {
		api := g.APIs[apiID]
		var members []string
		for _, route := range g.Routes {
			if route.APIID == apiID && !assigned[route.TargetRef] {
				if _, ok := g.Lambdas[route.TargetRef]; ok {
					members = append(members, route.TargetRef)
					assigned[route.TargetRef] = true
				}
			}
		}
		if len(members) > 0 {
			name := api.Name
			if name == "" {
				name = apiID
			}
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(name) + "-svc",
				Type:      "api",
				LambdaIDs: dedup(members),
				Reason:    "shared API Gateway: " + apiID,
			})
		}
	}

	// Also check for SAM implicit API edges.
	var implicitAPIMembers []string
	for _, edge := range g.Edges {
		if edge.From == "__implicit_api__" && !assigned[edge.To] {
			if _, ok := g.Lambdas[edge.To]; ok {
				implicitAPIMembers = append(implicitAPIMembers, edge.To)
				assigned[edge.To] = true
			}
		}
	}
	if len(implicitAPIMembers) > 0 {
		groups = append(groups, ServiceGroup{
			Name:      "api-svc",
			Type:      "api",
			LambdaIDs: dedup(implicitAPIMembers),
			Reason:    "SAM implicit API Gateway",
		})
	}

	// 2. Queue processors: Lambdas triggered by SQS.
	for _, queueID := range sortedKeys(g.Queues) {
		if members := triggeredMembers(g, queueID, assigned); len(members) > 0 {
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(queueID) + "-processor",
				Type:      "queue-processor",
				LambdaIDs: dedup(members),
				Reason:    "SQS trigger: " + queueID,
			})
		}
	}

	// SNS-triggered Lambdas.
	for _, topicID := range sortedKeys(g.Topics) {
		if members := triggeredMembers(g, topicID, assigned); len(members) > 0 {
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(topicID) + "-subscriber",
				Type:      "queue-processor",
				LambdaIDs: dedup(members),
				Reason:    "SNS trigger: " + topicID,
			})
		}
	}

	// Kinesis-triggered Lambdas. Grouped the same way as SQS: a stream
	// consumer is architecturally the same shape as a queue processor — a
	// long-running container polling/consuming a stream of events.
	for _, streamID := range sortedKeys(g.Streams) {
		if members := triggeredMembers(g, streamID, assigned); len(members) > 0 {
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(streamID) + "-consumer",
				Type:      "queue-processor",
				LambdaIDs: dedup(members),
				Reason:    "Kinesis trigger: " + streamID,
			})
		}
	}

	// S3-triggered Lambdas.
	for _, bucketID := range sortedKeys(g.Buckets) {
		if members := triggeredMembers(g, bucketID, assigned); len(members) > 0 {
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(bucketID) + "-processor",
				Type:      "queue-processor",
				LambdaIDs: dedup(members),
				Reason:    "S3 event trigger: " + bucketID,
			})
		}
	}

	// 3. Scheduled: Lambdas triggered by EventBridge rules.
	var scheduledMembers []string
	for _, ruleID := range sortedKeys(g.Rules) {
		for _, edge := range g.Edges {
			if edge.From == ruleID && edge.Type == model.EdgeTriggers && !assigned[edge.To] {
				if _, ok := g.Lambdas[edge.To]; ok {
					scheduledMembers = append(scheduledMembers, edge.To)
					assigned[edge.To] = true
				}
			}
		}
	}
	// Also catch SAM implicit schedules.
	for _, edge := range g.Edges {
		if strings.HasPrefix(edge.From, "__schedule_") && !assigned[edge.To] {
			if _, ok := g.Lambdas[edge.To]; ok {
				scheduledMembers = append(scheduledMembers, edge.To)
				assigned[edge.To] = true
			}
		}
	}
	if len(scheduledMembers) > 0 {
		groups = append(groups, ServiceGroup{
			Name:      "scheduled-tasks",
			Type:      "scheduled",
			LambdaIDs: dedup(scheduledMembers),
			Reason:    "EventBridge schedule triggers",
		})
	}

	// 4. Orchestrated: Lambdas called by Step Functions (not already assigned).
	for _, sfnID := range sortedKeys(g.StepFuncs) {
		sf := g.StepFuncs[sfnID]
		var members []string
		for _, target := range sf.TaskTargets {
			if !assigned[target] {
				if _, ok := g.Lambdas[target]; ok {
					members = append(members, target)
					assigned[target] = true
				}
			}
		}
		if len(members) > 0 {
			name := sf.Name
			if name == "" {
				name = sfnID
			}
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(name) + "-worker",
				Type:      "orchestrated",
				LambdaIDs: dedup(members),
				Reason:    "Step Functions orchestration: " + sfnID,
			})
		}
	}

	// 5. Remaining: group by naming prefix.
	var unassigned []string
	for _, id := range sortedKeys(g.Lambdas) {
		if !assigned[id] {
			unassigned = append(unassigned, id)
		}
	}
	if len(unassigned) > 0 {
		prefixGroups := groupByPrefix(unassigned)
		for _, prefix := range sortedKeys(prefixGroups) {
			members := prefixGroups[prefix]
			groups = append(groups, ServiceGroup{
				Name:      sanitizeName(prefix) + "-svc",
				Type:      "standalone",
				LambdaIDs: members,
				Reason:    "naming prefix: " + prefix,
			})
		}
	}

	return groups
}

// groupByPrefix clusters function logical IDs by their common prefix.
// Uses the longest common prefix among IDs, splitting on common delimiters.
func groupByPrefix(ids []string) map[string][]string {
	if len(ids) == 1 {
		return map[string][]string{ids[0]: ids}
	}

	groups := make(map[string][]string)
	for _, id := range ids {
		prefix := extractPrefix(id)
		groups[prefix] = append(groups[prefix], id)
	}
	return groups
}

// extractPrefix returns the first segment of a logical ID, splitting on
// common naming patterns (CamelCase boundary before Fn/Function/Lambda/Handler).
func extractPrefix(id string) string {
	// Try splitting on common suffixes.
	for _, suffix := range []string{"Fn", "Function", "Lambda", "Handler", "Worker"} {
		if idx := strings.LastIndex(id, suffix); idx > 0 {
			return id[:idx]
		}
	}
	// Try splitting on hyphens/underscores.
	for _, sep := range []string{"-", "_"} {
		parts := strings.Split(id, sep)
		if len(parts) > 1 {
			return parts[0]
		}
	}
	return id
}

func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(
		" ", "-",
		"_", "-",
		".", "-",
	).Replace(s)
	return s
}

// triggeredMembers finds Lambdas triggered by sourceID (via an EdgeTriggers
// edge) that haven't already been assigned to a group, marking each as
// assigned as it's found.
func triggeredMembers(g *model.Graph, sourceID string, assigned map[string]bool) []string {
	var members []string
	for _, edge := range g.Edges {
		if edge.From == sourceID && edge.Type == model.EdgeTriggers && !assigned[edge.To] {
			if _, ok := g.Lambdas[edge.To]; ok {
				members = append(members, edge.To)
				assigned[edge.To] = true
			}
		}
	}
	return members
}

// sortedKeys returns a map's keys in sorted order, so callers get a
// deterministic iteration order instead of Go's randomized map order —
// load-bearing here since iteration order determines the order of generated
// service groups (and downstream, ALB listener priorities and file layout).
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dedup(items []string) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}
