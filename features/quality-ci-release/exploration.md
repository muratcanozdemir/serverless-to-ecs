# Codebase Exploration: Quality, Test Coverage, CI/CD & Release

**Generated:** 2026-08-25 20:05
**Feature:** Harden `serverless-to-ecs` (a Go CLI that parses CFN/SAM templates, estimates AWS cost, and emits a Terraform ECS-migration strangler pattern) — find and fix bugs, raise test coverage, identify improvements, and stand up CI/CD + a release process. No prior `features/` pipeline artifacts exist for this repo; this is the first.

## Architecture & Conventions

Single linear pipeline, `main.go` → `cmd.Run()` (`cmd/analyze.go:18`):

1. `parser.ParseFile` → `*model.Graph` (YAML/JSON CFN/SAM → typed resource graph, two-pass: `extract.go` then `edges.go`)
2. `cost.LoadPricing()` (embedded `pricing.json`) → `pricing.ForRegion(region)`
3. `cost.DefaultProfile(g)`, optionally overridden by `usage.LoadSidecar(path)` (`-usage` flag)
4. `cost.GroupServices(g)` — clusters Lambdas into ECS service groups (API-backed, queue-processor, scheduled, orchestrated, prefix fallback)
5. `cost.EstimateCosts(...)` → current-serverless vs projected-ECS `*Estimate`
6. `-json` short-circuits to a raw dump; otherwise `printSummary` (cmd/analyze.go:109, ~130 lines, all formatting inline)
7. `emit.EmitTerraform(...)` renders `text/template` files from an embedded FS (`internal/emit/templates/*.tmpl`) into `<output>/terraform/`
8. `report.Generate(...)` — LLM-assisted (OpenAI-compatible `/chat/completions`, `report.go:251`) or `writeFallback` markdown data dump when `-llm-endpoint` is unset or the call fails

**Conventions:**
- Errors: idiomatic `fmt.Errorf("...: %w", err)` everywhere; zero sentinel errors, zero `panic` in non-test code. All error handling/printing centralized in `cmd/analyze.go`.
- Naming: standard Go, `Exported` cross-package API, lowercase per-file helpers. `*model.Graph` is the shared substrate through parser/cost/emit/report.
- Config: plain `flag` package, no config struct/viper, no globals — small `Options` structs or positional args threaded explicitly.
- **No interfaces anywhere** in the codebase (`grep '^type .* interface'` → zero hits). Every package uses concrete structs. This means the LLM HTTP client (`report.go:251`) has no seam for mocking — testing it requires a real/fake HTTP server (`httptest`).
- No `log` package; all output is `fmt.Print*`/`fmt.Fprintf(os.Stderr, ...)`, no levels.
- Minimal global state: read-only embedded `pricingJSON` (pricing.go:10), two static lookup tables (parser.go:273/284), embedded templates (emit.go:17).
- **Intentional silent-degrade pattern** used twice: usage-sidecar load failure warns and continues (cmd/analyze.go:56-58); LLM failure warns and falls back to `writeFallback` (report.go:112-116). Consistent, but failures never affect exit code — worth deciding whether that's desired going forward.
- Single external dependency: `gopkg.in/yaml.v3 v3.0.1` (used only in `parser.go` for YAML + manual CFN intrinsic-tag resolution). Everything else is stdlib (`net/http`, `text/template`, `encoding/json`, `embed`).

## Bugs Found (Parser / Cost)

Ranked by severity — all confirmed with file:line and a concrete trigger:

