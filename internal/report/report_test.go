package report

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/model"
)

func testGraph() *model.Graph {
	g := model.NewGraph()
	g.Description = "test stack"
	g.Lambdas["Fn"] = &model.Lambda{
		LogicalID:    "Fn",
		FunctionName: "test-fn",
		Runtime:      "go1.x",
		MemoryMB:     256,
		TimeoutSec:   10,
	}
	g.APIs["Api"] = &model.APIGateway{LogicalID: "Api", Name: "TestAPI", Protocol: "REST"}
	g.Routes = append(g.Routes, model.APIRoute{APIID: "Api", Path: "/x", Method: "GET", TargetRef: "Fn"})
	g.AddEdge("Api", "Fn", model.EdgeInvokes, "GET /x")
	return g
}

func testEstimate(t *testing.T, g *model.Graph) *cost.Estimate {
	t.Helper()
	db, err := cost.LoadPricing()
	if err != nil {
		t.Fatalf("LoadPricing: %v", err)
	}
	prices, err := db.ForRegion("eu-central-1")
	if err != nil {
		t.Fatalf("ForRegion: %v", err)
	}
	usage := cost.DefaultProfile(g)
	return cost.EstimateCosts(g, usage, prices, "eu-central-1")
}

func TestBuildContext(t *testing.T) {
	g := testGraph()
	est := testEstimate(t, g)
	groups := cost.GroupServices(g)

	ctx := buildContext(g, est, groups, "eu-central-1")

	if ctx.Stack.Description != "test stack" {
		t.Errorf("Stack.Description = %q, want %q", ctx.Stack.Description, "test stack")
	}
	if ctx.Stack.Counts["lambdas"] != 1 {
		t.Errorf("Counts[lambdas] = %d, want 1", ctx.Stack.Counts["lambdas"])
	}
	if len(ctx.Lambdas) != 1 || ctx.Lambdas[0].FunctionName != "test-fn" {
		t.Errorf("expected one lambda detail for test-fn, got %+v", ctx.Lambdas)
	}
	if len(ctx.APIs) != 1 || len(ctx.APIs[0].Routes) != 1 {
		t.Errorf("expected one API with one route, got %+v", ctx.APIs)
	}
}

func TestGenerate_FallbackWhenNoLLMEndpoint(t *testing.T) {
	g := testGraph()
	est := testEstimate(t, g)
	groups := cost.GroupServices(g)
	outDir := t.TempDir()

	err := Generate(g, est, groups, Options{OutputDir: outDir, Region: "eu-central-1"})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"# Serverless-to-ECS Migration Report",
		"data-only (no LLM endpoint configured)",
		"## Resource Inventory",
		"test-fn",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("fallback report missing %q\n--- report ---\n%s", want, content)
		}
	}
}

func TestGenerate_LLMSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": "## Executive Summary\n\nMigrate now."}},
			},
		})
	}))
	defer srv.Close()

	g := testGraph()
	est := testEstimate(t, g)
	groups := cost.GroupServices(g)
	outDir := t.TempDir()

	err := Generate(g, est, groups, Options{
		OutputDir:   outDir,
		Region:      "eu-central-1",
		LLMEndpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "Migrate now.") {
		t.Errorf("expected LLM-generated content in report, got:\n%s", content)
	}
	if strings.Contains(content, "data-only") {
		t.Error("LLM-generated report should not contain the data-only fallback marker")
	}
}

func TestGenerate_LLMFailureFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()

	g := testGraph()
	est := testEstimate(t, g)
	groups := cost.GroupServices(g)
	outDir := t.TempDir()

	err := Generate(g, est, groups, Options{
		OutputDir:   outDir,
		Region:      "eu-central-1",
		LLMEndpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	if !strings.Contains(string(data), "data-only (no LLM endpoint configured)") {
		t.Error("expected Generate to fall back to the data-only report when the LLM call fails")
	}
}

func TestGenerateWithLLM_NoChoicesErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{}})
	}))
	defer srv.Close()

	g := testGraph()
	est := testEstimate(t, g)
	groups := cost.GroupServices(g)
	ctx := buildContext(g, est, groups, "eu-central-1")

	_, err := generateWithLLM(ctx, Options{LLMEndpoint: srv.URL})
	if err == nil {
		t.Fatal("expected an error when the LLM response has no choices")
	}
}

func TestWriteFallback_ResourceCountsSortedAlphabetically(t *testing.T) {
	g := testGraph()
	g.Queues["Q"] = &model.SQSQueue{LogicalID: "Q", QueueName: "q"}
	est := testEstimate(t, g)
	groups := cost.GroupServices(g)
	ctx := buildContext(g, est, groups, "eu-central-1")

	outPath := filepath.Join(t.TempDir(), "report.md")
	if err := writeFallback(ctx, outPath); err != nil {
		t.Fatalf("writeFallback: %v", err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}

	apisIdx := strings.Index(string(data), "| apis |")
	lambdasIdx := strings.Index(string(data), "| lambdas |")
	if apisIdx == -1 || lambdasIdx == -1 {
		t.Fatalf("expected both apis and lambdas rows in the inventory table:\n%s", data)
	}
	if apisIdx > lambdasIdx {
		t.Error("expected resource counts table to be in sorted (alphabetical) key order")
	}
}
