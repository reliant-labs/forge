// Regression guard for the kalshi-trader friction report
// "forge add worker runs the full pipeline and scaffolds a Next.js
// dashboard" (forge-add-worker-runs-full-pipeline).
//
// Symptom: `forge add worker bar --kind cron` on a freshly-scaffolded
// `forge new x --kind service` project (no `--frontend`) was reported
// to (a) scaffold a complete `frontends/dashboard/` Next.js tree with
// node_modules, (b) append a `frontends:` block to forge.yaml, and
// (c) flip `features.frontend: false → true`.
//
// On audit, the current `runAddWorker` code path does NOT exhibit any
// of those side effects: every frontend-tagged step in the generate
// pipeline gates on either `FrontendEnabled()` (false in the friction
// scenario) or `len(cfg.Frontends) > 0` (zero), and `cfg.Features.Frontend`
// is only written from `runAddFrontend` — never from `runAddWorker`.
//
// This file pins those invariants so a future refactor that
// accidentally relaxes a gate (e.g. drops the `len(Frontends) > 0`
// check) or introduces a second write site for `Features.Frontend`
// fails CI before the regression ships.
package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestAddWorkerPipelineSkipsFrontendSteps reconstructs the
// pipelineContext that `runGeneratePipeline` would see during
// `forge add worker bar --kind cron` on a freshly-scaffolded
// service-only project, and asserts every step tagged "frontend" is
// gated OFF.
//
// Mirrors the friction repro exactly:
//   - cfg.Kind = "service"
//   - cfg.Features.Frontend = &false (explicit, from `forge new`)
//   - cfg.Services has one worker entry
//   - cfg.Frontends is empty
//   - proto/services/ is empty (no --service was passed)
//   - workers/ has the just-scaffolded bar/ dir (HasWorkers=true)
//
// If any frontend-tagged gate returns true here, the pipeline would
// proceed to render a Next.js dashboard — the exact failure mode the
// friction report describes.
func TestAddWorkerPipelineSkipsFrontendSteps(t *testing.T) {
	frontendOff := false
	ctx := &pipelineContext{
		ProjectDir: ".",
		AbsPath:    "/abs/.",
		Cfg: &config.ProjectConfig{
			Name:       "x",
			ModulePath: "github.com/x/x",
			Kind:       "service",
			Features: config.FeaturesConfig{
				Frontend: &frontendOff,
			},
			Services: []config.ServiceConfig{
				{Name: "bar", Type: "worker", Kind: "cron", Path: "workers/bar", Schedule: "*/5 * * * *"},
			},
			// No Frontends entries.
		},
		HasServices:  false,
		HasWorkers:   true,
		HasOperators: false,
	}

	var leaked []string
	for _, step := range generateSteps() {
		if step.Tag != "frontend" {
			continue
		}
		if step.Gate(ctx) {
			leaked = append(leaked, step.Name)
		}
	}
	if len(leaked) > 0 {
		t.Errorf("frontend pipeline step(s) gated ON during `forge add worker` on a service-only project:\n  - %s\n\n"+
			"This would scaffold Next.js artifacts the user never asked for. See friction "+
			"forge-add-worker-runs-full-pipeline (kalshi-trader migration round).",
			strings.Join(leaked, "\n  - "))
	}
}

// TestAddWorkerNoNewFeaturesFrontendWriteSite is a structural pairing:
// the only legal write sites for `cfg.Features.Frontend` in the cli
// package are `runAddFrontend` (add.go) and the `--disable` handler
// in `runNew` (new.go). The friction report claimed
// `forge add worker` flipped `features.frontend: false → true`; on
// inspection no such write exists today. This test fails if a future
// refactor introduces a third write site, which would risk
// reintroducing the same regression.
func TestAddWorkerNoNewFeaturesFrontendWriteSite(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob cli sources: %v", err)
	}
	allowed := map[string][]string{
		"add.go": {"cfg.Features.Frontend = &frontendOn"},
		"new.go": {"gen.Features.Frontend = f"},
	}
	var offenders []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, raw := range strings.Split(string(data), "\n") {
			line := strings.TrimSpace(raw)
			// Match only true assignments with " = " (single =, space-
			// padded). Read-only references like
			// `cfg.Features.FrontendEnabled()` or
			// `cfg.Features.Frontend != nil` don't match.
			if !strings.Contains(line, "Features.Frontend = ") {
				continue
			}
			matched := false
			for _, ok := range allowed[f] {
				if strings.Contains(line, ok) {
					matched = true
					break
				}
			}
			if !matched {
				offenders = append(offenders, f+": "+line)
			}
		}
	}
	if len(offenders) > 0 {
		t.Errorf("unexpected new write site(s) to cfg.Features.Frontend:\n  %s\n\n"+
			"Only runAddFrontend (add.go) and runNew's --disable handler (new.go) may "+
			"write this field. Any other write risks reintroducing the kalshi-trader "+
			"friction forge-add-worker-runs-full-pipeline where `forge add worker` was "+
			"reported to flip features.frontend on.",
			strings.Join(offenders, "\n  "))
	}
}

