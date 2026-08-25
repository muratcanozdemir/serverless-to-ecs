package parser

import (
	"encoding/json"
	"regexp"

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

	// --- S3 ---
	case "AWS::S3::Bucket":
		g.Buckets[logicalID] = extractS3Bucket(logicalID, res.Properties)

	// --- Kinesis ---
	case "AWS::Kinesis::Stream":
		g.Streams[logicalID] = extractKinesisStream(logicalID, res.Properties)

	// --- EFS ---
	case "AWS::EFS::FileSystem":
		g.FileSystems[logicalID] = &model.EFSFileSystem{LogicalID: logicalID}

	case "AWS::EFS::AccessPoint":
		g.AccessPoints[logicalID] = extractEFSAccessPoint(logicalID, res.Properties)

	// --- Secrets Manager / SSM Parameter Store ---
	case "AWS::SecretsManager::Secret":
		g.Secrets[logicalID] = extractSecret(logicalID, res.Properties)

	case "AWS::SSM::Parameter":
		g.Parameters[logicalID] = extractSSMParameter(logicalID, res.Properties)

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
		"AWS::Logs::LogGroup",
		"AWS::EFS::MountTarget",
		"AWS::SecretsManager::SecretTargetAttachment",
		"AWS::S3::BucketPolicy":
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
	envVars := getEnvVars(props)
	return &model.Lambda{
		LogicalID:    logicalID,
		FunctionName: getString(props, "FunctionName"),
		Runtime:      getString(props, "Runtime"),
		Handler:      getString(props, "Handler"),
		MemoryMB:     getInt(props, "MemorySize", 128),
		TimeoutSec:   getInt(props, "Timeout", 3),
		EnvVars:      envVars,
		VPCConfig:    extractVPCConfig(props),
		EFSMounts:    extractEFSMounts(props),
		SecretRefs:   detectDynamicSecretRefs(envVars),
	}
}

func extractSAMLambda(logicalID string, props map[string]interface{}) *model.Lambda {
	envVars := getEnvVars(props)
	l := &model.Lambda{
		LogicalID:    logicalID,
		FunctionName: getString(props, "FunctionName"),
		Runtime:      getString(props, "Runtime"),
		Handler:      getString(props, "Handler"),
		MemoryMB:     getInt(props, "MemorySize", 128),
		TimeoutSec:   getInt(props, "Timeout", 3),
		CodeURI:      getString(props, "CodeUri"),
		EnvVars:      envVars,
		VPCConfig:    extractVPCConfig(props),
		EFSMounts:    extractEFSMounts(props),
		SecretRefs:   detectDynamicSecretRefs(envVars),
	}
	// SAM functions with no explicit FunctionName get the logical ID.
	if l.FunctionName == "" {
		l.FunctionName = logicalID
	}
	return l
}

// extractVPCConfig records that a Lambda runs inside a VPC. Subnet/security
// group IDs are usually Ref/Fn::ImportValue/parameter references that can't
// be resolved to concrete values without deploying the stack, so these are
// kept as human-readable strings for the migration report, not as
// something the emitter can wire into Terraform automatically.
func extractVPCConfig(props map[string]interface{}) *model.VPCConfig {
	vpc, ok := props["VpcConfig"].(map[string]interface{})
	if !ok {
		return nil
	}
	subnets := stringRefs(vpc["SubnetIds"])
	sgs := stringRefs(vpc["SecurityGroupIds"])
	if len(subnets) == 0 && len(sgs) == 0 {
		return nil
	}
	return &model.VPCConfig{SubnetRefs: subnets, SecurityGroupRefs: sgs}
}

// stringRefs converts a CFN list value (literal strings and/or
// Ref/Fn::GetAtt/Fn::Sub intrinsics) into human-readable strings.
func stringRefs(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
			continue
		}
		out = append(out, refToString(item))
	}
	return out
}

// extractEFSMounts reads a Lambda's FileSystemConfigs (Lambda → EFS access
// point mounts).
func extractEFSMounts(props map[string]interface{}) []model.EFSMount {
	configs, ok := props["FileSystemConfigs"].([]interface{})
	if !ok {
		return nil
	}
	var mounts []model.EFSMount
	for _, c := range configs {
		cm, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		mounts = append(mounts, model.EFSMount{
			AccessPointRef: resolveRef(cm["Arn"]),
			LocalMountPath: getString(cm, "LocalMountPath"),
		})
	}
	return mounts
}

// dynamicRefRegex matches CloudFormation's dynamic reference syntax for
// Secrets Manager and SSM Parameter Store, e.g.
// "{{resolve:secretsmanager:my-secret:SecretString:password}}" or
// "{{resolve:ssm:/my/parameter}}". Unlike Ref/Fn::GetAtt, CFN resolves these
// server-side from a literal string embedded directly in the property value
// (they work even when the secret/parameter isn't a resource in this
// template at all), so they're detected independently of Ref resolution.
var dynamicRefRegex = regexp.MustCompile(`\{\{resolve:(secretsmanager|ssm-secure|ssm):[^}]+\}\}`)

// detectDynamicSecretRefs scans already-resolved env var values for the CFN
// dynamic reference syntax. Ref/Fn::GetAtt-based references to a
// AWS::SecretsManager::Secret or AWS::SSM::Parameter resource in this
// template are detected separately, in resolveLambdaEnvRefs (edges.go),
// since that requires cross-referencing other extracted resources.
func detectDynamicSecretRefs(envVars map[string]string) map[string]model.SecretRef {
	var refs map[string]model.SecretRef
	for k, v := range envVars {
		m := dynamicRefRegex.FindStringSubmatch(v)
		if m == nil {
			continue
		}
		kind := "ssm"
		if m[1] == "secretsmanager" {
			kind = "secretsmanager"
		}
		if refs == nil {
			refs = make(map[string]model.SecretRef)
		}
		refs[k] = model.SecretRef{Kind: kind, RawRef: v}
	}
	return refs
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

// --- S3 ---

func extractS3Bucket(logicalID string, props map[string]interface{}) *model.S3Bucket {
	return &model.S3Bucket{
		LogicalID:  logicalID,
		BucketName: getString(props, "BucketName"),
	}
}

// --- Kinesis ---

func extractKinesisStream(logicalID string, props map[string]interface{}) *model.KinesisStream {
	return &model.KinesisStream{
		LogicalID:  logicalID,
		StreamName: getString(props, "Name"),
		ShardCount: getInt(props, "ShardCount", 1),
	}
}

// --- EFS ---

func extractEFSAccessPoint(logicalID string, props map[string]interface{}) *model.EFSAccessPoint {
	return &model.EFSAccessPoint{
		LogicalID:     logicalID,
		FileSystemRef: resolveRef(props["FileSystemId"]),
	}
}

// --- Secrets Manager / SSM Parameter Store ---

func extractSecret(logicalID string, props map[string]interface{}) *model.SecretsManagerSecret {
	name := getString(props, "Name")
	if name == "" {
		name = logicalID
	}
	return &model.SecretsManagerSecret{LogicalID: logicalID, Name: name}
}

func extractSSMParameter(logicalID string, props map[string]interface{}) *model.SSMParameter {
	name := getString(props, "Name")
	if name == "" {
		name = logicalID
	}
	typ := getString(props, "Type")
	if typ == "" {
		typ = "String"
	}
	return &model.SSMParameter{LogicalID: logicalID, Name: name, Type: typ}
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
