package contract

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/linter/finding"
)

func TestIsLoopbackHost(t *testing.T) {
	tests := map[string]bool{
		"localhost":      true,
		"LOCALHOST":      true,
		"127.0.0.1":      true,
		"127.1.2.3":      true,
		"::1":            true,
		"[::1]":          true,
		"0.0.0.0":        false,
		"10.0.0.5":       false,
		"example.com":    false,
		"localhost.evil": false,
		"notlocalhost":   false,
		"":               false,
		"2001:db8::1":    false,
		"192.168.0.1":    false,
	}
	for host, want := range tests {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	tests := map[string]bool{
		"127.0.0.1:8080":               true,
		"[::1]:80":                     true,
		"localhost:3000":               true,
		"http://localhost:8080/x":      true,
		"https://127.0.0.1/health":     true,
		"http://[::1]:9/":              true,
		"grpc://10.0.0.1:50051":        false,
		"http://example.com/":          false,
		"example.com:443":              false,
		"localhost":                    false, // no port: not a dialable host:port
		"":                             false,
		"http://%zz":                   false, // unparseable URL
		"http://localhost.example.com": false,
	}
	for addr, want := range tests {
		if got := isLoopbackAddress(addr); got != want {
			t.Errorf("isLoopbackAddress(%q) = %v, want %v", addr, got, want)
		}
	}
}

// constExpr is a string-constant expression the type checker "folded".
func constExpr(info *types.Info, s string) ast.Expr {
	e := &ast.BasicLit{Kind: token.STRING, Value: `"` + s + `"`}
	info.Types[e] = types.TypeAndValue{Type: types.Typ[types.UntypedString], Value: constant.MakeString(s)}
	return e
}

func TestConstantString(t *testing.T) {
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	if got, ok := constantString(info, constExpr(info, "abc")); !ok || got != "abc" {
		t.Errorf("constantString = (%q, %v)", got, ok)
	}
	unfolded := &ast.Ident{Name: "x"}
	if _, ok := constantString(info, unfolded); ok {
		t.Error("an expression the checker did not record is not a constant")
	}
	info.Types[unfolded] = types.TypeAndValue{Type: types.Typ[types.String]}
	if _, ok := constantString(info, unfolded); ok {
		t.Error("a non-constant expression is not a constant")
	}
	number := &ast.BasicLit{Kind: token.INT, Value: "1"}
	info.Types[number] = types.TypeAndValue{Type: types.Typ[types.UntypedInt], Value: constant.MakeInt64(1)}
	if _, ok := constantString(info, number); ok {
		t.Error("a numeric constant is not a string constant")
	}
}

func TestStaticLoopbackTarget(t *testing.T) {
	newInfo := func() *types.Info {
		return &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Selections: map[*ast.SelectorExpr]*types.Selection{}}
	}
	call := func(info *types.Info, pkgPath, fnName string, args ...ast.Expr) ast.Expr {
		pkg := types.NewPackage(pkgPath, pkgPath[strings.LastIndex(pkgPath, "/")+1:])
		fn := types.NewFunc(token.NoPos, pkg, fnName, types.NewSignatureType(nil, nil, nil, nil, nil, false))
		sel := &ast.SelectorExpr{X: &ast.Ident{Name: pkg.Name()}, Sel: &ast.Ident{Name: fnName}}
		info.Uses[sel.Sel] = fn
		return &ast.CallExpr{Fun: sel, Args: args}
	}

	t.Run("a constant loopback address", func(t *testing.T) {
		info := newInfo()
		if !staticLoopbackTarget(info, constExpr(info, "127.0.0.1:8080")) {
			t.Error("a constant loopback address is loopback")
		}
		if staticLoopbackTarget(info, constExpr(info, "example.com:443")) {
			t.Error("a constant remote address is not loopback")
		}
	})

	t.Run("JoinHostPort over a constant loopback host, any port", func(t *testing.T) {
		info := newInfo()
		port := &ast.Ident{Name: "port"} // not foldable: the port may be anything
		info.Types[port] = types.TypeAndValue{Type: types.Typ[types.String]}
		if !staticLoopbackTarget(info, call(info, "net", "JoinHostPort", constExpr(info, "localhost"), port)) {
			t.Error("net.JoinHostPort(\"localhost\", port) is loopback")
		}
		if staticLoopbackTarget(info, call(info, "net", "JoinHostPort", constExpr(info, "db.internal"), constExpr(info, "5432"))) {
			t.Error("net.JoinHostPort over a remote host is not loopback")
		}
	})

	t.Run("what the checker cannot fold is not loopback", func(t *testing.T) {
		info := newInfo()
		host := &ast.Ident{Name: "host"}
		info.Types[host] = types.TypeAndValue{Type: types.Typ[types.String]}
		if staticLoopbackTarget(info, call(info, "net", "JoinHostPort", host, constExpr(info, "80"))) {
			t.Error("a dynamic host must stay outbound I/O")
		}
		if staticLoopbackTarget(info, host) {
			t.Error("a dynamic address must stay outbound I/O")
		}
	})

	t.Run("only net.JoinHostPort with two arguments counts", func(t *testing.T) {
		info := newInfo()
		if staticLoopbackTarget(info, call(info, "example.com/netx", "JoinHostPort", constExpr(info, "localhost"), constExpr(info, "1"))) {
			t.Error("a lookalike JoinHostPort outside package net must not be trusted")
		}
		if staticLoopbackTarget(info, call(info, "net", "Join", constExpr(info, "localhost"), constExpr(info, "1"))) {
			t.Error("another net function is not JoinHostPort")
		}
		if staticLoopbackTarget(info, call(info, "net", "JoinHostPort", constExpr(info, "localhost"))) {
			t.Error("a one-argument call is not JoinHostPort")
		}
		if staticLoopbackTarget(info, &ast.Ident{Name: "unrelated"}) {
			t.Error("a bare identifier is not a call")
		}
	})
}

