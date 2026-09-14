// File: internal/codegen/stub_error_fails_closed_test.go
//
// The auto-stub's default for a method whose ONLY result is `error`.
//
// The zero value of `error` is nil, and nil returned from a gate the
// application consults before acting means PERMITTED. So a synthesized
// stub that returns the zero value turns every such gate off — silently,
// in every test that does not override the Deps field. A measured dogfood
// run wrote a test asserting that an under-privileged caller could not
// obtain an upload URL; it failed, because the generated stub had let the
// call through. The application's own code was correct and the harness
// had disabled it.
//
// Forge already makes the opposite choice one generator over: MockService,
// projected from the same contract.go, returns contractkit.MockNotSet for
// a method whose Func field was never assigned. Two generated doubles for
// one interface with opposite safety defaults is the defect — this pins
// them to the same one.
//
// The rule is deliberately structural rather than name-based. A stub
// method returning ONLY error carries no value a caller can inspect, so
// nil is not "an empty result", it is an affirmative success the stub
// never computed. Matching on a naming convention instead would fail open
// for every gate spelled some other way, which is most of them — and this
// generator stubs EVERY interface on a handler's Deps, not a special
// category of them.
//
// The fixtures below use a quota gate rather than a permission one on
// purpose: the defect is a property of the SIGNATURE, and naming the
// fixture after one domain invites a future reader to narrow the rule to
// it.

package codegen

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// quotaStubFixture is a handler package whose Deps carry a cross-package
// gate seam — the shape the run hit. CheckQuota returns bare `error`
// (the fail-open case), OrgIDsFor returns a value plus error (which must
// keep its zero-value default: there is a real result to be empty).
func quotaStubFixture(t *testing.T) string {
	t.Helper()
	return writeStubFixtureModule(t, "internal/handlers/documents", map[string]string{
		"internal/policy/policy.go": `package policy

import "context"

// Tier is a named scalar, so the stub's zero for it is the universal form.
type Tier int

// Service is the gate a handler consults before acting.
type Service interface {
	CheckQuota(ctx context.Context, userID, orgID string, min Tier) error
	OrgIDsFor(ctx context.Context, userID string) ([]string, error)
	Close() error
}
`,
		"internal/handlers/documents/service.go": `package documents

import (
	"log/slog"

	"example.com/proj/internal/policy"
)

type Deps struct {
	Logger *slog.Logger
	Policy policy.Service
}
`,
	})
}

func methodsByName(t *testing.T, stub DepsAutoStub) map[string]InterfaceMethod {
	t.Helper()
	out := map[string]InterfaceMethod{}
	for _, m := range stub.Methods {
		out[m.Name] = m
	}
	return out
}

// TestComputeAutoStubs_ErrorOnlyMethodFailsClosed is the finding itself.
// `CheckQuota(...) error` must not stub to `return nil`: nil is ALLOW,
// and a test that never overrides the field would assert against a check
// that was switched off.
func TestComputeAutoStubs_ErrorOnlyMethodFailsClosed(t *testing.T) {
	handlerDir := quotaStubFixture(t)

	stubs, unresolved := computeAutoStubs(handlerDir, "")
	if len(unresolved) != 0 {
		t.Fatalf("policy.Service should resolve, got unresolved %v", unresolved)
	}
	if len(stubs) != 1 {
		t.Fatalf("expected exactly one auto-stub (Policy), got %d: %v", len(stubs), stubs)
	}
	byName := methodsByName(t, stubs[0])

	require, ok := byName["CheckQuota"]
	if !ok {
		t.Fatalf("missing CheckQuota in %v", stubs[0].Methods)
	}
	if require.ReturnStatement == "return nil" {
		t.Fatalf("CheckQuota stubs to `return nil`, which is ALLOW.\n"+
			"A handler test that does not override Deps.Policy runs with the check disabled, "+
			"and a bug that skips the gate passes its tests.\n"+
			"Want a fail-closed default, as MockService already emits for an unset method.\n"+
			"got: %q", require.ReturnStatement)
	}
	if !strings.Contains(require.ReturnStatement, "StubNotConfigured") {
		t.Errorf("CheckQuota should return the named stub-not-configured error so the "+
			"failure says WHICH method was left unconfigured.\ngot: %q", require.ReturnStatement)
	}
}