// TestAddWorkerUsesBootstrapOnlyScope is the structural pairing for the
// cp-forge port-workers friction: `forge add worker` × 7 rewrote 5
// UNRELATED Tier-1 files per call because runAddWorker invoked the full
// generate pipeline. Fix: the worker path now passes Scope:
// "bootstrap-only" so only the bootstrap regen subset runs.
//
// This test pins that contract by scanning add.go for the worker-path
// call site and asserting it passes the bootstrap-only scope. A future
// refactor that accidentally drops the Scope (or switches it back to
// the unscoped runGeneratePipeline) trips the test before the regression
// ships.
func TestAddWorkerUsesBootstrapOnlyScope(t *testing.T) {
	data, err := os.ReadFile("add.go")
	if err != nil {
		t.Fatalf("read add.go: %v", err)
	}
	src := string(data)
	// Find the runAddWorker body. The function name is unique; the
	// generate-pipeline invocation we care about sits between the
	// definition and the closing brace at indent 0.
	idx := strings.Index(src, "func runAddWorker(")
	if idx < 0 {
		t.Fatal("runAddWorker not found in add.go")
	}
	// Look for the closing of runAddWorker — scan to the next "\nfunc "
	// at top level. This is a coarse boundary but the function isn't long
	// enough to need an AST-grade approach.
	tail := src[idx:]
	end := strings.Index(tail, "\nfunc ")
	if end < 0 {
		end = len(tail)
	}
	body := tail[:end]
	if !strings.Contains(body, `Scope: "bootstrap-only"`) {
		t.Errorf("runAddWorker must invoke the generate pipeline with Scope: \"bootstrap-only\". " +
			"Running the unscoped pipeline rewrites unrelated Tier-1 files " +
			"(.github/workflows/ci.yml, cmd/server.go, frontend mocks, pkg/config/config.go) " +
			"that the worker scaffold has no business touching. " +
			"See FRICTION cp-forge-2026-06-03 port-workers.")
	}
	if strings.Contains(body, "runGeneratePipeline(root, false, false)") {
		t.Errorf("runAddWorker is still calling the unscoped runGeneratePipeline. " +
			"Switch to runGeneratePipelineFlags(root, pipelineFlags{Scope: \"bootstrap-only\"}).")
	}
}

// TestScopedStepAllowlistMembersExist guards against typo / rename drift
// between scopedStepAllowlist (generate_pipeline.go) and generateSteps().
// Every key in every scope's allowlist MUST match a step.Name in the
// canonical plan — otherwise a renamed step would silently fall out of
// the scoped pipeline and produce a no-op generate.
func TestScopedStepAllowlistMembersExist(t *testing.T) {
	stepNames := map[string]bool{}
	for _, step := range generateSteps() {
		stepNames[step.Name] = true
	}
	for scope, allow := range scopedStepAllowlist {
		for name := range allow {
			if !stepNames[name] {
				t.Errorf("scope %q allowlists step %q, but no GenStep with that name exists in generateSteps(). "+
					"Either the step was renamed (update the allowlist) or the name has a typo.",
					scope, name)
			}
		}
	}
}

// TestBootstrapOnlyScopeExcludesStompedSteps pins the FRICTION-named
// step set: the bootstrap-only scope must NOT run any of the steps whose
// outputs were stomped in the cp-forge port-workers report. If a future
// refactor adds one of these step names to the allowlist, this test
// trips before the regression hits a user.
func TestBootstrapOnlyScopeExcludesStompedSteps(t *testing.T) {
	allow := scopedStepAllowlist["bootstrap-only"]
	if allow == nil {
		t.Fatal("scopedStepAllowlist is missing the bootstrap-only entry")
	}
	stomped := []string{
		"CI workflows",            // .github/workflows/ci.yml
		"config loader (proto/config)", // pkg/config/config.go (+ cmd/server.go re-render)
		"frontend mocks + transport",   // frontends/<name>/src/lib/mock-transport.ts
		"regenerate infra files",       // deploy/ / Dockerfile.* / etc.
		"per-env deploy config",        // deploy/ env-specific KCL
		"Grafana dashboards",           // observability dashboards
		"service stubs",                // service.go / handlers.go scaffolds
		"CRUD handlers",                // handlers/<svc>/handlers_crud_gen.go
		"authorizer",                   // handlers/<svc>/authorizer_gen.go
		"service mocks",                // internal/<svc>/mock_gen.go
		"frontend hooks",               // frontends/<name>/src/hooks/*-hooks.ts
		"frontend CRUD pages",          // frontends/<name>/src/app/<svc>/page.tsx
		"frontend nav + dashboard",     // frontends/<name>/src/components/nav.tsx
	}
	for _, name := range stomped {
		if allow[name] {
			t.Errorf("bootstrap-only scope must NOT include step %q — it was named in the cp-forge port-workers FRICTION report as one of the stomped emitters. "+
				"Adding a worker should not regenerate this output.",
				name)
		}
	}
}
