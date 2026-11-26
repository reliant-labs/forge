package cli

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// Tests for `forge generate --scope` (generate_scope.go): refresh one
// package's derived contract files without a whole-project run (forge
// friction #36).

func TestContractScopeCovers(t *testing.T) {
	s := contractScope{"internal/assigner", "internal/mcp/db"}
	for rel, want := range map[string]bool{
		"internal/assigner":       true,
		"internal/assigner/sub":   true,
		"internal/assignerx":      false,
		"internal/mcp/db":         true,
		"internal/mcp":            false,
		"internal/store":          false,
		"internal":                false,
		"internal/mcp/db/fixture": true,
	} {
		if got := s.covers(rel); got != want {
			t.Errorf("covers(%q) = %v, want %v", rel, got, want)
		}
	}
	for rel, want := range map[string]bool{
		"internal":          true,
		"internal/mcp":      true,
		"internal/assigner": false, // covered, not an ancestor
		"internal/store":    false,
		"internal/mc":       false,
	} {
		if got := s.leadsTo(rel); got != want {
			t.Errorf("leadsTo(%q) = %v, want %v", rel, got, want)
		}
	}
	if !contractScope(nil).covers("internal/anything") {
		t.Error("a nil scope must cover every package (the full run)")
	}
}

// writeScopeFixture lays out a module with two contract packages, alpha and
// beta, a forge:deps package gamma, and a plain package plain.
func writeScopeFixture(t *testing.T, root string) {
	t.Helper()
	mustWriteFile(t, filepath.Join(root, "go.mod"), "module example.com/proj\n\ngo 1.24\n")
	for _, name := range []string{"alpha", "beta"} {
		pkg := filepath.Join(root, "internal", name)
		mustWriteFile(t, filepath.Join(pkg, "contract.go"), "package "+name+`

import "context"

type Service interface {
	Ping(ctx context.Context) error
}

// Store is a dependency the package declares at its boundary.
type Store interface {
	Get(ctx context.Context, id string) (string, error)
}

type Deps struct{ Store Store }
`)
		mustWriteFile(t, filepath.Join(pkg, "client.go"), "package "+name+`

import "context"

type client struct{}

// New builds the package.
func New(Deps) Service { return &client{} }

func (c *client) Ping(context.Context) error { return nil }
`)
	}
	mustWriteFile(t, filepath.Join(root, "internal", "gamma", "deps.go"), "// forge:deps — consumer-declared interfaces for this package's Deps.\npackage gamma\n\ntype Clock interface{ Now() int64 }\n")
	mustWriteFile(t, filepath.Join(root, "internal", "plain", "plain.go"), "package plain\n")
}

func TestNormalizeGenerateScope(t *testing.T) {
	root := t.TempDir()
	writeScopeFixture(t, root)

	got, err := normalizeGenerateScope(root, []string{"./internal/beta/", "internal/alpha/...", filepath.Join(root, "internal", "alpha"), " internal/gamma "})
	if err != nil {
		t.Fatalf("normalizeGenerateScope: %v", err)
	}
	if want := (contractScope{"internal/alpha", "internal/beta", "internal/gamma"}); !slices.Equal(got, want) {
		t.Fatalf("scope = %v, want %v (normalized, deduplicated, sorted)", got, want)
	}
	if got, err := normalizeGenerateScope(root, []string{"internal"}); err != nil || !slices.Equal(got, contractScope{"internal"}) {
		t.Fatalf("scope internal = %v, %v; want every package under internal/", got, err)
	}
	if got, err := normalizeGenerateScope(root, nil); err != nil || got != nil {
		t.Fatalf("no --scope = %v, %v; want nil (the full run)", got, err)
	}

	for arg, wantMsg := range map[string]string{
		"proto/services":                  "only packages under internal/",
		"internal/missing":                "no such directory",
		"internal/plain":                  "no package there has a contract",
		filepath.Join(filepath.Dir(root)): "outside the project",
	} {
		if _, err := normalizeGenerateScope(root, []string{arg}); err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Errorf("--scope %q: err = %v, want one containing %q", arg, err, wantMsg)
		}
	}
}

// TestGenerateScopeIsNeverRecordedAsVerified: a narrowed run proves nothing
// about the tree, so it neither consults nor records the generate cache.
func TestGenerateScopeIsNeverRecordedAsVerified(t *testing.T) {
	flags := pipelineFlags{Cache: true}.withScope(contractScope{"internal/alpha"})
	if flags.Steps != contractsStepPreset {
		t.Fatalf("withScope set Steps = %q, want %q", flags.Steps, contractsStepPreset)
	}
	if m := generateCacheModeFor(flags); m.consult || m.record {
		t.Fatalf("cache mode for a --scope run = %+v, want neither consult nor record", m)
	}
	flags.Steps = "" // even if the preset were lost, the scope alone opts out
	if m := generateCacheModeFor(flags); m.consult || m.record {
		t.Fatalf("cache mode for a scope without its preset = %+v, want neither consult nor record", m)
	}
	if got := (pipelineFlags{Steps: "mocks"}).withScope(nil); got.Steps != "mocks" || got.Scope != nil {
		t.Fatalf("withScope(nil) changed the flags: %+v", got)
	}
}

func TestGenerateScopeRefusesOtherRunShapes(t *testing.T) {
	for _, other := range [][]string{{"--steps", "mocks"}, {"--templates-only"}, {"--check"}, {"--watch"}} {
		cmd := newGenerateCmd()
		cmd.SetArgs(append([]string{"--scope", "internal/alpha"}, other...))
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "scope") {
			t.Errorf("--scope with %v: err = %v, want the flags refused together", other, err)
		}
	}
}