// TestComputeAutoStubs_ErrorOnlyAppliesToEveryErrorOnlyMethod pins that the
// rule is structural, not a name heuristic. `Close() error` is not a gate
// method and still returns only error — a stub cannot know it succeeded.
func TestComputeAutoStubs_ErrorOnlyAppliesToEveryErrorOnlyMethod(t *testing.T) {
	handlerDir := quotaStubFixture(t)

	stubs, _ := computeAutoStubs(handlerDir, "")
	if len(stubs) != 1 {
		t.Fatalf("expected one auto-stub, got %d", len(stubs))
	}
	closeM, ok := methodsByName(t, stubs[0])["Close"]
	if !ok {
		t.Fatalf("missing Close in %v", stubs[0].Methods)
	}
	if closeM.ReturnStatement == "return nil" {
		t.Errorf("Close() error stubs to `return nil` — a name-based rule that exempted it " +
			"would fail open for every gate spelled some other way")
	}
}

// TestComputeAutoStubs_ValuePlusErrorKeepsZeroValues is the blast-radius
// bound, and it is the reason this change is scoped to error-ONLY methods.
//
// A method returning (T, error) has a real result, so its zero value is a
// meaningful "nothing found" a caller can act on, and a stub that errored
// instead would break every legitimate test that reads through such a
// method to construct Deps. Only the error-only shape carries no value at
// all, which is what makes nil there an unearned success rather than an
// empty one.
func TestComputeAutoStubs_ValuePlusErrorKeepsZeroValues(t *testing.T) {
	handlerDir := quotaStubFixture(t)

	stubs, _ := computeAutoStubs(handlerDir, "")
	if len(stubs) != 1 {
		t.Fatalf("expected one auto-stub, got %d", len(stubs))
	}
	orgIDs, ok := methodsByName(t, stubs[0])["OrgIDsFor"]
	if !ok {
		t.Fatalf("missing OrgIDsFor in %v", stubs[0].Methods)
	}
	if orgIDs.ReturnStatement != "return nil, nil" {
		t.Errorf("a (T, error) method must keep its zero-value default — erroring there would "+
			"break tests that only need Deps to construct.\n got: %q\nwant: %q",
			orgIDs.ReturnStatement, "return nil, nil")
	}
}

