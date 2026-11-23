package codegen

import (
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// optionalFixture: a ChangeController provider that is optional, consumed by
// a Tools handler whose Deps.Changes takes it.
func optionalComponentFixture(t *testing.T, fieldOptional bool) *graphModel {
	t.Helper()
	pkg := types.NewPackage("example.com/x/internal/tools", "tools")
	iface := types.NewNamed(types.NewTypeName(token.NoPos, pkg, "ChangeController", nil), types.NewInterfaceType(nil, nil), nil)
	cc := &graphNode{Name: "ChangeController", LogName: "change-controller", Kind: nodeProvider, Type: "tools.ChangeController", File: "internal/app/change_controller.go", Line: 7, Optional: true}
	tools := &graphNode{Name: "Tools", LogName: "tools", Kind: nodeComponent, Comp: &BuildComponent{ImportPath: "internal/handlers/tools"},
		Deps: []DepsField{{Name: "Changes", Type: "tools.ChangeController", Optional: fieldOptional}, {Name: "Name", Type: "string"}}}
	m := &graphModel{
		byName: map[string]*graphNode{"ChangeController": cc, "Tools": tools},
		nodes:  []*graphNode{cc, tools},
		u: &graphUniverse{
			produced: map[string]types.Type{"ChangeController": iface},
			deps:     map[string]map[string]types.Type{"Tools": {"Changes": iface, "Name": types.Typ[types.String]}},
		},
		edges: map[string][]string{"Tools": {"ChangeController"}},
	}
	return m
}

func TestOptionalComponentConsumerMustBeOptionalDep(t *testing.T) {
	fs := optionalComponentFixture(t, false).optionalComponentFindings()
	if len(fs) != 1 {
		t.Fatalf("findings = %+v, want one", fs)
	}
	f := fs[0]
	if f.Rule != GraphRuleOptionalComponent || !f.Error || f.File != "internal/handlers/tools" ||
		!strings.Contains(f.Message, "Deps.Changes") || !strings.Contains(f.Fix, "forge:optional-dep") {
		t.Fatalf("finding = %+v", f)
	}
	if fs := optionalComponentFixture(t, true).optionalComponentFindings(); len(fs) != 0 {
		t.Fatalf("an optional-dep consumer is clean, got %+v", fs)
	}
}

func TestOptionalComponentWithoutMarkerIsUnaffected(t *testing.T) {
	m := optionalComponentFixture(t, false)
	m.byName["ChangeController"].Optional = false
	if fs := m.optionalComponentFindings(); len(fs) != 0 {
		t.Fatalf("a non-optional producer yields no finding, got %+v", fs)
	}
}

func TestOptionalComponentMustBeNilable(t *testing.T) {
	m := optionalComponentFixture(t, true)
	if err := m.checkOptionalNilable(); err != nil {
		t.Fatal(err)
	}
	if !m.byName["ChangeController"].NilGuard {
		t.Fatal("an optional node's lifecycle literal must be nil-checked")
	}
	pkg := types.NewPackage("example.com/x/internal/app", "app")
	m.u.produced["ChangeController"] = types.NewNamed(types.NewTypeName(token.NoPos, pkg, "Value", nil), types.NewStruct(nil, nil), nil)
	if err := m.checkOptionalNilable(); err == nil || !strings.Contains(err.Error(), "cannot be nil") {
		t.Fatalf("a struct-valued optional node must be refused, got %v", err)
	}
}

func TestOptionalWrappedComponentReturnsNilUnwrapped(t *testing.T) {
	n := &graphNode{Name: "OrgMove", Type: "orgmove.Service", Ctor: "orgmove.New", DepsType: "orgmove.Deps", Doc: "orgmove",
		Wrapped: true, MiddlewareCtor: "NewObserved", Optional: true, Fallible: true, Comp: &BuildComponent{Alias: "orgmove"}}
	got := renderGraphMethod(n, nil)
	if !strings.Contains(got, "if err != nil || svc == nil {\n\t\treturn nil, err\n\t}\n\treturn orgmove.NewObserved(svc), nil") {
		t.Fatalf("fallible optional wrapped:\n%s", got)
	}
	n.Fallible = false
	got = renderGraphMethod(n, nil)
	if !strings.Contains(got, "\tsvc := orgmove.New(") || !strings.Contains(got, "if svc == nil {\n\t\treturn nil, nil\n\t}\n\treturn orgmove.NewObserved(svc), nil") {
		t.Fatalf("infallible optional wrapped:\n%s", got)
	}
	n.Optional = false
	if got := renderGraphMethod(n, nil); strings.Contains(got, "svc == nil") {
		t.Fatalf("a non-optional component is rendered as before:\n%s", got)
	}
}

func TestOptionalComponentDirectiveIsRead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.go"), []byte("// Package orgmove moves orgs.\n//\n//forge:optional-component: nil without a control plane\npackage orgmove\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, ok := FindOptionalComponentMarker(dir)
	if !ok || m.Reason != "nil without a control plane" {
		t.Fatalf("marker = %+v, %v", m, ok)
	}
}