1. **HTTP API v2 routes never group correctly** (`internal/parser/edges.go:101-121`, `internal/cost/grouping.go:30-52`) — `resolveHTTPAPIRoute` sets `TargetRef` to the `AWS::ApiGatewayV2::Integration` logical ID (per the code's own comment), not the Lambda, so `GroupServices`'s Lambda-existence check always fails. Every Lambda behind an `AWS::ApiGatewayV2::Api` in a plain (non-SAM) CFN/CDK template is misgrouped, producing wrong ECS topology and cost. **Silent wrong-output bug, no crash.**
2. **`DefinitionString` parsed then discarded** (`internal/parser/extract.go:161-168`) — `_ = defStr // will be used in WBS 2...`. The standard (non-SAM) way to define a Step Functions state machine is completely ignored: `StateCount` stays 0, `Pattern` stays `SFNUnknown`, no `TaskTargets`, no `EdgeOrchestrates` edges, Step Functions cost undercounted (`usage.go:98` forces `AvgTransitionsPerExec = 1`). Trigger: any standard CFN template with `DefinitionString: !Sub | {...}`.
3. **Unchecked type assertion panics on malformed `Fn::GetAtt`** (`internal/parser/extract.go:455-456`) — `ga[0].(string)` / `ga[1].(string)` without `, ok`, unlike the safer `resolveRef` nearby. A Lambda env var like `{"Fn::GetAtt": [123, "Arn"]}` crashes the entire `ParseFile` call with an interface-conversion panic instead of returning an error.
4. **`Fn::Sub` array form unhandled** (`internal/parser/edges.go:273-276`, and likely elsewhere `Fn::Sub` is consumed) — only the string form is handled; the common 2-element array form (`["...${Var}...", {"Var": {"Ref": "X"}}]`) silently fails the type assertion, dropping the `EdgeInvokes` edge between API Gateway and Lambda.
5. **Cross-stack / literal-ARN event sources dropped silently** (`internal/parser/edges.go:41-47`) — `resolveEventSourceMapping` only understands `Ref`/`Fn::GetAtt`; a literal ARN or `Fn::ImportValue` (common for cross-stack SQS/Lambda refs) returns `""` and the trigger edge is lost with no diagnostic.

## Bugs Found (Emit / Report / Cmd)

Ranked by severity:

1. **Generated `alb.tf` is invalid HCL for every API-backed stack** (`internal/emit/templates/alb.tf.tmpl:43`) — the line `name = substr("{{ .Name }}", 0, min 32 (len "{{ .Name }}"))` is literal template text, not a `{{ }}` action, so it's emitted verbatim. `min 32 (len "x")` isn't valid HCL (should be `min(32, length("x"))`). The `substr`/`min`/`strLen` functions registered in `emit.go`'s `funcMap` (lines 90-96) are dead code — never invoked as template actions — so the ALB target-group 32-char truncation (an AWS hard limit) never actually runs. **Every stack with an API-backed Lambda produces Terraform that fails `terraform validate`.** Highest-priority fix.
2. **Undeclared resource reference for unresolvable scheduled rules** (`internal/emit/emit.go:182,187-188,384-387` + `templates/scheduled.tf.tmpl:21`) — `findServiceForLambdas` returns `"unknown"` when a schedule's targets don't resolve to a known Lambda (e.g. targets SNS/Step Functions/an unrecognized ARN). The template then emits `task_definition_arn = aws_ecs_task_definition.unknown.arn`, referencing a resource that doesn't exist. CLI reports success with no warning.
3. **Unescaped Lambda env vars interpolated into HCL/JSON string literals** (`internal/emit/emit.go:314-324` + `templates/ecs.tf.tmpl:44`) — `Key`/`Value` from CFN `Environment.Variables` are inserted raw. A value containing `"`, `\`, or newlines breaks the surrounding `jsonencode([...])`, or worst case injects extra HCL from a crafted template.
4. **Comment injection via `FunctionName`/env-var hints** (`internal/emit/emit.go:206-217` + `templates/main.tf.tmpl:72`) — emitted raw inside a `#`-comment with no newline stripping; a `\n` in the value escapes the comment and becomes live Terraform.
5. **Byte-slice truncation can produce invalid UTF-8** (`internal/emit/emit.go:394-399`, `deriveProjectName`) — `name[:20]` truncates by byte index, not rune index; non-ASCII `Description` text can be cut mid-rune, corrupting every generated resource name.

No nil-pointer panics, leaked file handles, or unclosed HTTP bodies were found in `emit.go`/`report.go`/`cmd/analyze.go`/`main.go` — I/O error handling is otherwise solid there.

## Test Coverage

**Existing tests:**
- `internal/parser/parser_test.go` — one large integration-style test against `examples/synthetic-stack.yaml` (SAM template) asserting most resource types, plus a small `TestLoadJSON`. No table-driven style, no mocks.
- `internal/emit/emit_test.go` — golden-file testing (`TestEmitTerraform_GoldenFiles`, expects `examples/expected-output/terraform/*`, supports `-update` to regenerate) plus `TestEmitTerraform_AllFilesCreated` (minimal hand-built graph, checks non-empty output).

**Blocking issue:** `examples/expected-output/terraform/` doesn't exist on disk, so all 7 golden subtests fail right now — `go test ./...` is currently red. Fixing this (regenerate + commit goldens, ideally *after* fixing the `alb.tf` bug above so goldens aren't checked in broken) is the first step before any coverage work.

