package parser

import (
	"path/filepath"
	"testing"

	"serverless-to-ecs/internal/model"
)

// parseJSON writes the given JSON template to a temp file and parses it —
// a lighter-weight fixture than a full YAML file for testing one behavior
// in isolation.
func parseJSON(t *testing.T, jsonTemplate string) *model.Graph {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.json")
	if err := writeFile(path, []byte(jsonTemplate)); err != nil {
		t.Fatalf("write template: %v", err)
	}
	g, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return g
}

// Regression test for the bug where an HTTP API v2 Route's Target (which
// points at an Integration, not a Lambda) was stored verbatim as TargetRef,
// so the route never resolved to its Lambda and was silently dropped from
// service grouping.
func TestParseHTTPAPIV2Route_ResolvesThroughIntegration(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"HttpApi": {"Type": "AWS::ApiGatewayV2::Api", "Properties": {"Name": "MyHttpApi", "ProtocolType": "HTTP"}},
			"MyFunc": {"Type": "AWS::Lambda::Function", "Properties": {"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main"}},
			"MyIntegration": {
				"Type": "AWS::ApiGatewayV2::Integration",
				"Properties": {
					"ApiId": {"Ref": "HttpApi"},
					"IntegrationType": "AWS_PROXY",
					"IntegrationUri": {"Fn::GetAtt": ["MyFunc", "Arn"]}
				}
			},
			"MyRoute": {
				"Type": "AWS::ApiGatewayV2::Route",
				"Properties": {
					"ApiId": {"Ref": "HttpApi"},
					"RouteKey": "GET /items",
					"Target": {"Fn::Sub": "integrations/${MyIntegration}"}
				}
			}
		}
	}`)

	if len(g.Routes) != 1 {
		t.Fatalf("expected 1 route, got %d: %+v", len(g.Routes), g.Routes)
	}
	route := g.Routes[0]
	if route.TargetRef != "MyFunc" {
		t.Errorf("route.TargetRef = %q, want %q (resolved through the Integration)", route.TargetRef, "MyFunc")
	}
	if route.APIID != "HttpApi" {
		t.Errorf("route.APIID = %q, want %q", route.APIID, "HttpApi")
	}

	found := false
	for _, e := range g.Edges {
		if e.From == "HttpApi" && e.To == "MyFunc" && e.Type == model.EdgeInvokes {
			found = true
		}
	}
	if !found {
		t.Error("expected an invokes edge from HttpApi to MyFunc")
	}
}

// Regression test: a Route whose Target integration is never defined (or
// doesn't resolve to a Lambda) must be dropped, not produce a route with an
// empty/wrong TargetRef.
func TestParseHTTPAPIV2Route_UnresolvedIntegrationDropsRoute(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"HttpApi": {"Type": "AWS::ApiGatewayV2::Api", "Properties": {"Name": "MyHttpApi"}},
			"MyRoute": {
				"Type": "AWS::ApiGatewayV2::Route",
				"Properties": {
					"ApiId": {"Ref": "HttpApi"},
					"RouteKey": "GET /items",
					"Target": {"Fn::Sub": "integrations/${MissingIntegration}"}
				}
			}
		}
	}`)

	if len(g.Routes) != 0 {
		t.Errorf("expected no routes for an unresolvable integration target, got %+v", g.Routes)
	}
}

// Regression test for the bug where DefinitionString (the standard, non-SAM
// way to define a state machine) was parsed and immediately discarded,
// leaving StateCount/Pattern/TaskTargets zeroed out.
func TestParseStepFunction_DefinitionStringPlainString(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {"Type": "AWS::Lambda::Function", "Properties": {"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main"}},
			"MySFN": {
				"Type": "AWS::StepFunctions::StateMachine",
				"Properties": {
					"StateMachineName": "wf",
					"DefinitionString": "{\"StartAt\":\"Task1\",\"States\":{\"Task1\":{\"Type\":\"Task\",\"Resource\":\"${MyFunc.Arn}\",\"End\":true}}}"
				}
			}
		}
	}`)

	sf, ok := g.StepFuncs["MySFN"]
	if !ok {
		t.Fatal("missing StepFunction: MySFN")
	}
	if sf.StateCount != 1 {
		t.Errorf("StateCount = %d, want 1", sf.StateCount)
	}
	if len(sf.TaskTargets) != 1 || sf.TaskTargets[0] != "MyFunc" {
		t.Errorf("TaskTargets = %v, want [MyFunc]", sf.TaskTargets)
	}
	if sf.Pattern != model.SFNSequential {
		t.Errorf("Pattern = %q, want %q", sf.Pattern, model.SFNSequential)
	}
}

// The far more common form: DefinitionString wrapped in Fn::Sub so ${Ref}
// placeholders can be substituted at deploy time.
func TestParseStepFunction_DefinitionStringFnSub(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {"Type": "AWS::Lambda::Function", "Properties": {"FunctionName": "fn", "Runtime": "go1.x", "Handler": "main"}},
			"MySFN": {
				"Type": "AWS::StepFunctions::StateMachine",
				"Properties": {
					"StateMachineName": "wf",
					"DefinitionString": {
						"Fn::Sub": "{\"StartAt\":\"Task1\",\"States\":{\"Task1\":{\"Type\":\"Task\",\"Resource\":\"${MyFunc.Arn}\",\"End\":true}}}"
					}
				}
			}
		}
	}`)

	sf, ok := g.StepFuncs["MySFN"]
	if !ok {
		t.Fatal("missing StepFunction: MySFN")
	}
	if sf.StateCount != 1 || len(sf.TaskTargets) != 1 || sf.TaskTargets[0] != "MyFunc" {
		t.Errorf("expected DefinitionString wrapped in Fn::Sub to be parsed like a plain string, got StateCount=%d TaskTargets=%v",
			sf.StateCount, sf.TaskTargets)
	}

	found := false
	for _, e := range g.Edges {
		if e.From == "MySFN" && e.To == "MyFunc" && e.Type == model.EdgeOrchestrates {
			found = true
		}
	}
	if !found {
		t.Error("expected an orchestrates edge from MySFN to MyFunc")
	}
}

