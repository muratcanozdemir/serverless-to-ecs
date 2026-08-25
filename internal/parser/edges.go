package parser

import (
	"sort"
	"strings"

	"serverless-to-ecs/internal/model"
)

// sortedStringKeys returns a map's keys in sorted order. Several CFN
// properties (SAM Events, env var Variables) are unordered JSON objects, but
// the order in which they're processed here determines the order of edges
// added to the graph — and downstream, the order of generated Terraform
// resources — so iterating in Go's randomized map order would make output
// non-reproducible between runs of the same template.
func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resolveEdges examines a resource's properties for references to other resources
// and creates typed edges in the graph. Called once per resource in the second pass.
func resolveEdges(g *model.Graph, logicalID string, res RawResource) {
	switch res.Type {

	case "AWS::Lambda::EventSourceMapping":
		resolveEventSourceMapping(g, logicalID, res.Properties)

	case "AWS::ApiGateway::Method":
		resolveAPIGatewayMethod(g, logicalID, res.Properties)

	case "AWS::ApiGatewayV2::Route":
		resolveHTTPAPIRoute(g, logicalID, res.Properties)

	case "AWS::Events::Rule":
		resolveEventBridgeTargets(g, logicalID)

	case "AWS::StepFunctions::StateMachine":
		resolveStepFunctionTargets(g, logicalID)

	case "AWS::SNS::Subscription":
		resolveSNSSubscription(g, res.Properties)

	case "AWS::S3::Bucket":
		resolveS3BucketNotifications(g, logicalID, res.Properties)
	}

	// For any Lambda, check env vars for table/secret references and EFS mounts.
	if res.Type == "AWS::Lambda::Function" || res.Type == "AWS::Serverless::Function" {
		resolveLambdaEnvRefs(g, logicalID, res.Properties)
		resolveLambdaEFSRefs(g, logicalID)
	}
}

// resolveEventSourceMapping handles AWS::Lambda::EventSourceMapping.
// These wire SQS queues (or DynamoDB streams, Kinesis) to Lambda functions.
//
// Known limitation: only Ref/Fn::GetAtt forms of FunctionName/EventSourceArn are
// resolved. A literal ARN string or an Fn::ImportValue (common when the queue or
// function is defined in a different stack) has no logical ID in this template's
// graph to link to, so the trigger edge is silently skipped rather than guessed at.
func resolveEventSourceMapping(g *model.Graph, _ string, props map[string]interface{}) {
	fnRef := resolveRef(props["FunctionName"])
	sourceRef := resolveRef(props["EventSourceArn"])

	if fnRef == "" || sourceRef == "" {
		return
	}

	// Determine edge type based on what the source is.
	if _, ok := g.Queues[sourceRef]; ok {
		g.AddEdge(sourceRef, fnRef, model.EdgeTriggers, "SQS event source")
	} else if _, ok := g.Tables[sourceRef]; ok {
		g.AddEdge(sourceRef, fnRef, model.EdgeTriggers, "DynamoDB stream")
	} else if _, ok := g.Streams[sourceRef]; ok {
		g.AddEdge(sourceRef, fnRef, model.EdgeTriggers, "Kinesis stream")
	} else {
		// Some other source we don't model.
		g.AddEdge(sourceRef, fnRef, model.EdgeTriggers, "event source mapping")
	}
}

// resolveS3BucketNotifications handles native (non-SAM) S3 → Lambda event
// notifications configured directly on the bucket's NotificationConfiguration.
func resolveS3BucketNotifications(g *model.Graph, bucketID string, props map[string]interface{}) {
	notif, ok := props["NotificationConfiguration"].(map[string]interface{})
	if !ok {
		return
	}
	configs, ok := notif["LambdaConfigurations"].([]interface{})
	if !ok {
		return
	}
	for _, c := range configs {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		fnRef := resolveRef(cm["Function"])
		if fnRef == "" {
			continue
		}
		event := getString(cm, "Event")
		g.AddEdge(bucketID, fnRef, model.EdgeTriggers, "S3 notification: "+event)
	}
}

