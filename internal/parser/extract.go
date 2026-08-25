package parser

import (
	"encoding/json"

	"serverless-to-ecs/internal/model"
)

// extractResource dispatches on the CFN resource Type and populates the graph.
func extractResource(g *model.Graph, logicalID string, res RawResource) {
	switch res.Type {

	// --- Lambda ---
	case "AWS::Lambda::Function":
		g.Lambdas[logicalID] = extractLambda(logicalID, res.Properties)

	case "AWS::Serverless::Function":
		g.Lambdas[logicalID] = extractSAMLambda(logicalID, res.Properties)

	// --- API Gateway ---
	case "AWS::ApiGateway::RestApi":
		g.APIs[logicalID] = extractRestAPI(logicalID, res.Properties)

	case "AWS::ApiGatewayV2::Api":
		g.APIs[logicalID] = extractHTTPAPI(logicalID, res.Properties)

	case "AWS::Serverless::Api":
		g.APIs[logicalID] = extractSAMApi(logicalID, res.Properties)

	case "AWS::Serverless::HttpApi":
		g.APIs[logicalID] = extractSAMHttpApi(logicalID, res.Properties)

	case "AWS::ApiGatewayV2::Integration":
		if lambdaRef := extractLambdaFromIntegrationURI(res.Properties["IntegrationUri"]); lambdaRef != "" {
			g.HTTPIntegrations[logicalID] = lambdaRef
		}

	// --- Step Functions ---
	case "AWS::StepFunctions::StateMachine":
		g.StepFuncs[logicalID] = extractStepFunction(logicalID, res.Properties)

	// --- EventBridge ---
	case "AWS::Events::Rule":
		g.Rules[logicalID] = extractEventBridgeRule(logicalID, res.Properties)

	// --- SQS ---
	case "AWS::SQS::Queue":
		g.Queues[logicalID] = extractSQSQueue(logicalID, res.Properties)

	case "AWS::Serverless::SimpleTable":
		g.Tables[logicalID] = extractSAMSimpleTable(logicalID, res.Properties)

	// --- SNS ---
	case "AWS::SNS::Topic":
		g.Topics[logicalID] = extractSNSTopic(logicalID, res.Properties)

	// --- DynamoDB ---
	case "AWS::DynamoDB::Table":
		g.Tables[logicalID] = extractDynamoDBTable(logicalID, res.Properties)

	// --- Resources we detect but don't model ---
	case "AWS::Lambda::EventSourceMapping",
		"AWS::ApiGateway::Method",
		"AWS::ApiGateway::Integration",
		"AWS::ApiGateway::Deployment",
		"AWS::ApiGateway::Stage",
		"AWS::ApiGatewayV2::Route",
		"AWS::ApiGatewayV2::Stage",
		"AWS::SNS::Subscription",
		"AWS::Lambda::Permission",
		"AWS::IAM::Role",
		"AWS::IAM::Policy",
		"AWS::Logs::LogGroup":
		// These are wiring/permission resources. We use them for edge detection
		// (in edges.go) but don't model them as top-level inventory items.
		return

	default:
		g.Unsupported = append(g.Unsupported, model.UnsupportedResource{
			LogicalID:    logicalID,
			ResourceType: res.Type,
		})
	}
}

// --- Lambda extractors ---

func extractLambda(logicalID string, props map[string]interface{}) *model.Lambda {
	return &model.Lambda{
		LogicalID:    logicalID,
		FunctionName: getString(props, "FunctionName"),
		Runtime:      getString(props, "Runtime"),
		Handler:      getString(props, "Handler"),
		MemoryMB:     getInt(props, "MemorySize", 128),
		TimeoutSec:   getInt(props, "Timeout", 3),
		EnvVars:      getEnvVars(props),
	}
}

