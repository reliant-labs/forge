package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAssignablePairs_NarrowInterfaceWideConcrete proves the matcher
// surfaces the fat-repo greppability pair: AppExtras holds a concrete
// pointer (*Repo) that implements a narrow Deps interface (Logger). The
// returned assertion must name the interface and the pointer concrete.
func TestAssignablePairs_NarrowInterfaceWideConcrete(t *testing.T) {
	dir := writeMatcherProject(t,
		`package app

type Repo struct{}

func (*Repo) Log() {}

type AppExtras struct {
	RepoField *Repo
}

type App struct {
	*AppExtras
}
`,
		`package audit

type Logger interface{ Log() }

type Deps struct {
	RepoField Logger
}
`,
		"internal", "audit",
	)

	m := NewDepsAssignabilityMatcher(dir)
	pairs := m.AssignablePairs("internal", "audit")
	if len(pairs) != 1 {
		t.Fatalf("AssignablePairs = %d pairs, want 1: %+v", len(pairs), pairs)
	}
	p := pairs[0]
	if !strings.HasSuffix(p.Interface, "Logger") {
		t.Errorf("interface = %q, want suffix Logger", p.Interface)
	}
	if !strings.Contains(p.Concrete, "Repo") || !strings.HasPrefix(p.Concrete, "*") {
		t.Errorf("concrete = %q, want pointer to Repo", p.Concrete)
	}
}

// TestAssignablePairs_SkipsValueConcrete ensures a value-typed concrete
// (not a pointer/interface) produces no assertion — (Concrete)(nil) would
// not compile for a value type, so the emitter must skip it.
func TestAssignablePairs_SkipsValueConcrete(t *testing.T) {
	dir := writeMatcherProject(t,
		`package app

type Repo struct{}

func (Repo) Log() {}

type AppExtras struct {
	RepoField Repo
}

type App struct {
	*AppExtras
}
`,
		`package audit

type Logger interface{ Log() }

type Deps struct {
	RepoField Logger
}
`,
		"internal", "audit",
	)

	m := NewDepsAssignabilityMatcher(dir)
	pairs := m.AssignablePairs("internal", "audit")
	if len(pairs) != 0 {
		t.Fatalf("AssignablePairs = %d pairs, want 0 (value concrete skipped): %+v", len(pairs), pairs)
	}
}

// TestAssignablePairs_SkipsNonInterfaceDeps ensures a Deps field that is
// NOT an interface (a concrete pointer wired directly) yields no
// assertion — there's no interface to make greppable.
func TestAssignablePairs_SkipsNonInterfaceDeps(t *testing.T) {
	dir := writeMatcherProject(t,
		`package app

type Repo struct{}

type AppExtras struct {
	RepoField *Repo
}

type App struct {
	*AppExtras
}
`,
		`package audit

import app "example.com/proj/pkg/app"

type Deps struct {
	RepoField *app.Repo
}
`,
		"internal", "audit",
	)

	m := NewDepsAssignabilityMatcher(dir)
	pairs := m.AssignablePairs("internal", "audit")
	if len(pairs) != 0 {
		t.Fatalf("AssignablePairs = %d pairs, want 0 (Deps field is concrete): %+v", len(pairs), pairs)
	}
}

// TestGenerateInterfaceAssertions_EmitsCompilableFile drives the full
// emitter against a project where pkg/app holds the concrete and a
// component declares the narrow interface. The generated file must parse
// as valid Go, declare package app, contain the assertion, and import the
// component package the interface lives in.
func TestGenerateInterfaceAssertions_EmitsCompilableFile(t *testing.T) {
	dir := writeMatcherProject(t,
		`package app

type Repo struct{}

func (*Repo) Log() {}

type AppExtras struct {
	RepoField *Repo
}

type App struct {
	*AppExtras
}
`,
		`package audit

type Logger interface{ Log() }

type Deps struct {
	RepoField Logger
}
`,
		"internal", "audit",
	)

	m := NewDepsAssignabilityMatcher(dir)
	comps := []InterfaceAssertionComponent{{RoleRoot: "internal", PkgDir: "audit"}}
	if err := GenerateInterfaceAssertions(comps, "example.com/proj", dir, m, nil); err != nil {
		t.Fatalf("GenerateInterfaceAssertions: %v", err)
	}

	path := filepath.Join(dir, "pkg", "app", "interface_assertions_gen.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated file: %v", err)
	}
	body := string(src)

	// Must be valid Go.
	if _, perr := parser.ParseFile(token.NewFileSet(), path, src, parser.AllErrors); perr != nil {
		t.Fatalf("generated file does not parse: %v\n---\n%s", perr, body)
	}
	if !strings.Contains(body, "package app") {
		t.Errorf("generated file must declare package app:\n%s", body)
	}
	// The concrete *Repo lives in pkg/app itself, so it renders
	// unqualified (a package cannot import itself).
	if !strings.Contains(body, "var _ audit.Logger = (*Repo)(nil)") {
		t.Errorf("generated file must assert audit.Logger = (*Repo)(nil):\n%s", body)
	}
	if !strings.Contains(body, "example.com/proj/internal/audit") {
		t.Errorf("generated file must import the audit package:\n%s", body)
	}
}

// TestGenerateInterfaceAssertions_NilMatcherNoOp ensures a nil matcher
// (assertions disabled) writes nothing and returns no error.
func TestGenerateInterfaceAssertions_NilMatcherNoOp(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateInterfaceAssertions(nil, "example.com/proj", dir, nil, nil); err != nil {
		t.Fatalf("nil matcher should be a no-op, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pkg", "app", "interface_assertions_gen.go")); !os.IsNotExist(err) {
		t.Errorf("nil matcher must not write a file")
	}
}
