// File: internal/codegen/cross_pkg_interface_test.go
//
// Tests for ResolveCrossPkgInterface + the new selector branch of
// computeAutoStubs. These are heavier than the rest of the codegen
// tests because they build a tiny on-disk Go module that
// golang.org/x/tools/go/packages can actually load — the resolver is
// real-go-types-based, so a fixture has to be real Go source.

package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestModule lays out a temp Go module with:
//
//	<root>/go.mod                     (module example.com/proj)
//	<root>/internal/repo/repo.go      (interface declaration the handler imports)
//	<root>/handlers/billing/service.go (Deps struct referencing repo.Repository)
//
// Returns the handler dir so callers can pass it straight to the
// auto-stub helpers.
//
// Each test variant tweaks one of the three files via the maps; passing
// nil keeps the default content.
func writeTestModule(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()

	defaults := map[string]string{
		"go.mod": `module example.com/proj

go 1.22
`,
		"internal/repo/repo.go": `package repo

import "context"

// Repository is a cross-package interface the billing handler depends on.
type Repository interface {
	GetByID(ctx context.Context, id string) (*User, error)
	List(ctx context.Context) ([]User, error)
	Delete(ctx context.Context, id string) error
}

// User is referenced by Repository's method signatures.
type User struct {
	ID   string
	Name string
}
`,
		"handlers/billing/service.go": `package billing

import (
	"log/slog"

	"example.com/proj/internal/repo"
)

// Deps is the billing service's dependency graph.
type Deps struct {
	Logger *slog.Logger
	Repo   repo.Repository
}
`,
	}

	for path, content := range defaults {
		if override, ok := files[path]; ok {
			content = override
		}
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	return filepath.Join(root, "handlers/billing")
}

// TestResolveCrossPkgInterface_HappyPath verifies that a Deps field
// typed `repo.Repository` resolves to the right interface, with all
// three methods and their fully qualified parameter / result types.
func TestResolveCrossPkgInterface_HappyPath(t *testing.T) {
	handlerDir := writeTestModule(t, nil)

	res, ok := ResolveCrossPkgInterface(handlerDir, "repo", "Repository")
	if !ok {
		t.Fatal("expected resolver to succeed for repo.Repository")
	}

	if res.PackageName != "repo" {
		t.Errorf("PackageName = %q, want %q", res.PackageName, "repo")
	}
	if res.PackagePath != "example.com/proj/internal/repo" {
		t.Errorf("PackagePath = %q, want %q", res.PackagePath, "example.com/proj/internal/repo")
	}
	if len(res.Methods) != 3 {
		t.Fatalf("Methods len = %d, want 3 (GetByID, List, Delete)", len(res.Methods))
	}

	// Spot-check signatures. types.TypeString renders pointer / slice
	// types in canonical form, so we can string-match exactly.
	wantSig := map[string]struct {
		params  string
		results string
	}{
		"GetByID": {params: "ctx context.Context, id string", results: "(*repo.User, error)"},
		"List":    {params: "ctx context.Context", results: "([]repo.User, error)"},
		"Delete":  {params: "ctx context.Context, id string", results: "error"},
	}
	got := map[string]InterfaceMethod{}
	for _, m := range res.Methods {
		got[m.Name] = m
	}
	for name, want := range wantSig {
		m, ok := got[name]
		if !ok {
			t.Errorf("missing method %q", name)
			continue
		}
		if m.Params != want.params {
			t.Errorf("%s params = %q, want %q", name, m.Params, want.params)
		}
		if m.Results != want.results {
			t.Errorf("%s results = %q, want %q", name, m.Results, want.results)
		}
	}

	// NeededImports must include both the interface's own package
	// (for the stub-type declaration to compile) and the context
	// package (referenced by every method signature).
	if _, ok := res.NeededImports["example.com/proj/internal/repo"]; !ok {
		t.Error("NeededImports missing the interface's own package")
	}
	if _, ok := res.NeededImports["context"]; !ok {
		t.Error("NeededImports missing the context package")
	}
}

// TestResolveCrossPkgInterface_EmbeddedInterface verifies that the
// types-package method-set walk picks up methods on embedded interfaces
// without recursing manually — Go's types package flattens for us.
func TestResolveCrossPkgInterface_EmbeddedInterface(t *testing.T) {
	handlerDir := writeTestModule(t, map[string]string{
		"internal/repo/repo.go": `package repo

import "context"

// Reader is embedded into Repository.
type Reader interface {
	GetByID(ctx context.Context, id string) (*User, error)
}

// Writer is also embedded.
type Writer interface {
	Save(ctx context.Context, u *User) error
}

// Repository composes Reader + Writer plus its own Delete method.
type Repository interface {
	Reader
	Writer
	Delete(ctx context.Context, id string) error
}

type User struct {
	ID string
}
`,
	})

	res, ok := ResolveCrossPkgInterface(handlerDir, "repo", "Repository")
	if !ok {
		t.Fatal("expected resolver to succeed")
	}
	names := map[string]bool{}
	for _, m := range res.Methods {
		names[m.Name] = true
	}
	for _, want := range []string{"GetByID", "Save", "Delete"} {
		if !names[want] {
			t.Errorf("method set should include %q after embed flattening, got %v", want, names)
		}
	}
}

// TestResolveCrossPkgInterface_NotAnInterface confirms the resolver
// rejects a named type that isn't actually an interface (e.g. a
// struct). The caller falls through to the "field stays nil"
// behavior, which is correct.
func TestResolveCrossPkgInterface_NotAnInterface(t *testing.T) {
	handlerDir := writeTestModule(t, map[string]string{
		"internal/repo/repo.go": `package repo

// User is a struct, NOT an interface. Asking for a stub here is
// meaningless — the auto-stub feature only synthesizes interface
// implementations.
type User struct {
	ID string
}
`,
		"handlers/billing/service.go": `package billing

import "example.com/proj/internal/repo"

type Deps struct {
	Repo repo.User
}
`,
	})

	if _, ok := ResolveCrossPkgInterface(handlerDir, "repo", "User"); ok {
		t.Error("expected ok=false for a non-interface named type")
	}
}

// TestResolveCrossPkgInterface_UnknownAlias confirms the resolver
// returns ok=false when the alias doesn't correspond to any import.
// This is the "graceful skip" path computeAutoStubs relies on.
func TestResolveCrossPkgInterface_UnknownAlias(t *testing.T) {
	handlerDir := writeTestModule(t, nil)
	if _, ok := ResolveCrossPkgInterface(handlerDir, "doesnotexist", "Repository"); ok {
		t.Error("expected ok=false for an alias not present in the handler dir")
	}
}

// TestResolveCrossPkgInterface_UnloadablePackage confirms that when the
// imported package fails to load (here: import path that doesn't exist
// in the module), the resolver returns ok=false rather than crashing.
func TestResolveCrossPkgInterface_UnloadablePackage(t *testing.T) {
	handlerDir := writeTestModule(t, map[string]string{
		"handlers/billing/service.go": `package billing

import broken "example.com/proj/internal/does_not_exist"

type Deps struct {
	Repo broken.Repository
}
`,
		// And don't write a "internal/repo/repo.go" so the typed
		// import-path is invalid.
		"internal/repo/repo.go": "",
	})
	if _, ok := ResolveCrossPkgInterface(handlerDir, "broken", "Repository"); ok {
		t.Error("expected ok=false when the imported package can't load")
	}
}

// TestComputeAutoStubs_CrossPackage verifies that the selector branch
// of computeAutoStubs produces a DepsAutoStub with CrossPackage=true,
// the right InterfaceQualified, and a non-empty ExtraImports set.
func TestComputeAutoStubs_CrossPackage(t *testing.T) {
	handlerDir := writeTestModule(t, nil)

	stubs, unresolved := computeAutoStubs(handlerDir, "billing")
	if len(unresolved) != 0 {
		t.Errorf("expected zero unresolved stubs on the happy path, got %v", unresolved)
	}
	if len(stubs) != 1 {
		t.Fatalf("got %d stubs, want 1", len(stubs))
	}
	s := stubs[0]
	if !s.CrossPackage {
		t.Error("stub should be CrossPackage=true for repo.Repository")
	}
	if s.FieldName != "Repo" {
		t.Errorf("FieldName = %q, want %q", s.FieldName, "Repo")
	}
	if s.InterfaceQualified != "repo.Repository" {
		t.Errorf("InterfaceQualified = %q, want %q", s.InterfaceQualified, "repo.Repository")
	}
	if s.StubType != "stubBillingRepo" {
		t.Errorf("StubType = %q, want %q", s.StubType, "stubBillingRepo")
	}
	if len(s.ExtraImports) == 0 {
		t.Error("CrossPackage stub should carry ExtraImports")
	}
	// The repo and context packages must both be present.
	paths := map[string]bool{}
	for _, ei := range s.ExtraImports {
		paths[ei.Path] = true
	}
	if !paths["example.com/proj/internal/repo"] {
		t.Errorf("ExtraImports missing the interface's package; got paths %v", paths)
	}
	if !paths["context"] {
		t.Errorf("ExtraImports missing context; got paths %v", paths)
	}
}

// TestComputeAutoStubs_LocalInterfacePreserved confirms the existing
// local-interface path is unchanged. A regression here would break
// every project that currently relies on the auto-stub feature.
func TestComputeAutoStubs_LocalInterfacePreserved(t *testing.T) {
	handlerDir := writeTestModule(t, map[string]string{
		"internal/repo/repo.go": "", // not used in this variant
		"handlers/billing/service.go": `package billing

import "log/slog"

// Repository is declared LOCALLY in the billing package, same shape as
// every existing forge project before this lane.
type Repository interface {
	GetByID(id string) error
}

type Deps struct {
	Logger *slog.Logger
	Repo   Repository
}
`,
	})

	stubs, unresolved := computeAutoStubs(handlerDir, "billing")
	if len(unresolved) != 0 {
		t.Errorf("expected zero unresolved stubs for a local interface, got %v", unresolved)
	}
	if len(stubs) != 1 {
		t.Fatalf("got %d stubs, want 1", len(stubs))
	}
	s := stubs[0]
	if s.CrossPackage {
		t.Error("local-interface stub should NOT be flagged CrossPackage")
	}
	if s.InterfaceQualified != "<alias>.Repository" {
		t.Errorf("local-interface InterfaceQualified = %q, want the <alias>. placeholder",
			s.InterfaceQualified)
	}
	if len(s.ExtraImports) != 0 {
		t.Errorf("local-interface stub should not carry ExtraImports, got %v", s.ExtraImports)
	}
}

// TestGenerateBootstrapTesting_CrossPackageStub is the end-to-end
// validation: lay down a tiny module with handlers/billing/service.go
// pointing at an interface in internal/repo, run GenerateBootstrapTesting
// against that project root, and inspect the resulting pkg/app/testing.go.
//
// The assertions span the three template touches:
//   - the cross-package import is added to the import block
//   - the auto-stub default-assignment line exists inside NewTestBilling
//   - the stub struct itself + at least one method are emitted
func TestGenerateBootstrapTesting_CrossPackageStub(t *testing.T) {
	handlerDir := writeTestModule(t, nil)
	projectRoot := filepath.Dir(filepath.Dir(handlerDir)) // <root>/handlers/billing -> <root>

	services := []ServiceDef{
		{Name: "BillingService", ModulePath: "example.com/proj"},
	}

	if err := GenerateBootstrapTesting(services, nil, nil, nil, "example.com/proj", false, projectRoot, nil); err != nil {
		t.Fatalf("GenerateBootstrapTesting: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(projectRoot, "pkg", "app", "testing.go"))
	if err != nil {
		t.Fatalf("read testing.go: %v", err)
	}
	content := string(body)

	assertions := []struct {
		name string
		want string
	}{
		{"cross-package import block has repo path",
			`repo "example.com/proj/internal/repo"`},
		{"NewTestBilling assigns the auto-stub",
			"deps.Repo = stubBillingRepo{}"},
		{"stub struct is declared",
			"type stubBillingRepo struct{}"},
		{"stub method GetByID is emitted with qualified types",
			"GetByID(ctx context.Context, id string) (*repo.User, error)"},
	}
	for _, a := range assertions {
		if !strings.Contains(content, a.want) {
			t.Errorf("%s: testing.go missing %q\n--- rendered ---\n%s\n--- end ---",
				a.name, a.want, content)
		}
	}
}

// TestGenerateBootstrapTesting_UnresolvedSelectorTODO confirms that a
// selector forge can't resolve produces a visible TODO comment in the
// generated NewTest<Svc> body.
func TestGenerateBootstrapTesting_UnresolvedSelectorTODO(t *testing.T) {
	handlerDir := writeTestModule(t, map[string]string{
		"internal/repo/repo.go": "",
		"handlers/billing/service.go": `package billing

import broken "example.com/proj/internal/does_not_exist"

type Deps struct {
	Repo broken.Repository
}
`,
	})
	projectRoot := filepath.Dir(filepath.Dir(handlerDir))

	services := []ServiceDef{
		{Name: "BillingService", ModulePath: "example.com/proj"},
	}
	if err := GenerateBootstrapTesting(services, nil, nil, nil, "example.com/proj", false, projectRoot, nil); err != nil {
		t.Fatalf("GenerateBootstrapTesting: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(projectRoot, "pkg", "app", "testing.go"))
	content := string(body)
	if !strings.Contains(content, "TODO: stub broken.Repository") {
		t.Errorf("testing.go should carry the TODO comment for an unresolved selector\n--- rendered ---\n%s", content)
	}
}

// TestComputeAutoStubs_UnresolvedSelector covers the graceful-skip
// path: a Deps field typed `pkg.X` where `pkg` is imported but the
// package fails to load. The field falls back to the "stays nil"
// behavior; computeAutoStubs records the field on the unresolved
// slice so the template can emit a TODO comment.
func TestComputeAutoStubs_UnresolvedSelector(t *testing.T) {
	handlerDir := writeTestModule(t, map[string]string{
		"internal/repo/repo.go": "", // remove the would-be target package
		"handlers/billing/service.go": `package billing

import broken "example.com/proj/internal/does_not_exist"

type Deps struct {
	Repo broken.Repository
}
`,
	})

	stubs, unresolved := computeAutoStubs(handlerDir, "billing")
	if len(stubs) != 0 {
		t.Errorf("expected zero stubs (selector unloadable), got %d", len(stubs))
	}
	if len(unresolved) != 1 {
		t.Fatalf("expected exactly one unresolved entry, got %v", unresolved)
	}
	u := unresolved[0]
	if u.FieldName != "Repo" {
		t.Errorf("unresolved FieldName = %q, want %q", u.FieldName, "Repo")
	}
	if u.TypeExpr != "broken.Repository" {
		t.Errorf("unresolved TypeExpr = %q, want %q", u.TypeExpr, "broken.Repository")
	}
}

// TestMergeExtraImports verifies the dedupe-by-path behavior and the
// deterministic Path-sorted ordering of the assembled list.
func TestMergeExtraImports(t *testing.T) {
	services := []BootstrapTestServiceData{
		{
			AutoStubs: []DepsAutoStub{
				{
					CrossPackage: true,
					ExtraImports: []ExtraImport{
						{Path: "example.com/proj/internal/repo", Alias: "repo"},
						{Path: "context", Alias: "context"},
					},
				},
			},
		},
		{
			AutoStubs: []DepsAutoStub{
				{
					CrossPackage: true,
					ExtraImports: []ExtraImport{
						// Duplicate of the first service's repo import.
						{Path: "example.com/proj/internal/repo", Alias: "repo"},
						{Path: "example.com/proj/internal/audit", Alias: "audit"},
					},
				},
				{
					CrossPackage: false, // should be ignored by mergeExtraImports
					ExtraImports: []ExtraImport{
						{Path: "should/never/appear", Alias: "noop"},
					},
				},
			},
		},
	}

	got := mergeExtraImports(services)
	wantPaths := []string{
		"context",
		"example.com/proj/internal/audit",
		"example.com/proj/internal/repo",
	}
	if len(got) != len(wantPaths) {
		t.Fatalf("got %d imports, want %d (%v)", len(got), len(wantPaths), got)
	}
	for i, p := range wantPaths {
		if got[i].Path != p {
			t.Errorf("imports[%d].Path = %q, want %q", i, got[i].Path, p)
		}
	}
	// No CrossPackage=false ExtraImports should have leaked through.
	for _, ei := range got {
		if ei.Path == "should/never/appear" {
			t.Error("mergeExtraImports should ignore non-CrossPackage stubs")
		}
	}
}
