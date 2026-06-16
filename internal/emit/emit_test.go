package emit

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/model"
	"serverless-to-ecs/internal/parser"
)

var update = false

func init() {
	// Check for -update flag in test args.
	for _, arg := range os.Args {
		if arg == "-update" || arg == "--update" {
			update = true
		}
	}
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func TestEmitTerraform_GoldenFiles(t *testing.T) {
	root := repoRoot()
	stackPath := filepath.Join(root, "examples", "synthetic-stack.yaml")
	goldenDir := filepath.Join(root, "examples", "expected-output", "terraform")

	g, err := parser.ParseFile(stackPath)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	groups := cost.GroupServices(g)

	// Generate to a temp directory.
	tmpDir := t.TempDir()
	if err := EmitTerraform(g, groups, "examples/synthetic-stack.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	tfFiles := []string{
		"main.tf",
		"variables.tf",
		"ecs.tf",
		"alb.tf",
		"scheduled.tf",
		"sqs_pollers.tf",
		"outputs.tf",
	}

	if update {
		// Write golden files.
		if err := os.MkdirAll(goldenDir, 0755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		for _, name := range tfFiles {
			src := filepath.Join(tmpDir, name)
			dst := filepath.Join(goldenDir, name)
			data, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("read generated %s: %v", name, err)
			}
			// Strip the timestamp line so golden files are deterministic.
			data = stripTimestamp(data)
			if err := os.WriteFile(dst, data, 0644); err != nil {
				t.Fatalf("write golden %s: %v", name, err)
			}
			t.Logf("updated golden: %s", dst)
		}
		return
	}

	// Compare against golden files.
	for _, name := range tfFiles {
		t.Run(name, func(t *testing.T) {
			generated, err := os.ReadFile(filepath.Join(tmpDir, name))
			if err != nil {
				t.Fatalf("read generated: %v", err)
			}
			generated = stripTimestamp(generated)

			golden, err := os.ReadFile(filepath.Join(goldenDir, name))
			if err != nil {
				t.Fatalf("read golden (run 'go test ./internal/emit/ -args -update' to generate): %v", err)
			}

			if string(generated) != string(golden) {
				t.Errorf("%s does not match golden file.\n"+
					"Run 'go test ./internal/emit/ -args -update' to regenerate.\n"+
					"Diff (first divergence):\n%s",
					name, firstDiff(string(golden), string(generated)))
			}
		})
	}
}

// TestEmitTerraform_AllFilesCreated verifies every expected file is generated.
func TestEmitTerraform_AllFilesCreated(t *testing.T) {
	g := minimalGraph()
	groups := cost.GroupServices(g)
	tmpDir := t.TempDir()

	if err := EmitTerraform(g, groups, "test.yaml", "eu-central-1", tmpDir); err != nil {
		t.Fatalf("EmitTerraform: %v", err)
	}

	expected := []string{
		"main.tf", "variables.tf", "ecs.tf", "alb.tf",
		"scheduled.tf", "sqs_pollers.tf", "outputs.tf",
	}
	for _, name := range expected {
		path := filepath.Join(tmpDir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("missing file: %s", name)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("empty file: %s", name)
		}
	}
}

// minimalGraph returns a small graph for basic template rendering tests.
func minimalGraph() *model.Graph {
	g := model.NewGraph()
	g.Description = "test stack"
	g.Lambdas["TestFn"] = &model.Lambda{
		LogicalID:    "TestFn",
		FunctionName: "test-fn",
		Runtime:      "go1.x",
		MemoryMB:     256,
		TimeoutSec:   10,
	}
	g.APIs["TestAPI"] = &model.APIGateway{
		LogicalID: "TestAPI",
		Name:      "TestAPI",
		Protocol:  "REST",
	}
	g.Routes = append(g.Routes, model.APIRoute{
		APIID:     "TestAPI",
		Path:      "/test",
		Method:    "GET",
		TargetRef: "TestFn",
	})
	g.AddEdge("TestAPI", "TestFn", model.EdgeInvokes, "GET /test")
	return g
}

// stripTimestamp removes the Date line from main.tf so golden files are stable.
func stripTimestamp(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var out []string
	for _, line := range lines {
		if strings.Contains(line, "Date:") && strings.Contains(line, "20") {
			out = append(out, "# │  Date:   (stripped for golden file)")
		} else {
			out = append(out, line)
		}
	}
	return []byte(strings.Join(out, "\n"))
}

func firstDiff(a, b string) string {
	linesA := strings.Split(a, "\n")
	linesB := strings.Split(b, "\n")
	maxLines := len(linesA)
	if len(linesB) > maxLines {
		maxLines = len(linesB)
	}
	for i := 0; i < maxLines; i++ {
		la, lb := "", ""
		if i < len(linesA) {
			la = linesA[i]
		}
		if i < len(linesB) {
			lb = linesB[i]
		}
		if la != lb {
			return formatDiffContext(linesA, linesB, i, 3)
		}
	}
	return "(files differ in length)"
}

func formatDiffContext(a, b []string, idx, ctx int) string {
	var sb strings.Builder
	start := idx - ctx
	if start < 0 {
		start = 0
	}
	end := idx + ctx + 1

	sb.WriteString("--- golden\n+++ generated\n")
	for i := start; i < end; i++ {
		la, lb := "", ""
		if i < len(a) {
			la = a[i]
		}
		if i < len(b) {
			lb = b[i]
		}
		if i == idx {
			sb.WriteString("- " + la + "\n")
			sb.WriteString("+ " + lb + "\n")
		} else {
			sb.WriteString("  " + la + "\n")
		}
	}
	return sb.String()
}