func TestSetExcludesAndFlag(t *testing.T) {
	t.Cleanup(func() { SetExcludes(nil) })

	SetExcludes([]string{"example.com/m/internal/legacy", "gen"})
	if got := GetExcludes(); !reflect.DeepEqual(got, []string{"example.com/m/internal/legacy", "gen"}) {
		t.Errorf("GetExcludes = %v", got)
	}
	if !IsExcluded("example.com/m/internal/legacy") || !IsExcluded("example.com/m/gen/pb") {
		t.Error("configured patterns must exclude matching packages")
	}
	if IsExcluded("example.com/m/internal/other") {
		t.Error("an unrelated package must not be excluded")
	}

	// GetExcludes hands out a copy; mutating it must not edit the live list.
	got := GetExcludes()
	got[0] = "tampered"
	if GetExcludes()[0] != "example.com/m/internal/legacy" {
		t.Error("GetExcludes leaked the internal slice")
	}

	// SetExcludes copies its input as well.
	in := []string{"a/b"}
	SetExcludes(in)
	in[0] = "changed"
	if GetExcludes()[0] != "a/b" {
		t.Error("SetExcludes kept a reference to the caller's slice")
	}

	var f excludeFlag
	if err := f.Set(" x/y , ,z "); err != nil {
		t.Fatal(err)
	}
	if got := GetExcludes(); !reflect.DeepEqual(got, []string{"x/y", "z"}) {
		t.Errorf("after Set = %v, want the trimmed, non-empty entries", got)
	}
	if f.String() != "x/y,z" {
		t.Errorf("String = %q", f.String())
	}
	if err := f.Set(""); err != nil {
		t.Fatal(err)
	}
	if got := GetExcludes(); len(got) != 0 || IsExcluded("x/y") {
		t.Errorf("an empty value must clear the list, got %v", got)
	}
}