**Zero test coverage** (confirmed via `go test ./...` output — "no test files"):
- `internal/cost` (`estimate.go`, `pricing.go`, `grouping.go`, `usage.go`) — the tool's core value proposition (pricing math) is entirely untested. Priority targets: `EstimateCosts`/`lambdaCost`/`projectECSService` (GB-seconds, Fargate sizing tiers, ALB LCU, DynamoDB provisioned-vs-on-demand), `GroupServices`'s 5-tier heuristic and `extractPrefix`/`groupByPrefix` string logic, `usage.go`'s `rateToMonthly`/`cronToMonthly` regex+cron parsing and `LoadSidecar` merge logic, `pricing.go`'s `ForRegion`/`RegionList`.
- `internal/model` — no tests.
- `internal/report` — `writeFallback`/`buildContext` (pure, always-invoked when no LLM configured) untested despite being cheap to test with no I/O; `generateWithLLM` (the one real HTTP boundary in the codebase) untested — needs `httptest.Server` for success/non-200/malformed-JSON/fallback-on-error paths.
- `cmd` — `Run` (flag parsing, `-json` mode, error paths) untested; smoke-testable end-to-end with `examples/synthetic-stack.yaml`.

**Under-tested even within "tested" packages:**
- Parser: only `SFNChoice` pattern is asserted; `SFNParallel`/`SFNMap`/`SFNSequential` aren't. No plain (non-SAM) `AWS::ApiGateway::Method`/`AWS::Events::Rule` coverage, no malformed-input error-path coverage, no intrinsics beyond `!Ref`.
- Emit: only one golden scenario (SAM + REST API + SQS). No HTTP API, FIFO queue, multi-service-group, scheduled-only, or no-API/no-queue (empty file) scenarios.

**Reusable fixtures:** `examples/synthetic-stack.yaml` (covers most resource types — reusable across cost/grouping/report tests) and `internal/cost/pricing.json` (real embedded pricing data, no mocking needed).

## CI/CD & Release Readiness

Repo: `git@github.com:muratcanozdemir/serverless-to-ecs.git` → GitHub Actions is the natural CI choice. Single commit history (`8c7be34 first commit`), no tags, no other branches.

**Confirmed absent:** `.github/` (no CI at all), `Makefile`, `Dockerfile`, goreleaser config, `.gitignore`, `VERSION`/`CHANGELOG`, version-stamping (`-ldflags`, no `-version` flag, `main.go` has no build-info wiring).

**Confirmed present and problematic:** the compiled binary `serverless-to-ecs` (11.5MB, **Mach-O x86_64** — a macOS build artifact) is tracked in git at the repo root. With no `.gitignore`, this will keep happening.

**Current build/test health:**
- `go build .` — succeeds, clean.
- `go vet ./...` — succeeds, clean.
- `go test ./...` — **fails** (emit golden-file tests, see Test Coverage above; everything else either passes or has no tests).

## Cross-Cutting Priorities

Putting the pieces together, a sensible work order:

1. **Fix `alb.tf.tmpl`** (Bug #1, Emit) — it's the most severe correctness bug (every API-backed migration produces invalid Terraform) and must land before golden fixtures are regenerated, or the goldens bake in broken output.
2. **Regenerate and commit emit golden fixtures** so `go test ./...` is green — currently blocking, and blocking is different from "no coverage."
3. **Remove the committed Mach-O binary, add `.gitignore`** — quick, unblocks a clean release process and prevents recurrence.
4. **Fix remaining high-confidence bugs**: `DefinitionString` (#2 parser), HTTP API v2 grouping (#1 parser), unchecked `Fn::GetAtt` panic (#3 parser), `scheduled.tf` undeclared-resource reference (#2 emit) — these produce silently wrong output or crashes on realistic templates.
5. **Backfill tests**, prioritized by risk: `internal/cost` pricing/grouping/usage math first (core value prop, currently 0% covered), then `report.writeFallback`/`generateWithLLM` (cheap + the one HTTP boundary), then `cmd.Run` smoke test, then parser/emit branch-coverage gaps.
6. **Stand up CI**: GitHub Actions workflow running `go build`, `go vet`, `go test ./...` on PRs/push — straightforward once tests are green.
7. **Release process**: add `-ldflags` version stamping (or a `Version` var) tied to git tags, and either a minimal GitHub Actions release workflow (build + attach binaries on tag push) or goreleaser, given this is a single-binary Go CLI.

No conflicting conventions were found between areas — the codebase is small and consistent enough that fixes/tests/CI can be layered in without cross-cutting refactors, except that the total lack of interfaces means `report.generateWithLLM` testing must go through a real `httptest.Server` rather than a mock.