// TestGenerateInternalPackageContractsIn_WritesOnlyTheScopeByteIdentically:
// a scoped run writes the scoped package's files exactly as the full run
// does, and touches no other package.
func TestGenerateInternalPackageContractsIn_WritesOnlyTheScopeByteIdentically(t *testing.T) {
	testkit.Slow(t, "execs go (the contract Resolver loads packages)")
	cfg := &config.ProjectConfig{Name: "proj"}

	full := t.TempDir()
	writeScopeFixture(t, full)
	t.Chdir(full)
	if err := generateInternalPackageContracts(".", cfg, nil); err != nil {
		t.Fatalf("full run: %v", err)
	}

	scoped := t.TempDir()
	writeScopeFixture(t, scoped)
	t.Chdir(scoped)
	if err := generateInternalPackageContractsIn(".", cfg, nil, contractScope{"internal/alpha"}); err != nil {
		t.Fatalf("scoped run: %v", err)
	}

	for _, f := range []string{"mock_gen.go", "middleware_gen.go"} {
		want, err := os.ReadFile(filepath.Join(full, "internal", "alpha", f))
		if err != nil {
			t.Fatalf("full run wrote no alpha/%s: %v", f, err)
		}
		got, err := os.ReadFile(filepath.Join(scoped, "internal", "alpha", f))
		if err != nil {
			t.Fatalf("scoped run wrote no alpha/%s: %v", f, err)
		}
		if string(got) != string(want) {
			t.Errorf("alpha/%s differs between the scoped and the full run:\n--- full\n%s\n--- scoped\n%s", f, want, got)
		}
	}
	for _, out := range []string{"internal/beta/mock_gen.go", "internal/beta/middleware_gen.go", "internal/gamma/mock_gen.go"} {
		if _, err := os.Stat(filepath.Join(full, out)); err != nil {
			t.Fatalf("fixture check: the full run wrote no %s: %v", out, err)
		}
		if _, err := os.Stat(filepath.Join(scoped, out)); !os.IsNotExist(err) {
			t.Errorf("the scoped run wrote %s, outside its scope (err=%v)", out, err)
		}
	}
}

// TestCompileScopedPackages_AttributesErrors: a compile error in a scoped
// package is the scope's; one in a package the scope imports is reported
// apart; one in a package the scope never reaches is not compiled at all —
// so another agent's half-edited package does not block a scoped run.
func TestCompileScopedPackages_AttributesErrors(t *testing.T) {
	testkit.Slow(t, "execs go list -export")
	root := t.TempDir()
	mustWriteFile(t, filepath.Join(root, "go.mod"), "module example.com/proj\n\ngo 1.24\n")
	mustWriteFile(t, filepath.Join(root, "internal", "a", "a.go"), "package a\n\nimport \"example.com/proj/internal/b\"\n\nvar _ = b.V\n")
	mustWriteFile(t, filepath.Join(root, "internal", "b", "b.go"), "package b\n\nvar V = 1\n")
	mustWriteFile(t, filepath.Join(root, "pkg", "c", "c.go"), "package c\n\nfunc f() { undefinedInC() }\n")
	scope := contractScope{"internal/a"}

	rep, err := compileScopedPackages(root, scope)
	if err != nil {
		t.Fatalf("compileScopedPackages: %v", err)
	}
	if rep.Scoped != "" || rep.Elsewhere != "" {
		t.Fatalf("a scope that compiles reported %+v; the broken pkg/c is outside it and not imported", rep)
	}

	mustWriteFile(t, filepath.Join(root, "internal", "b", "broken.go"), "package b\n\nfunc g() { undefinedInB() }\n")
	rep, err = compileScopedPackages(root, scope)
	if err != nil {
		t.Fatalf("compileScopedPackages: %v", err)
	}
	if rep.Scoped != "" || !strings.Contains(rep.Elsewhere, "undefinedInB") {
		t.Fatalf("a broken dependency: report = %+v, want its error outside the scope only", rep)
	}
	if strings.Contains(rep.Elsewhere, "undefinedInC") {
		t.Fatalf("pkg/c, which the scope does not import, was compiled: %s", rep.Elsewhere)
	}

	if err := os.Remove(filepath.Join(root, "internal", "b", "broken.go")); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(root, "internal", "a", "broken.go"), "package a\n\nfunc g() { undefinedInA() }\n")
	ctx := &pipelineContext{ProjectDir: root, Scope: scope}
	stepErr := stepValidateScopedPackages(ctx)
	var ve *validateBuildError
	if !errors.As(stepErr, &ve) || !strings.Contains(ve.Output, "undefinedInA") {
		t.Fatalf("a broken scoped package: step err = %v, want a validateBuildError quoting it (the run rolls back)", stepErr)
	}

	ctx = &pipelineContext{ProjectDir: root, Scope: scope, KeepOnBuildFailure: true}
	if err := stepValidateScopedPackages(ctx); err != nil {
		t.Fatalf("--keep-on-build-failure: step err = %v, want the failure kept as a soft failure", err)
	}
	if ctx.keptBuildFailure == nil || !slices.Contains(ctx.SoftFailures, scopedValidateStep) {
		t.Fatalf("--keep-on-build-failure: kept=%v soft=%v, want the failure kept and the step failed", ctx.keptBuildFailure, ctx.SoftFailures)
	}
}