// resolveAPIGatewayMethod handles REST API methods with Lambda integrations.
// The integration URI typically references the Lambda function.
func resolveAPIGatewayMethod(g *model.Graph, _ string, props map[string]interface{}) {
	// Find the parent REST API.
	apiRef := resolveRef(props["RestApiId"])
	method := getString(props, "HttpMethod")

	// Extract the resource path. In CFN this comes from AWS::ApiGateway::Resource,
	// but for simplicity we use the method's resource reference.
	resourceRef := resolveRef(props["ResourceId"])
	path := "/" + resourceRef // approximate — real path requires walking the resource tree

	// Check for Lambda integration.
	integration, ok := props["Integration"].(map[string]interface{})
	if !ok {
		return
	}

	intType := getString(integration, "Type")
	if intType != "AWS_PROXY" && intType != "AWS" {
		return
	}

	// Integration URI may contain a GetAtt or Sub referencing the Lambda.
	lambdaRef := extractLambdaFromIntegrationURI(integration["Uri"])
	if lambdaRef == "" {
		return
	}

	if apiRef != "" {
		g.Routes = append(g.Routes, model.APIRoute{
			APIID:     apiRef,
			Path:      path,
			Method:    method,
			TargetRef: lambdaRef,
		})
	}
	g.AddEdge(apiRef, lambdaRef, model.EdgeInvokes, method+" "+path)
}

// resolveHTTPAPIRoute handles HTTP API (v2) routes. A Route's Target property
// references an AWS::ApiGatewayV2::Integration (typically
// Fn::Sub: "integrations/${MyIntegration}"), not a Lambda directly, so it's
// resolved via g.HTTPIntegrations (populated during extraction from each
// Integration's IntegrationUri) to find the actual Lambda target.
func resolveHTTPAPIRoute(g *model.Graph, _ string, props map[string]interface{}) {
	apiRef := resolveRef(props["ApiId"])
	routeKey := getString(props, "RouteKey") // e.g., "GET /items"

	integrationID := extractIntegrationID(props["Target"])
	if integrationID == "" {
		return
	}
	lambdaRef, ok := g.HTTPIntegrations[integrationID]
	if !ok || lambdaRef == "" {
		return
	}

	if apiRef != "" && routeKey != "" {
		g.Routes = append(g.Routes, model.APIRoute{
			APIID:     apiRef,
			Path:      routeKey,
			Method:    "",
			TargetRef: lambdaRef,
		})
	}
	g.AddEdge(apiRef, lambdaRef, model.EdgeInvokes, routeKey)
}

// extractIntegrationID pulls the Integration logical ID out of a Route's Target
// property, which is a literal "integrations/{id}" string or (far more commonly)
// an Fn::Sub of the same shape, e.g. Fn::Sub: "integrations/${MyIntegration}".
func extractIntegrationID(target interface{}) string {
	switch v := target.(type) {
	case string:
		return strings.TrimPrefix(v, "integrations/")
	case map[string]interface{}:
		switch sub := v["Fn::Sub"].(type) {
		case string:
			return extractSubRef(sub)
		case []interface{}:
			if len(sub) > 0 {
				if s, ok := sub[0].(string); ok {
					return extractSubRef(s)
				}
			}
		}
	}
	return ""
}

// resolveEventBridgeTargets creates edges from EventBridge rules to their targets.
func resolveEventBridgeTargets(g *model.Graph, logicalID string) {
	rule, ok := g.Rules[logicalID]
	if !ok {
		return
	}
	for _, targetRef := range rule.TargetRefs {
		detail := "event rule"
		if rule.Schedule != "" {
			detail = "schedule: " + rule.Schedule
		}
		g.AddEdge(logicalID, targetRef, model.EdgeTriggers, detail)
	}
}

// resolveStepFunctionTargets creates edges from Step Functions to their task Lambdas.
func resolveStepFunctionTargets(g *model.Graph, logicalID string) {
	sf, ok := g.StepFuncs[logicalID]
	if !ok {
		return
	}
	for _, target := range sf.TaskTargets {
		g.AddEdge(logicalID, target, model.EdgeOrchestrates, "task state")
	}
}