// Regression test: a malformed Fn::GetAtt (non-string element) in a Lambda
// env var must not panic ParseFile.
func TestParseLambda_MalformedGetAttEnvVarDoesNotPanic(t *testing.T) {
	g := parseJSON(t, `{
		"Resources": {
			"MyFunc": {
				"Type": "AWS::Lambda::Function",
				"Properties": {
					"FunctionName": "fn",
					"Runtime": "go1.x",
					"Handler": "main",
					"Environment": {"Variables": {"BAD": {"Fn::GetAtt": [123, "Arn"]}}}
				}
			}
		}
	}`)

	fn, ok := g.Lambdas["MyFunc"]
	if !ok {
		t.Fatal("missing Lambda: MyFunc")
	}
	if fn.EnvVars["BAD"] != "<?>" {
		t.Errorf("expected malformed Fn::GetAtt to resolve to the \"<?>\" placeholder, got %q", fn.EnvVars["BAD"])
	}
}

func TestAnalyzeASL_PatternClassification(t *testing.T) {
	tests := []struct {
		name string
		def  map[string]interface{}
		want model.SFNPattern
	}{
		{
			name: "sequential (only Task states)",
			def: map[string]interface{}{"States": map[string]interface{}{
				"A": map[string]interface{}{"Type": "Task"},
				"B": map[string]interface{}{"Type": "Task"},
			}},
			want: model.SFNSequential,
		},
		{
			name: "parallel",
			def: map[string]interface{}{"States": map[string]interface{}{
				"A": map[string]interface{}{"Type": "Parallel"},
			}},
			want: model.SFNParallel,
		},
		{
			name: "map",
			def: map[string]interface{}{"States": map[string]interface{}{
				"A": map[string]interface{}{"Type": "Map"},
			}},
			want: model.SFNMap,
		},
		{
			name: "choice",
			def: map[string]interface{}{"States": map[string]interface{}{
				"A": map[string]interface{}{"Type": "Choice"},
			}},
			want: model.SFNChoice,
		},
		{
			name: "mixed (more than one of choice/parallel/map)",
			def: map[string]interface{}{"States": map[string]interface{}{
				"A": map[string]interface{}{"Type": "Choice"},
				"B": map[string]interface{}{"Type": "Parallel"},
			}},
			want: model.SFNMixed,
		},
		{
			name: "no States key at all",
			def:  map[string]interface{}{},
			want: model.SFNUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, pattern, _ := analyzeASL(tt.def)
			if pattern != tt.want {
				t.Errorf("analyzeASL pattern = %q, want %q", pattern, tt.want)
			}
		})
	}
}

// Regression test: analyzeASL must iterate States in a deterministic order
// so TaskTargets (and downstream orchestrated-group membership) doesn't
// vary between runs of the same template.
func TestAnalyzeASL_DeterministicTaskTargetOrder(t *testing.T) {
	def := map[string]interface{}{"States": map[string]interface{}{
		"Zeta":  map[string]interface{}{"Type": "Task", "Resource": map[string]interface{}{"Ref": "ZFn"}},
		"Alpha": map[string]interface{}{"Type": "Task", "Resource": map[string]interface{}{"Ref": "AFn"}},
		"Mu":    map[string]interface{}{"Type": "Task", "Resource": map[string]interface{}{"Ref": "MFn"}},
	}}

	_, _, first := analyzeASL(def)
	for i := 0; i < 20; i++ {
		_, _, got := analyzeASL(def)
		if len(got) != len(first) {
			t.Fatalf("run %d: length changed: %v vs %v", i, got, first)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: order changed: %v vs %v", i, got, first)
			}
		}
	}
}