func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func loadModule(t *testing.T, dir string) []*packages.Package {
	t.Helper()
	pkgs, err := packages.Load(&packages.Config{
		Dir: dir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedModule,
	}, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if n := packages.PrintErrors(pkgs); n != 0 {
		t.Fatalf("fixture has %d load errors", n)
	}
	return pkgs
}

func findingsByDir(fs []ExcludeFinding) map[string][]ExcludeFinding {
	out := map[string][]ExcludeFinding{}
	for _, f := range fs {
		d := filepath.Base(filepath.Dir(f.File))
		out[d] = append(out[d], f)
	}
	return out
}

func TestCheckNotAComponentDirectives(t *testing.T) {
	if testing.Short() {
		t.Skip("loads and type-checks a fixture module via go/packages; runs in task test")
	}
	const contract = "package %s\n\ntype Service interface{ Do() }\n"
	dir := writeModule(t, map[string]string{
		"go.mod": "module example.com/nac\n\ngo 1.24\n",
		// Well placed: a contract package under internal/ with a reason.
		"internal/cliauth/contract.go": "// Package cliauth is built by the login command.\n//\n//forge:not-a-component: CLI-only: built by the login command\npackage cliauth\n\ntype Service interface{ Do() }\n",
		// A bare marker is refused for its missing reason (and is otherwise well placed).
		"internal/bare/contract.go": "//forge:not-a-component\npackage bare\n\ntype Service interface{ Do() }\n",
		// A handler is a server component by definition.
		"internal/handlers/users/doc.go":  "//forge:not-a-component: not mounted\npackage users\n",
		"internal/workers/sweeper/doc.go": "//forge:not-a-component: not supervised\npackage sweeper\n",
		// Nested BELOW a role root, composition reads the marker: accepted.
		"internal/handlers/users/inner/contract.go": "//forge:not-a-component: constructed by the handler\npackage inner\n\ntype Service interface{ Do() }\n",
		// Contradicts the external-component marker.
		"internal/both/contract.go": "//forge:not-a-component: not built by the server\n//forge:external-component\npackage both\n\ntype Service interface{ Do() }\n",
		// Outside internal/.
		"pkg/lib/lib.go": "//forge:not-a-component: library\npackage lib\n",
		// Inside internal/ but no contract.
		"internal/nocontract/x.go": "//forge:not-a-component: helper\npackage nocontract\n\nfunc F() {}\n",
		// Already out of codegen via exclude-contract.
		"internal/excluded/contract.go": "//forge:exclude-contract: pure helper\n//forge:not-a-component: also not a component\npackage excluded\n\ntype Service interface{ Do() }\n",
		// Disagreeing repeats.
		"internal/twice/a.go":        "//forge:not-a-component: first reason\npackage twice\n\ntype Service interface{ Do() }\n",
		"internal/twice/b.go":        "//forge:not-a-component: a different reason\npackage twice\n",
		"internal/twice/contract.go": "package twice\n",
		// No marker at all: never mentioned.
		"internal/plain/contract.go": "package plain\n\ntype Service interface{ Do() }\n",
	})
	_ = contract
	pkgs := loadModule(t, dir)

	by := findingsByDir(CheckNotAComponentDirectives(pkgs, ExcludeGateOptions{}))

	rules := func(d string) []string {
		var out []string
		for _, f := range by[d] {
			out = append(out, f.Rule)
		}
		return out
	}
	for d, want := range map[string][]string{
		"cliauth":    nil,
		"plain":      nil,
		"inner":      nil,
		"bare":       {RuleNotAComponentReason},
		"users":      {RuleNotAComponentScope},
		"sweeper":    {RuleNotAComponentScope},
		"both":       {RuleNotAComponentScope},
		"lib":        {RuleNotAComponentScope},
		"nocontract": {RuleNotAComponentScope},
		"excluded":   {RuleNotAComponentScope},
		"twice":      {RuleNotAComponentConflict},
	} {
		if got := rules(d); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: rules = %v, want %v (%+v)", d, got, want, by[d])
		}
	}

	messages := map[string]string{
		"users":      "a service handler (it is mounted)",
		"sweeper":    "a worker (it is supervised)",
		"both":       "contradicts its `//forge:external-component` marker",
		"lib":        "forge composes only contract packages under internal/",
		"nocontract": "has no contract.go or //forge:contract type",
		"excluded":   "already takes the package out of contract codegen",
		"bare":       "the marker requires a reason",
		"twice":      "declares `//forge:not-a-component` twice, and the two disagree",
	}
	for d, want := range messages {
		if len(by[d]) == 0 || !strings.Contains(by[d][0].Message, want) {
			t.Errorf("%s: message %q lacks %q", d, firstMessage(by[d]), want)
		}
		if len(by[d]) > 0 && (by[d][0].Severity != finding.SeverityError || by[d][0].FixHint == "" || by[d][0].Line == 0) {
			t.Errorf("%s: finding %+v must be an error with a fix hint and a line", d, by[d][0])
		}
	}

	t.Run("no codegen means no placement findings", func(t *testing.T) {
		by := findingsByDir(CheckNotAComponentDirectives(pkgs, ExcludeGateOptions{CodegenUnavailable: true}))
		if got := len(by["users"]) + len(by["lib"]) + len(by["nocontract"]); got != 0 {
			t.Errorf("placement findings %v with CodegenUnavailable; there is no composition to misplace a marker in", by)
		}
		if len(by["bare"]) != 1 || by["bare"][0].Rule != RuleNotAComponentReason {
			t.Errorf("a missing reason stays an error everywhere: %+v", by["bare"])
		}
	})

	t.Run("a test variant of a package is not reported twice", func(t *testing.T) {
		with := append([]*packages.Package(nil), pkgs...)
		for _, p := range pkgs {
			if strings.HasSuffix(p.PkgPath, "/internal/bare") {
				variant := *p
				variant.ID = p.PkgPath + " [" + p.PkgPath + ".test]"
				with = append(with, &variant)
			}
		}
		if got := findingsByDir(CheckNotAComponentDirectives(with, ExcludeGateOptions{})); len(got["bare"]) != 1 {
			t.Errorf("findings for bare = %d, want 1", len(got["bare"]))
		}
	})

	t.Run("an in-source allowance suppresses the refusal", func(t *testing.T) {
		d := writeModule(t, map[string]string{
			"go.mod":                    "module example.com/nac2\n\ngo 1.24\n",
			"internal/bare/contract.go": "// forge:lint-disable-next-line forge-not-a-component-reason: legacy, tracked in #123\n//forge:not-a-component\npackage bare\n\ntype Service interface{ Do() }\n",
		})
		if got := CheckNotAComponentDirectives(loadModule(t, d), ExcludeGateOptions{}); len(got) != 0 {
			t.Errorf("an allowance with a reason must silence the finding, got %+v", got)
		}
	})
}