func extractSAMLambda(logicalID string, props map[string]interface{}) *model.Lambda {
	l := &model.Lambda{
		LogicalID:    logicalID,
		FunctionName: getString(props, "FunctionName"),
		Runtime:      getString(props, "Runtime"),
		Handler:      getString(props, "Handler"),
		MemoryMB:     getInt(props, "MemorySize", 128),
		TimeoutSec:   getInt(props, "Timeout", 3),
		CodeURI:      getString(props, "CodeUri"),
		EnvVars:      getEnvVars(props),
	}
	// SAM functions with no explicit FunctionName get the logical ID.
	if l.FunctionName == "" {
		l.FunctionName = logicalID
	}
	return l
}

// --- API Gateway extractors ---

func extractRestAPI(logicalID string, props map[string]interface{}) *model.APIGateway {
	return &model.APIGateway{
		LogicalID: logicalID,
		Name:      getString(props, "Name"),
		Protocol:  "REST",
	}
}

func extractHTTPAPI(logicalID string, props map[string]interface{}) *model.APIGateway {
	proto := "HTTP"
	if pt := getString(props, "ProtocolType"); pt != "" {
		proto = pt
	}
	return &model.APIGateway{
		LogicalID: logicalID,
		Name:      getString(props, "Name"),
		Protocol:  proto,
	}
}

func extractSAMApi(logicalID string, props map[string]interface{}) *model.APIGateway {
	return &model.APIGateway{
		LogicalID: logicalID,
		Name:      getString(props, "Name"),
		Protocol:  "REST",
		Stage:     getString(props, "StageName"),
	}
}

func extractSAMHttpApi(logicalID string, props map[string]interface{}) *model.APIGateway {
	return &model.APIGateway{
		LogicalID: logicalID,
		Name:      getString(props, "Name"),
		Protocol:  "HTTP",
		Stage:     getString(props, "StageName"),
	}
}

// --- Step Functions ---

func extractStepFunction(logicalID string, props map[string]interface{}) *model.StepFunction {
	sf := &model.StepFunction{
		LogicalID: logicalID,
		Name:      getString(props, "StateMachineName"),
		Pattern:   model.SFNUnknown,
	}

	// The Definition can be inline (map, SAM style) or DefinitionString (raw CFN
	// style — a JSON string, typically wrapped in Fn::Sub so ${Ref} placeholders
	// can be substituted at deploy time).
	if def, ok := props["Definition"].(map[string]interface{}); ok {
		sf.DefinitionRaw = def
		sf.StateCount, sf.Pattern, sf.TaskTargets = analyzeASL(def)
	} else if defStr := definitionStringValue(props["DefinitionString"]); defStr != "" {
		var def map[string]interface{}
		if err := json.Unmarshal([]byte(defStr), &def); err == nil {
			sf.DefinitionRaw = def
			sf.StateCount, sf.Pattern, sf.TaskTargets = analyzeASL(def)
		}
	}

	return sf
}

// definitionStringValue extracts the raw ASL JSON text from a DefinitionString
// property. It handles both a plain string and the far more common Fn::Sub-wrapped
// form (string or 2-element array). The ${...} placeholders left inside Task
// "Resource" strings by Fn::Sub don't affect JSON validity, so the text can be
// parsed as-is; extractTaskTarget resolves those placeholders separately.
func definitionStringValue(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}:
		switch sub := t["Fn::Sub"].(type) {
		case string:
			return sub
		case []interface{}:
			if len(sub) > 0 {
				if s, ok := sub[0].(string); ok {
					return s
				}
			}
		}
	}
	return ""
}

