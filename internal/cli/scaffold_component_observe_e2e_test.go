//go:build e2e

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EComponentObserveMarkerAndEnforceLint is the end-to-end proof of
// Phase 2 of the component-middleware feature:
//
//  1. `scaffold package --type adapter` STAMPS `// forge:constructor` on the
//     constructor (born instrumented) and a decorator is generated.
//  2. A SECOND package added BY HAND without the marker makes `forge lint`
//     emit ONE aggregated enforce-component-observe error naming it + all
//     three escapes.
//  3. `// forge:no-observe` on that package → the lint error is gone and no
//     decorator is generated.
//  4. `// forge:no-observe` on a single METHOD of the instrumented package →
//     that method's wrapper delegates DIRECTLY (no chain), siblings still
//     route through it.
//  5. `config.enforce_component_observe: off` → lint clean even with an
//     unmarked component present.
//  6. generate ×2 is idempotent; build + vet stay green.
func TestE2EComponentObserveMarkerAndEnforceLint(t *testing.T) {
	t.Parallel()
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "obsent", "--mod", "example.com/obsent", "--service", "api")
	projectDir := filepath.Join(dir, "obsent")
	addCorpusForgePkgReplace(t, projectDir)

	// ── (1) Scaffold stamps the marker + a decorator is generated ──────────
	runCmd(t, projectDir, forgeBin, "scaffold", "package", "checkout", "--type", "adapter")
	checkoutDir := filepath.Join(projectDir, "internal", "checkout")
	implSrc := readFileE2E(t, filepath.Join(checkoutDir, "adapter.go"))
	if !strings.Contains(implSrc, "// forge:constructor") {
		t.Fatalf("scaffolded constructor not stamped with // forge:constructor:\n%s", implSrc)
	}

	runCmd(t, projectDir, forgeBin, "generate")
	decoratorPath := filepath.Join(checkoutDir, "middleware_gen.go")
	assertPathExistsE2E(t, decoratorPath)

	// ── (2) Hand-add a SECOND package WITHOUT the marker → aggregated error ─
	notifyDir := filepath.Join(projectDir, "internal", "notify")
	writeFileE2E(t, filepath.Join(notifyDir, "contract.go"), `package notify

import "context"

type Service interface {
	Ping(ctx context.Context) error
}
`)
	// Undecided (pure-compute): Logger/Config only, no marker, no seam.
	notifyUndecided := `package notify

import (
	"context"
	"log/slog"

	"example.com/obsent/pkg/config"
)

type Deps struct {
	Logger *slog.Logger
	Config *config.Config
}

type service struct{ deps Deps }

func New(deps Deps) (Service, error) {
	return &service{deps: deps}, nil
}

func (s *service) Ping(ctx context.Context) error { return nil }
`
	writeFileE2E(t, filepath.Join(notifyDir, "service.go"), notifyUndecided)

	runCmd(t, projectDir, forgeBin, "generate")
	// Undecided → not instrumented → no decorator for notify.
	assertPathNotExistsE2E(t, filepath.Join(notifyDir, "middleware_gen.go"))

	out, err := runLintCaptureE2E(t, projectDir, forgeBin)
	if err == nil {
		t.Fatalf("forge lint should FAIL with an undecided component; output:\n%s", out)
	}
	for _, want := range []string{
		"notify",
		"have no observability decision",
		"// forge:constructor",
		"// forge:no-observe",
		"config.enforce_component_observe: off",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("enforce-component-observe error missing %q:\n%s", want, out)
		}
	}
	// Exactly ONE aggregated error naming the one undecided component.
	if !strings.Contains(out, "1 component(s) have no observability decision: notify") {
		t.Fatalf("expected ONE aggregated error naming notify:\n%s", out)
	}

	// ── (3) Opt notify OUT (package-level no-observe) → error gone ─────────
	notifyOptOut := strings.Replace(notifyUndecided,
		"func New(deps Deps) (Service, error) {",
		"// forge:no-observe\nfunc New(deps Deps) (Service, error) {", 1)
	writeFileE2E(t, filepath.Join(notifyDir, "service.go"), notifyOptOut)

	runCmd(t, projectDir, forgeBin, "generate")
	assertPathNotExistsE2E(t, filepath.Join(notifyDir, "middleware_gen.go"))

	out3, _ := runLintCaptureE2E(t, projectDir, forgeBin)
	// The enforce-component-observe rule itself must now pass — assert on its
	// clean line so other (unrelated) lint steps can't mask the signal.
	if !strings.Contains(out3, "every wired component has an observability decision") {
		t.Fatalf("enforce-component-observe should be clean after opt-out:\n%s", out3)
	}
	if strings.Contains(out3, "have no observability decision") {
		t.Fatalf("no component should be flagged after notify opts out:\n%s", out3)
	}

	// ── (4) Method-level no-observe on the instrumented package ────────────
	contractPath := filepath.Join(checkoutDir, "contract.go")
	contractSrc := readFileE2E(t, contractPath)
	contractSrc = strings.Replace(contractSrc,
		"HealthCheck(ctx context.Context) error",
		"HealthCheck(ctx context.Context) error\n\n\t// forge:no-observe\n\tPing(ctx context.Context) error",
		1)
	writeFileE2E(t, contractPath, contractSrc)
	implPath := filepath.Join(checkoutDir, "adapter.go")
	writeFileE2E(t, implPath, readFileE2E(t, implPath)+
		"\n\n// Ping is a hot-path probe excluded from the observe chain.\nfunc (s *service) Ping(ctx context.Context) error { return nil }\n")

	runCmd(t, projectDir, forgeBin, "generate")
	dec := readFileE2E(t, decoratorPath)
	// HealthCheck still routes through the chain.
	if !strings.Contains(dec, `observe.Run(ctx, o.chain, "checkout.HealthCheck"`) {
		t.Fatalf("HealthCheck must still route through the chain:\n%s", dec)
	}
	// Ping delegates DIRECTLY — no chain, no op name.
	if !strings.Contains(dec, "func (o *forgeMiddlewareService) Ping(ctx context.Context) error {\n\treturn o.inner.Ping(ctx)\n}") {
		t.Fatalf("Ping wrapper must delegate directly (no chain):\n%s", dec)
	}
	if strings.Contains(dec, `"checkout.Ping"`) {
		t.Fatalf("Ping must NOT be routed through the chain:\n%s", dec)
	}

	// ── (5) Kill-switch: enforce_component_observe: off → clean ────────────
	// Re-make notify undecided so there IS an unmarked component present.
	writeFileE2E(t, filepath.Join(notifyDir, "service.go"), notifyUndecided)
	yamlPath := filepath.Join(projectDir, "forge.yaml")
	yaml := readFileE2E(t, yamlPath)
	// Insert enforce_component_observe as a sibling of enforce_typed_access,
	// matching its exact indentation (yaml.v3 marshals with 4-space indent —
	// derive it from the existing line rather than hard-coding).
	lines := strings.Split(yaml, "\n")
	inserted := false
	for i, ln := range lines {
		if strings.Contains(ln, "enforce_typed_access:") {
			indent := ln[:len(ln)-len(strings.TrimLeft(ln, " "))]
			lines[i] = ln + "\n" + indent + `enforce_component_observe: "off"`
			inserted = true
			break
		}
	}
	if !inserted {
		t.Fatalf("forge.yaml has no enforce_typed_access line to anchor on:\n%s", yaml)
	}
	yaml = strings.Join(lines, "\n")
	writeFileE2E(t, yamlPath, yaml)

	runCmd(t, projectDir, forgeBin, "generate")
	out5, _ := runLintCaptureE2E(t, projectDir, forgeBin)
	if strings.Contains(out5, "have no observability decision") {
		t.Fatalf("off mode must skip the enforce-component-observe error entirely:\n%s", out5)
	}

	// ── (6) Idempotent generate + build + vet ──────────────────────────────
	runCmd(t, projectDir, forgeBin, "generate")
	if again := readFileE2E(t, decoratorPath); again != dec {
		t.Fatalf("decorator changed on a second generate (not idempotent)")
	}
	runCmd(t, projectDir, "go", "build", "./...")
	runCmd(t, projectDir, "go", "vet", "./...")
}

// runLintCaptureE2E runs `forge lint` in projectDir and returns the combined
// output and error (non-nil when any gating linter fails). Unlike runCmd it
// tolerates a non-zero exit so the caller can assert on the enforce-component-
// observe verdict directly.
func runLintCaptureE2E(t *testing.T, projectDir, forgeBin string) (string, error) {
	t.Helper()
	cmd := exec.Command(forgeBin, "lint")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		"GOFLAGS=",
		"GOPROXY=https://proxy.golang.org,direct",
		// `forge lint` shells out to golangci-lint, which otherwise contends on
		// one machine-global lock with every other test's lint step. See the
		// block comment in scaffold_e2e_test.go.
		golangciLockIsolationEnv(t),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
