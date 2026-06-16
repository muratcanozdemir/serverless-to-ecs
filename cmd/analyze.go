package cmd

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"serverless-to-ecs/internal/cost"
	"serverless-to-ecs/internal/emit"
	"serverless-to-ecs/internal/model"
	"serverless-to-ecs/internal/parser"
	"serverless-to-ecs/internal/report"
)

// Run is the CLI entrypoint. Returns exit code.
func Run() int {
	templatePath := flag.String("template", "", "Path to CFN/SAM template (JSON or YAML)")
	region := flag.String("region", "eu-central-1", "AWS region for pricing")
	usagePath := flag.String("usage", "", "Path to usage profile sidecar JSON (optional)")
	outputDir := flag.String("output", "output", "Directory for generated artifacts")
	llmEndpoint := flag.String("llm-endpoint", "", "OpenAI-compatible API base URL (e.g. http://localhost:8080/v1)")
	llmModel := flag.String("llm-model", "", "Model name for LLM report generation")
	jsonDump := flag.Bool("json", false, "Dump the full analysis as JSON and exit")
	flag.Parse()

	if *templatePath == "" {
		fmt.Fprintln(os.Stderr, "error: -template is required")
		flag.Usage()
		return 1
	}

	// Parse template.
	g, err := parser.ParseFile(*templatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Load pricing.
	pricing, err := cost.LoadPricing()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}
	prices, err := pricing.ForRegion(*region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	// Build usage profile.
	usage := cost.DefaultProfile(g)
	if *usagePath != "" {
		if err := usage.LoadSidecar(*usagePath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v (continuing with defaults)\n", err)
		}
	}

	// Estimate costs.
	groups := cost.GroupServices(g)
	estimate := cost.EstimateCosts(g, usage, prices, *region)

	if *jsonDump {
		return dumpJSON(estimate)
	}

	printSummary(g, estimate, groups)

	// Generate Terraform.
	tfDir := filepath.Join(*outputDir, "terraform")
	if err := emit.EmitTerraform(g, groups, *templatePath, *region, tfDir); err != nil {
		fmt.Fprintf(os.Stderr, "error generating terraform: %v\n", err)
		return 1
	}
	fmt.Printf("\nTerraform written to %s/\n", tfDir)

	// Generate report.
	reportOpts := report.Options{
		LLMEndpoint: *llmEndpoint,
		LLMModel:    *llmModel,
		OutputDir:   *outputDir,
		Region:      *region,
	}
	if err := report.Generate(g, estimate, groups, reportOpts); err != nil {
		fmt.Fprintf(os.Stderr, "error generating report: %v\n", err)
		return 1
	}
	mode := "data-only"
	if *llmEndpoint != "" {
		mode = "LLM-generated"
	}
	fmt.Printf("Report written to %s/report.md (%s)\n", *outputDir, mode)

	return 0
}

func dumpJSON(v interface{}) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "error: json encode: %v\n", err)
		return 1
	}
	return 0
}