// analyzeASL does a shallow parse of the ASL definition to count states,
// classify the dominant pattern, and extract Lambda task targets.
func analyzeASL(def map[string]interface{}) (int, model.SFNPattern, []string) {
	statesRaw, ok := def["States"].(map[string]interface{})
	if !ok {
		return 0, model.SFNUnknown, nil
	}

	var targets []string
	hasChoice := false
	hasParallel := false
	hasMap := false

	// Iterate state names in sorted order so TaskTargets (and therefore
	// downstream orchestrated-group membership order) is deterministic —
	// ASL's States object has no inherent order of its own (actual execution
	// order comes from StartAt/Next, which this shallow parse doesn't follow).
	for _, stateName := range sortedStringKeys(statesRaw) {
		state, ok := statesRaw[stateName].(map[string]interface{})
		if !ok {
			continue
		}

		stateType := getString(state, "Type")
		switch stateType {
		case "Task":
			if ref := extractTaskTarget(state); ref != "" {
				targets = append(targets, ref)
			}
		case "Choice":
			hasChoice = true
		case "Parallel":
			hasParallel = true
		case "Map":
			hasMap = true
		}
	}

	count := len(statesRaw)
	pattern := classifyPattern(hasChoice, hasParallel, hasMap, count)
	return count, pattern, targets
}

func classifyPattern(hasChoice, hasParallel, hasMap bool, stateCount int) model.SFNPattern {
	mixed := 0
	if hasChoice {
		mixed++
	}
	if hasParallel {
		mixed++
	}
	if hasMap {
		mixed++
	}
	if mixed > 1 {
		return model.SFNMixed
	}
	if hasChoice {
		return model.SFNChoice
	}
	if hasParallel {
		return model.SFNParallel
	}
	if hasMap {
		return model.SFNMap
	}
	if stateCount > 0 {
		return model.SFNSequential
	}
	return model.SFNUnknown
}

// extractTaskTarget attempts to resolve the Lambda logical ID from a Task state's
// Resource field. In an inline (SAM) Definition, Resource is an intrinsic function
// object ({"Fn::GetAtt": [...]} or {"Ref": ...}). In a DefinitionString parsed from
// raw ASL JSON text, Resource is instead a plain string containing a CFN Fn::Sub
// placeholder, e.g. "${MyFunction.Arn}" or "${MyFunction}".
func extractTaskTarget(state map[string]interface{}) string {
	res := state["Resource"]
	if res == nil {
		return ""
	}
	switch v := res.(type) {
	case string:
		return extractSubRef(v)
	case map[string]interface{}:
		if getAtt, ok := v["Fn::GetAtt"].([]interface{}); ok && len(getAtt) >= 1 {
			if s, ok := getAtt[0].(string); ok {
				return s
			}
		}
		if ref, ok := v["Ref"].(string); ok {
			return ref
		}
	}
	return ""
}

// --- EventBridge ---

func extractEventBridgeRule(logicalID string, props map[string]interface{}) *model.EventBridgeRule {
	rule := &model.EventBridgeRule{
		LogicalID: logicalID,
		Name:      getString(props, "Name"),
		Schedule:  getString(props, "ScheduleExpression"),
	}

	if ep, ok := props["EventPattern"].(map[string]interface{}); ok {
		rule.EventPattern = ep
	}

	if targets, ok := props["Targets"].([]interface{}); ok {
		for _, t := range targets {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			if ref := resolveRef(tm["Arn"]); ref != "" {
				rule.TargetRefs = append(rule.TargetRefs, ref)
			} else if arn, ok := tm["Arn"].(string); ok {
				rule.TargetLiterals = append(rule.TargetLiterals, arn)
			}
		}
	}

	return rule
}

// --- SQS ---

func extractSQSQueue(logicalID string, props map[string]interface{}) *model.SQSQueue {
	return &model.SQSQueue{
		LogicalID:            logicalID,
		QueueName:            getString(props, "QueueName"),
		FIFOQueue:            getBool(props, "FifoQueue"),
		VisibilityTimeoutSec: getInt(props, "VisibilityTimeout", 30),
		MessageRetentionSec:  getInt(props, "MessageRetentionPeriod", 345600),
		DelaySeconds:         getInt(props, "DelaySeconds", 0),
	}
}

// --- SNS ---

func extractSNSTopic(logicalID string, props map[string]interface{}) *model.SNSTopic {
	return &model.SNSTopic{
		LogicalID: logicalID,
		TopicName: getString(props, "TopicName"),
		FIFOTopic: getBool(props, "FifoTopic"),
	}
}

