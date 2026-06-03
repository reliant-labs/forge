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