// TestComputeAutoStubs_FailClosedStubCompiles is the assertion that the
// text-matching tests above cannot make. The fail-closed body is not a
// literal — it calls into pkg/testkit — so unlike `return nil` it can be
// emitted wrongly and still look plausible in a string comparison. The
// generated harness is regenerated with a forge:hash and cannot be
// hand-repaired, so "it compiles" has to be checked, not assumed.
//
// The fixture requires the real forge module so testkit resolves exactly
// as it does in a scaffolded project, where the template imports
// pkg/testkit unconditionally.
func TestComputeAutoStubs_FailClosedStubCompiles(t *testing.T) {
	forgeRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve forge root: %v", err)
	}

	root := t.TempDir()
	files := map[string]string{
		// pkg/testkit lives in the SEPARATE github.com/reliant-labs/forge/pkg
		// module (forge/pkg has its own go.mod), which is what a scaffolded
		// project requires. Getting this wrong is precisely the failure this
		// test exists to catch: the emitted call is valid Go and still does
		// not build if the module it names is not required.
		"go.mod": "module example.com/proj\n\ngo 1.24\n\n" +
			"require github.com/reliant-labs/forge/pkg v0.0.0\n\n" +
			"replace github.com/reliant-labs/forge/pkg => " +
			filepath.ToSlash(filepath.Join(forgeRoot, "pkg")) + "\n",
		"internal/policy/policy.go": `package policy

import "context"

type Tier int

type Service interface {
	CheckQuota(ctx context.Context, userID, orgID string, min Tier) error
	OrgIDsFor(ctx context.Context, userID string) ([]string, error)
}
`,
		"internal/handlers/documents/service.go": `package documents

import (
	"log/slog"

	"example.com/proj/internal/policy"
)

type Deps struct {
	Logger *slog.Logger
	Policy policy.Service
}
`,
	}
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
			t.Fatalf("mkdir: %v", mkErr)
		}
		if wErr := os.WriteFile(full, []byte(content), 0o644); wErr != nil {
			t.Fatalf("write %s: %v", path, wErr)
		}
	}

	handlerDir := filepath.Join(root, "internal", "handlers", "documents")
	stubs, _ := computeAutoStubs(handlerDir, "")
	if len(stubs) != 1 {
		t.Fatalf("expected one auto-stub, got %d", len(stubs))
	}
	stub := stubs[0]

	// Render the stub the way the template does: the struct, the
	// interface-satisfaction guard, and one method per entry.
	var b strings.Builder
	b.WriteString("package documents\n\nimport (\n")
	for _, imp := range stub.ExtraImports {
		fmt.Fprintf(&b, "\t%s %q\n", imp.Alias, imp.Path)
	}
	b.WriteString("\t\"github.com/reliant-labs/forge/pkg/testkit\"\n)\n\n")
	fmt.Fprintf(&b, "type %s struct{}\n\n", stub.StubType)
	fmt.Fprintf(&b, "var _ %s = %s{}\n\n", stub.InterfaceQualified, stub.StubType)
	for _, m := range stub.Methods {
		results := ""
		if m.Results != "" {
			results = " " + m.Results
		}
		fmt.Fprintf(&b, "func (%s) %s(%s)%s { %s }\n\n",
			stub.StubType, m.Name, m.Params, results, m.ReturnStatement)
	}
	src := b.String()

	if wErr := os.WriteFile(filepath.Join(handlerDir, "helpers_gen_test_stub.go"), []byte(src), 0o644); wErr != nil {
		t.Fatalf("write stub: %v", wErr)
	}

	// The fixture depends on the real forge module through a replace, so it
	// needs a resolved go.mod/go.sum before packages.Load will type-check
	// it. Tidy AFTER the stub is on disk — tidy resolves the imports it can
	// see, and pkg/testkit is imported only by the file just written.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = root
	// GOWORK=off: forge's own go.work must not capture the fixture module.
	tidy.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOWORK=off")
	if out, tErr := tidy.CombinedOutput(); tErr != nil {
		t.Skipf("go mod tidy unavailable for the fixture module (%v): %s", tErr, out)
	}

	pkgs, lErr := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedDeps |
			packages.NeedImports | packages.NeedSyntax,
		Dir: handlerDir,
		Env: append(os.Environ(), "GOWORK=off"),
	}, ".")
	if lErr != nil {
		t.Fatalf("load generated stub package: %v", lErr)
	}
	var problems []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			problems = append(problems, e.Error())
		}
	}
	if len(problems) > 0 {
		t.Fatalf("generated fail-closed stub does not compile:\n  %s\n--- generated ---\n%s--- end ---",
			strings.Join(problems, "\n  "), src)
	}
}

// TestComputeAutoStubs_LocalErrorOnlyMethodFailsClosed covers the other
// branch. computeAutoStubs resolves a bare-identifier Deps type through
// ParseLocalInterfaces, an entirely separate path from the cross-package
// resolver — a fix applied to only one of them leaves half the surface
// failing open.
func TestComputeAutoStubs_LocalErrorOnlyMethodFailsClosed(t *testing.T) {
	handlerDir := writeStubFixtureModule(t, "internal/handlers/documents", map[string]string{
		"internal/handlers/documents/service.go": `package documents

import (
	"context"
	"log/slog"
)

// QuotaChecker is declared IN this package, so the local branch handles it.
type QuotaChecker interface {
	CheckQuota(ctx context.Context, userID, orgID string) error
}

type Deps struct {
	Logger *slog.Logger
	Quota  QuotaChecker
}
`,
	})

	stubs, _ := computeAutoStubs(handlerDir, "")
	if len(stubs) != 1 {
		t.Fatalf("expected one auto-stub (Quota), got %d: %v", len(stubs), stubs)
	}
	require, ok := methodsByName(t, stubs[0])["CheckQuota"]
	if !ok {
		t.Fatalf("missing CheckQuota in %v", stubs[0].Methods)
	}
	if require.ReturnStatement == "return nil" {
		t.Errorf("a LOCALLY-declared interface's error-only method stubs to ALLOW — the local " +
			"and cross-package branches must agree, or half the surface still fails open")
	}
}