// --- DynamoDB ---

func extractDynamoDBTable(logicalID string, props map[string]interface{}) *model.DynamoDBTable {
	t := &model.DynamoDBTable{
		LogicalID:   logicalID,
		TableName:   getString(props, "TableName"),
		BillingMode: getString(props, "BillingMode"),
	}
	if t.BillingMode == "" {
		t.BillingMode = "PROVISIONED" // CFN default
	}

	// Key schema.
	if ks, ok := props["KeySchema"].([]interface{}); ok {
		for _, k := range ks {
			km, ok := k.(map[string]interface{})
			if !ok {
				continue
			}
			name := getString(km, "AttributeName")
			keyType := getString(km, "KeyType")
			if keyType == "HASH" {
				t.HashKey = name
			} else if keyType == "RANGE" {
				t.RangeKey = name
			}
		}
	}

	// Provisioned throughput.
	if pt, ok := props["ProvisionedThroughput"].(map[string]interface{}); ok {
		t.RCU = getInt(pt, "ReadCapacityUnits", 5)
		t.WCU = getInt(pt, "WriteCapacityUnits", 5)
	}

	// GSI count.
	if gsis, ok := props["GlobalSecondaryIndexes"].([]interface{}); ok {
		t.GSICount = len(gsis)
	}

	return t
}

func extractSAMSimpleTable(logicalID string, props map[string]interface{}) *model.DynamoDBTable {
	t := &model.DynamoDBTable{
		LogicalID:   logicalID,
		TableName:   getString(props, "TableName"),
		BillingMode: "PAY_PER_REQUEST",
	}
	if pk, ok := props["PrimaryKey"].(map[string]interface{}); ok {
		t.HashKey = getString(pk, "Name")
	}
	return t
}

// --- Property accessors ---

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func getInt(m map[string]interface{}, key string, fallback int) int {
	v, ok := m[key]
	if !ok {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	}
	return fallback
}

func getBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
		// CFN sometimes uses strings "true"/"false".
		if s, ok := v.(string); ok {
			return s == "true"
		}
	}
	return false
}

func getEnvVars(props map[string]interface{}) map[string]string {
	env, ok := props["Environment"].(map[string]interface{})
	if !ok {
		return nil
	}
	vars, ok := env["Variables"].(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]string, len(vars))
	for k, v := range vars {
		if s, ok := v.(string); ok {
			out[k] = s
		} else {
			// Intrinsic function reference — store a placeholder.
			out[k] = refToString(v)
		}
	}
	return out
}

// resolveRef checks if a value is a {"Ref": "X"} or {"Fn::GetAtt": ["X", "Y"]}
// and returns the logical ID, or empty string if not a reference.
func resolveRef(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	if ref, ok := m["Ref"].(string); ok {
		return ref
	}
	if ga, ok := m["Fn::GetAtt"].([]interface{}); ok && len(ga) >= 1 {
		if s, ok := ga[0].(string); ok {
			return s
		}
	}
	return ""
}

// refToString produces a human-readable placeholder for an intrinsic function.
func refToString(v interface{}) string {
	m, ok := v.(map[string]interface{})
	if !ok {
		return "<?>"
	}
	if ref, ok := m["Ref"].(string); ok {
		return "${" + ref + "}"
	}
	if ga, ok := m["Fn::GetAtt"].([]interface{}); ok && len(ga) >= 2 {
		s0, ok0 := ga[0].(string)
		s1, ok1 := ga[1].(string)
		if ok0 && ok1 {
			return "${" + s0 + "." + s1 + "}"
		}
	}
	switch sub := m["Fn::Sub"].(type) {
	case string:
		return sub
	case []interface{}:
		if len(sub) > 0 {
			if s, ok := sub[0].(string); ok {
				return s
			}
		}
	}
	return "<?>"
}