// resolveSNSSubscription handles AWS::SNS::Subscription to find SNS → SQS or SNS → Lambda edges.
func resolveSNSSubscription(g *model.Graph, props map[string]interface{}) {
	topicRef := resolveRef(props["TopicArn"])
	endpointRef := resolveRef(props["Endpoint"])
	protocol := getString(props, "Protocol")

	if topicRef == "" || endpointRef == "" {
		return
	}

	switch protocol {
	case "lambda":
		g.AddEdge(topicRef, endpointRef, model.EdgeTriggers, "SNS subscription (lambda)")
	case "sqs":
		g.AddEdge(topicRef, endpointRef, model.EdgeSubscribes, "SNS subscription (sqs)")
	}
}

// resolveLambdaEnvRefs scans a Lambda's environment variables for references
// to DynamoDB tables, SQS queues, SNS topics, Secrets Manager secrets, and
// SSM parameters. When an env var resolves to a secret/parameter, it's also
// recorded on the Lambda's SecretRefs so the emitter can route it into ECS's
// native "secrets" container field instead of "environment".
func resolveLambdaEnvRefs(g *model.Graph, logicalID string, props map[string]interface{}) {
	env, ok := props["Environment"].(map[string]interface{})
	if !ok {
		return
	}
	vars, ok := env["Variables"].(map[string]interface{})
	if !ok {
		return
	}
	fn := g.Lambdas[logicalID]

	for _, k := range sortedStringKeys(vars) {
		ref := resolveRef(vars[k])
		if ref == "" {
			continue
		}
		if _, ok := g.Tables[ref]; ok {
			g.AddEdge(logicalID, ref, model.EdgeReadsWrites, "env var reference")
		} else if _, ok := g.Queues[ref]; ok {
			g.AddEdge(logicalID, ref, model.EdgePublishes, "env var reference")
		} else if _, ok := g.Topics[ref]; ok {
			g.AddEdge(logicalID, ref, model.EdgePublishes, "env var reference")
		} else if _, ok := g.Secrets[ref]; ok {
			g.AddEdge(logicalID, ref, model.EdgeReadsSecret, "env var reference")
			setSecretRef(fn, k, "secretsmanager", ref)
		} else if _, ok := g.Parameters[ref]; ok {
			g.AddEdge(logicalID, ref, model.EdgeReadsSecret, "env var reference")
			setSecretRef(fn, k, "ssm", ref)
		}
	}
}

// setSecretRef records that a Lambda's env var key resolves to a Secrets
// Manager secret or SSM parameter defined elsewhere in this template.
func setSecretRef(fn *model.Lambda, key, kind, logicalID string) {
	if fn == nil {
		return
	}
	if fn.SecretRefs == nil {
		fn.SecretRefs = make(map[string]model.SecretRef)
	}
	fn.SecretRefs[key] = model.SecretRef{Kind: kind, LogicalID: logicalID}
}

// resolveLambdaEFSRefs creates Lambda → EFS access point edges from the
// mounts already extracted onto the Lambda during the first pass.
func resolveLambdaEFSRefs(g *model.Graph, logicalID string) {
	fn, ok := g.Lambdas[logicalID]
	if !ok {
		return
	}
	for _, m := range fn.EFSMounts {
		if m.AccessPointRef == "" {
			continue
		}
		g.AddEdge(logicalID, m.AccessPointRef, model.EdgeMounts, "EFS mount: "+m.LocalMountPath)
	}
}

