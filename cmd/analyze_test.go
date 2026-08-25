package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..")
}

// captureStdout redirects os.Stdout for the duration of fn and returns what was written.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	fn()

	w.Close()
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func TestRun_Version(t *testing.T) {
	old := Version
	Version = "v1.2.3-test"
	defer func() { Version = old }()

	var code int
	out := captureStdout(t, func() {
		code = Run([]string{"-version"})
	})
	if code != 0 {
		t.Fatalf("Run -version = %d, want 0", code)
	}
	if !strings.Contains(out, "v1.2.3-test") {
		t.Errorf("expected stamped version in output, got %q", out)
	}
}

func TestRun_MissingTemplateFlag(t *testing.T) {
	code := Run(nil)
	if code != 1 {
		t.Errorf("Run(nil) = %d, want 1", code)
	}
}

func TestRun_UnknownFlagReturnsErrorInsteadOfExiting(t *testing.T) {
	// Regression test: Run previously used the global flag.CommandLine with
	// ExitOnError, which would call os.Exit(2) directly on a bad flag and
	// kill the test process. It now uses a local FlagSet with
	// ContinueOnError, so an unknown flag must return an error code instead.
	code := Run([]string{"-this-flag-does-not-exist"})
	if code != 1 {
		t.Errorf("Run with unknown flag = %d, want 1", code)
	}
}

func TestRun_NonexistentTemplate(t *testing.T) {
	code := Run([]string{"-template", "/does/not/exist.yaml"})
	if code != 1 {
		t.Errorf("Run with nonexistent template = %d, want 1", code)
	}
}

func TestRun_JSONDump(t *testing.T) {
	stackPath := filepath.Join(repoRoot(t), "examples", "synthetic-stack.yaml")

	var code int
	out := captureStdout(t, func() {
		code = Run([]string{"-template", stackPath, "-json"})
	})
	if code != 0 {
		t.Fatalf("Run -json = %d, want 0; output:\n%s", code, out)
	}

	var dump map[string]interface{}
	if err := json.Unmarshal([]byte(out), &dump); err != nil {
		t.Fatalf("-json output is not valid JSON: %v\noutput:\n%s", err, out)
	}
	if _, ok := dump["serverless"]; !ok {
		t.Errorf("expected a top-level \"serverless\" key in JSON dump, got keys: %v", keysOf(dump))
	}
	if _, ok := dump["ecs"]; !ok {
		t.Errorf("expected a top-level \"ecs\" key in JSON dump, got keys: %v", keysOf(dump))
	}
}

func TestRun_FullPipeline(t *testing.T) {
	stackPath := filepath.Join(repoRoot(t), "examples", "synthetic-stack.yaml")
	outDir := t.TempDir()

	var code int
	out := captureStdout(t, func() {
		code = Run([]string{"-template", stackPath, "-output", outDir})
	})
	if code != 0 {
		t.Fatalf("Run full pipeline = %d, want 0; output:\n%s", code, out)
	}

	if !strings.Contains(out, "TOTAL SERVERLESS") || !strings.Contains(out, "TOTAL ECS") {
		t.Errorf("expected cost summary in stdout, got:\n%s", out)
	}

	for _, f := range []string{"main.tf", "ecs.tf", "alb.tf"} {
		p := filepath.Join(outDir, "terraform", f)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected generated %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outDir, "report.md")); err != nil {
		t.Errorf("expected generated report.md: %v", err)
	}
}

func TestRun_UsageSidecarBadPathWarnsButContinues(t *testing.T) {
	stackPath := filepath.Join(repoRoot(t), "examples", "synthetic-stack.yaml")
	outDir := t.TempDir()

	var code int
	captureStdout(t, func() {
		code = Run([]string{"-template", stackPath, "-output", outDir, "-usage", "/does/not/exist.json"})
	})
	if code != 0 {
		t.Errorf("expected Run to continue with defaults despite a bad -usage path, got exit code %d", code)
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