func printSummary(g *model.Graph, est *cost.Estimate, groups []cost.ServiceGroup) {
	fmt.Printf("Template: %s\n", g.Description)
	fmt.Printf("SAM:      %v\n", g.IsSAM)
	fmt.Printf("Region:   %s\n", est.Region)
	fmt.Printf("Accuracy: %s\n", est.Accuracy)
	fmt.Println()

	// Resource inventory.
	fmt.Printf("Resource inventory:\n")
	fmt.Printf("  Lambda functions:     %d\n", len(g.Lambdas))
	fmt.Printf("  API Gateways:         %d\n", len(g.APIs))
	fmt.Printf("  API routes:           %d\n", len(g.Routes))
	fmt.Printf("  Step Functions:       %d\n", len(g.StepFuncs))
	fmt.Printf("  EventBridge rules:    %d\n", len(g.Rules))
	fmt.Printf("  SQS queues:           %d\n", len(g.Queues))
	fmt.Printf("  SNS topics:           %d\n", len(g.Topics))
	fmt.Printf("  DynamoDB tables:      %d\n", len(g.Tables))
	fmt.Printf("  Unsupported:          %d\n", len(g.Unsupported))
	fmt.Println()

	// Lambda details.
	if len(g.Lambdas) > 0 {
		fmt.Println("Lambda functions:")
		for _, fn := range g.Lambdas {
			fmt.Printf("  %-35s %s  %4dMB  %3ds\n",
				fn.FunctionName, fn.Runtime, fn.MemoryMB, fn.TimeoutSec)
		}
		fmt.Println()
	}

	// Step Functions.
	if len(g.StepFuncs) > 0 {
		fmt.Println("Step Functions:")
		for _, sf := range g.StepFuncs {
			fmt.Printf("  %-35s pattern=%-12s states=%d tasks=%d\n",
				sf.Name, sf.Pattern, sf.StateCount, len(sf.TaskTargets))
		}
		fmt.Println()
	}

	// Cost: current serverless.
	fmt.Println("═══ Current serverless cost (monthly) ═══")
	fmt.Println()
	if len(est.Serverless.Lambda) > 0 {
		fmt.Println("  Lambda:")
		for _, lc := range est.Serverless.Lambda {
			fmt.Printf("    %-33s $%8.2f  (%d invocations, %.0f GB-s)\n",
				lc.FunctionName, lc.Total, lc.Invocations, lc.GBSeconds)
		}
	}
	if len(est.Serverless.APIGateway) > 0 {
		fmt.Println("  API Gateway:")
		for _, c := range est.Serverless.APIGateway {
			fmt.Printf("    %-33s $%8.2f  (%s)\n", c.Name, c.Total, c.Detail)
		}
	}
	if len(est.Serverless.StepFunctions) > 0 {
		fmt.Println("  Step Functions:")
		for _, c := range est.Serverless.StepFunctions {
			fmt.Printf("    %-33s $%8.2f  (%s)\n", c.Name, c.Total, c.Detail)
		}
	}
	if len(est.Serverless.EventBridge) > 0 {
		fmt.Println("  EventBridge:")
		for _, c := range est.Serverless.EventBridge {
			fmt.Printf("    %-33s $%8.2f  (%s)\n", c.Name, c.Total, c.Detail)
		}
	}
	if len(est.Serverless.SQS) > 0 {
		fmt.Println("  SQS:")
		for _, c := range est.Serverless.SQS {
			fmt.Printf("    %-33s $%8.2f  (%s)\n", c.Name, c.Total, c.Detail)
		}
	}
	if len(est.Serverless.DynamoDB) > 0 {
		fmt.Println("  DynamoDB:")
		for _, c := range est.Serverless.DynamoDB {
			fmt.Printf("    %-33s $%8.2f  (%s)\n", c.Name, c.Total, c.Detail)
		}
	}
	fmt.Printf("\n  TOTAL SERVERLESS:                    $%8.2f/month\n", est.Serverless.Total)
	fmt.Println()

	// Cost: projected ECS.
	fmt.Println("═══ Projected ECS cost (monthly) ═══")
	fmt.Println()
	if len(est.ECS.Services) > 0 {
		fmt.Println("  ECS Fargate services:")
		for _, svc := range est.ECS.Services {
			fmt.Printf("    %-33s $%8.2f  (%d tasks × %.2f vCPU / %.1f GB)\n",
				svc.Name, svc.Total, svc.Tasks, svc.VCPUs, svc.MemoryGB)
		}
	}
	fmt.Printf("  ALB:                                   $%8.2f\n", est.ECS.ALB)
	fmt.Printf("  Retained (DynamoDB, SQS, SNS):         $%8.2f\n", est.ECS.Retained)
	fmt.Printf("\n  TOTAL ECS:                           $%8.2f/month\n", est.ECS.Total)
	fmt.Println()

	// Savings summary.
	fmt.Println("═══ Savings ═══")
	fmt.Printf("  Current:   $%.2f/month\n", est.Savings.CurrentMonthly)
	fmt.Printf("  Projected: $%.2f/month\n", est.Savings.ProjectedMonthly)
	delta := est.Savings.DeltaAbsolute
	pct := est.Savings.DeltaPercent
	if delta > 0 {
		fmt.Printf("  Savings:   $%.2f/month (%.1f%%)\n", delta, pct)
	} else {
		fmt.Printf("  Increase:  $%.2f/month (%.1f%%)\n", -delta, -pct)
	}

	// Service grouping.
	if len(groups) > 0 {
		fmt.Println()
		fmt.Println("═══ Proposed service grouping ═══")
		for _, grp := range groups {
			fmt.Printf("  %-30s [%s] %v\n", grp.Name, grp.Type, grp.LambdaIDs)
			fmt.Printf("    reason: %s\n", grp.Reason)
		}
	}

	if len(g.Unsupported) > 0 {
		fmt.Println()
		fmt.Println("Unsupported resources (detected, not analyzed):")
		for _, u := range g.Unsupported {
			fmt.Printf("  %-35s %s\n", u.LogicalID, u.ResourceType)
		}
	}
}