func firstMessage(fs []ExcludeFinding) string {
	if len(fs) == 0 {
		return "<no findings>"
	}
	return fs[0].Message
}

func TestModuleRelDir(t *testing.T) {
	mod := t.TempDir()
	p := &packages.Package{PkgPath: "example.com/m/internal/x", Module: &packages.Module{Dir: mod}}
	if got := moduleRelDir(p, filepath.Join(mod, "internal", "x")); got != "internal/x" {
		t.Errorf("moduleRelDir = %q, want internal/x", got)
	}
	// A directory outside the module falls back to the import path's internal/ suffix.
	if got := moduleRelDir(p, filepath.Join(t.TempDir(), "elsewhere")); got != "internal/x" {
		t.Errorf("outside the module = %q, want the import-path fallback", got)
	}
	noMod := &packages.Package{PkgPath: "example.com/m/internal/y/z"}
	if got := moduleRelDir(noMod, "/anywhere"); got != "internal/y/z" {
		t.Errorf("no module info = %q", got)
	}
	plain := &packages.Package{PkgPath: "example.com/m/pkg/lib"}
	if got := moduleRelDir(plain, "/anywhere"); got != "example.com/m/pkg/lib" {
		t.Errorf("no internal/ in the path = %q, want the whole import path", got)
	}
}