// extractSAMEvents processes the SAM Events property on a Serverless::Function.
// SAM Events define triggers inline rather than through separate CFN resources.
func extractSAMEvents(g *model.Graph, logicalID string, res RawResource) {
	if res.Type != "AWS::Serverless::Function" {
		return
	}
	events, ok := res.Properties["Events"].(map[string]interface{})
	if !ok {
		return
	}

	for _, eventName := range sortedStringKeys(events) {
		event, ok := events[eventName].(map[string]interface{})
		if !ok {
			continue
		}
		eventType := getString(event, "Type")
		eventProps, _ := event["Properties"].(map[string]interface{})

		switch eventType {
		case "Api", "HttpApi":
			path := getString(eventProps, "Path")
			method := getString(eventProps, "Method")
			apiRef := resolveRef(eventProps["RestApiId"])
			if apiRef == "" {
				apiRef = resolveRef(eventProps["ApiId"])
			}
			detail := strings.ToUpper(method) + " " + path
			if apiRef != "" {
				g.Routes = append(g.Routes, model.APIRoute{
					APIID:     apiRef,
					Path:      path,
					Method:    strings.ToUpper(method),
					TargetRef: logicalID,
				})
				g.AddEdge(apiRef, logicalID, model.EdgeInvokes, detail)
			} else {
				// Implicit API — SAM creates one automatically.
				g.AddEdge("__implicit_api__", logicalID, model.EdgeInvokes, detail+" (SAM implicit API)")
			}

		case "SQS":
			queueRef := resolveRef(eventProps["Queue"])
			if queueRef != "" {
				g.AddEdge(queueRef, logicalID, model.EdgeTriggers, "SAM SQS event: "+eventName)
			}

		case "SNS":
			topicRef := resolveRef(eventProps["Topic"])
			if topicRef != "" {
				g.AddEdge(topicRef, logicalID, model.EdgeTriggers, "SAM SNS event: "+eventName)
			}

		case "Schedule":
			schedule := getString(eventProps, "Schedule")
			detail := "SAM schedule: " + schedule
			// SAM creates an implicit EventBridge rule. We record the edge
			// but don't create a Rule model entry since there's no explicit resource.
			g.AddEdge("__schedule_"+eventName+"__", logicalID, model.EdgeTriggers, detail)

		case "DynamoDB":
			streamRef := resolveRef(eventProps["Stream"])
			if streamRef != "" {
				g.AddEdge(streamRef, logicalID, model.EdgeTriggers, "SAM DynamoDB stream: "+eventName)
			}

		case "S3":
			bucketRef := resolveRef(eventProps["Bucket"])
			if bucketRef != "" {
				g.AddEdge(bucketRef, logicalID, model.EdgeTriggers, "SAM S3 event: "+eventName)
			}

		case "Kinesis":
			streamRef := resolveRef(eventProps["Stream"])
			if streamRef != "" {
				g.AddEdge(streamRef, logicalID, model.EdgeTriggers, "SAM Kinesis event: "+eventName)
			}
		}
	}
}

// extractLambdaFromIntegrationURI attempts to find a Lambda function reference
// in an API Gateway integration URI. The URI is typically an intrinsic function
// like !Sub "arn:aws:apigateway:.../${FnName}/invocations".
func extractLambdaFromIntegrationURI(uri interface{}) string {
	// Direct reference.
	if ref := resolveRef(uri); ref != "" {
		return ref
	}

	// Sub expression: look for ${LogicalID} or ${LogicalID.Arn} patterns.
	// Fn::Sub has both a plain-string form and a 2-element array form
	// (["...${Var}...", {"Var": {...}}]) used when substituting multiple refs.
	if m, ok := uri.(map[string]interface{}); ok {
		switch sub := m["Fn::Sub"].(type) {
		case string:
			return extractSubRef(sub)
		case []interface{}:
			if len(sub) > 0 {
				if s, ok := sub[0].(string); ok {
					return extractSubRef(s)
				}
			}
		}
	}

	return ""
}

// extractSubRef finds the first ${...} reference in a Fn::Sub string.
func extractSubRef(sub string) string {
	start := strings.Index(sub, "${")
	if start < 0 {
		return ""
	}
	end := strings.Index(sub[start:], "}")
	if end < 0 {
		return ""
	}
	ref := sub[start+2 : start+end]
	// Strip .Arn suffix if present.
	if dotIdx := strings.Index(ref, "."); dotIdx > 0 {
		ref = ref[:dotIdx]
	}
	return ref
}
