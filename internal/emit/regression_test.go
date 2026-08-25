package emit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/model"
)

// Regression test: a schedule whose triggered Lambda never resolves to a
// generated service group used to emit a reference to a task definition
// that was never declared (aws_ecs_task_definition.unknown), which fails
// terraform validate. It must now emit the schedule without that block.
func TestEmitTerraform_UnresolvedScheduleOmitsTaskDefRef(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	// A rule whose target Lambda doesn't exist in the graph — findTriggeredLambdas
	// will return nothing, so the schedule can't resolve to any service group.
	g.Rules["OrphanRule"] = &model.EventBridgeRule{
		LogicalID: "OrphanRule",
		Name:      "orphan-rule",
		Schedule:  "rate(1 hour)",
	}

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "scheduled.tf"))
	if err != nil {
		t.Fatalf("read scheduled.tf: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "aws_ecs_task_definition.unknown") {
		t.Errorf("scheduled.tf references an undeclared task definition:\n%s", content)
	}
	if !strings.Contains(content, "TODO") {
		t.Errorf("expected a TODO marker for the unresolved schedule, got:\n%s", content)
	}
}

// Regression test: Lambda env var values containing characters that would
// break out of an HCL string literal (quotes, backslashes) must be safely
// escaped in the generated ecs.tf, not interpolated raw.
func TestEmitTerraform_EnvVarsAreHCLEscaped(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Lambdas["Fn"] = &model.Lambda{
		LogicalID:    "Fn",
		FunctionName: "fn",
		MemoryMB:     256,
		EnvVars: map[string]string{
			"CONFIG": `{"key": "value with \"quotes\" and a backslash \\"}`,
		},
	}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "Api", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "ecs.tf"))
	if err != nil {
		t.Fatalf("read ecs.tf: %v", err)
	}
	content := string(data)

	// The escaped value must appear as a valid Go/HCL-quoted string — every
	// literal '"' from the source value must be preceded by a backslash.
	idx := strings.Index(content, `value = "`)
	if idx == -1 {
		t.Fatalf("expected an environment value in ecs.tf, got:\n%s", content)
	}
	// Sanity check the raw (unescaped) value never appears verbatim, which
	// would indicate it broke out of its string literal.
	if strings.Contains(content, `value with "quotes"`) {
		t.Errorf("env var value was interpolated unescaped, breaking the HCL string literal:\n%s", content)
	}
}

// Regression test: main.tf's IAM TODO comments must not let a newline in a
// function name or env var hint break out of the "#"-comment line.
func TestEmitTerraform_CommentHintsStripNewlines(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Lambdas["Fn"] = &model.Lambda{
		LogicalID:    "Fn",
		FunctionName: "fn",
		MemoryMB:     256,
		EnvVars: map[string]string{
			"INJECT": "safe\n} resource \"evil\" \"x\" {}",
		},
	}

	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(tmpDir, "main.tf"))
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}
	if strings.Contains(string(data), "\nresource \"evil\"") {
		t.Errorf("env var hint newline was not stripped, injecting a line into main.tf:\n%s", data)
	}
}

// Regression test: deriveProjectName must truncate by rune, not byte, so a
// non-ASCII Description doesn't produce an invalid UTF-8 project name.
func TestDeriveProjectName_TruncatesByRune(t *testing.T) {
	g := model.NewGraph()
	// A word of 25 multi-byte runes ("é" is 2 bytes in UTF-8) — byte-index
	// truncation at 20 would split a rune; rune-index truncation must not.
	word := strings.Repeat("é", 25)
	g.Description = word + " rest of description"

	got := deriveProjectName(g)

	if !stringsValidUTF8(got) {
		t.Fatalf("deriveProjectName produced invalid UTF-8: %q", got)
	}
}

func stringsValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestTruncateName_TruncatesByRune(t *testing.T) {
	s := strings.Repeat("é", 10)
	got := truncateName(s, 5)
	if want := strings.Repeat("é", 5); got != want {
		t.Errorf("truncateName(%q, 5) = %q, want %q", s, got, want)
	}
}

func TestAPIServiceData_TGNameRespects32CharLimit(t *testing.T) {
	g := model.NewGraph()
	g.Description = "test"
	g.Lambdas["Fn"] = &model.Lambda{LogicalID: "Fn", FunctionName: "fn", MemoryMB: 256}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "a-very-long-api-gateway-name-that-exceeds-the-limit", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")

	groups := cost.GroupServices(g)
	data := assembleTerraformData(g, groups, "test.yaml", "eu-central-1")

	if len(data.APIServices) != 1 {
		t.Fatalf("expected 1 API service, got %d", len(data.APIServices))
	}
	if len(data.APIServices[0].TGName) > 32 {
		t.Errorf("TGName %q exceeds AWS's 32-char target group name limit (%d chars)",
			data.APIServices[0].TGName, len(data.APIServices[0].TGName))
	}
}