func TestPackageDeclaration(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rules := []string{RuleExcludeReasonRequired}
	a := write("a.go", "//forge:exclude-contract: same reason\npackage p\n")
	b := write("b.go", "//forge:exclude-contract: same reason\npackage p\n")
	c := write("c.go", "// forge:lint-disable-next-line "+RuleExcludeReasonRequired+": legacy until #9\n//forge:exclude-contract: same reason\npackage p\n")
	d := write("d.go", "//forge:exclude-contract: different\npackage p\n")

	marker := func(path string, line int, reason string) codegen.ReasonedMarker {
		return codegen.ReasonedMarker{File: path, Line: line, Reason: reason}
	}

	t.Run("identical repeats agree and the first wins", func(t *testing.T) {
		got, conflict := packageDeclaration("example.com/p", "forge:exclude-contract",
			[]codegen.ReasonedMarker{marker(a, 1, "same reason"), marker(b, 1, "same reason")}, rules, RuleExcludeConflict)
		if conflict != nil || got.File != a {
			t.Errorf("got (%+v, %+v), want the first marker and no conflict", got, conflict)
		}
	})

	t.Run("a different reason is a conflict naming both files", func(t *testing.T) {
		_, conflict := packageDeclaration("example.com/p", "forge:exclude-contract",
			[]codegen.ReasonedMarker{marker(a, 1, "same reason"), marker(d, 1, "different")}, rules, RuleExcludeConflict)
		if conflict == nil || conflict.Rule != RuleExcludeConflict || conflict.File != d || conflict.Line != 1 {
			t.Fatalf("conflict = %+v", conflict)
		}
		for _, want := range []string{"a.go:1", "d.go:1", `"same reason"`, `"different"`, "no allowance"} {
			if !strings.Contains(conflict.Message, want) {
				t.Errorf("message %q lacks %q", conflict.Message, want)
			}
		}
	})

	t.Run("an allowance on one marker only is a disagreement", func(t *testing.T) {
		_, conflict := packageDeclaration("example.com/p", "forge:exclude-contract",
			[]codegen.ReasonedMarker{marker(a, 1, "same reason"), marker(c, 2, "same reason")}, rules, RuleExcludeConflict)
		if conflict == nil {
			t.Fatal("markers that differ in their allowances must conflict: the refusal would come and go with file order")
		}
		if !strings.Contains(conflict.Message, "an allowance for "+RuleExcludeReasonRequired) {
			t.Errorf("message %q must say which allowance differs", conflict.Message)
		}
	})

	t.Run("a bare marker is described as having no reason", func(t *testing.T) {
		_, conflict := packageDeclaration("example.com/p", "forge:exclude-contract",
			[]codegen.ReasonedMarker{marker(a, 1, ""), marker(d, 1, "different")}, rules, RuleExcludeConflict)
		if conflict == nil || !strings.Contains(conflict.Message, "no reason") {
			t.Errorf("conflict = %+v", conflict)
		}
	})

	t.Run("an unreadable file reads as no allowance rather than failing", func(t *testing.T) {
		missing := filepath.Join(dir, "missing.go")
		got, conflict := packageDeclaration("example.com/p", "forge:exclude-contract",
			[]codegen.ReasonedMarker{marker(missing, 1, "r"), marker(missing, 5, "r")}, rules, RuleExcludeConflict)
		if conflict != nil || got.File != missing {
			t.Errorf("got (%+v, %+v)", got, conflict)
		}
	})
}

func TestMarkerPos(t *testing.T) {
	got := markerPos(codegen.ReasonedMarker{File: filepath.Join("some", "dir", "doc.go"), Line: 12})
	if got != "doc.go:12" {
		t.Errorf("markerPos = %q, want the base name and line", got)
	}
}
