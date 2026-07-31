// File: internal/contractcheck/deps_are_interfaces.go
//
// The forgeconv-deps-are-interfaces rule fails ANY package that
// declares a `Deps` struct field whose type is a concrete struct
// pointer (or a concrete struct value) rather than an interface.
//
// Why this exists, and why it is not gated on a marker
//
// A component earns its unit tests precisely because every dep is
// behind an interface: `New(Deps{...})` takes mocks, the test
// exercises the real subject, and nothing real is dragged in.
// `Deps: *db.PostgresRepository` defeats that — the test has to
// construct a real repository (or fork-and-mock the whole concrete
// type), so it stops being a unit test and starts needing a database.
//
// That property has nothing to do with what ROLE a package plays. It
// follows from `Deps` existing at all. The rule was once gated on a
// per-package role marker, and the gate is why it covered zero
// packages in forge's own tree and zero in control-plane — while
// control-plane carried 28 concrete-typed `Deps` fields, nine of them
// exactly `*db.PostgresRepository`. An opt-in rule is not a rule; it
// now applies wherever a `type Deps struct` exists.
//
// Detection
//
// 1. Scan internal/<pkg>/ for `type Deps struct { ... }`.
// 2. For each field, classify the type:
//      *T            (pointer to anything other than `interface{...}`) → fire
//      pkg.T         (selector type) → resolve it; interface → ok, else fire
//      stdlib.T      (any type from the standard library)              → ok
//      []T, map[K]T  → primitive element/key → ok, else fire
//      func(...) ...  (any signature)                                   → ok
//      interface{}, named interfaces                                    → ok
//      Logger / Config singletons by field name                         → ok
// 3. The `*slog.Logger`-style logger is the one allowed concrete
//    pointer (loggers are pre-configured singletons; mocking is rare
//    and hand-rolled). We allow it by matching the field name
//    `Logger` rather than the type, which is loose but pragmatic.
//
// Resolution is not optional, and that is why this is an ERROR
//
// A `pkg.T` selector is resolved for real — in THIS module by walking
// the source tree, in any other module by asking `go list` where that
// import path lives and parsing it there. Both answer one question: is
// T declared `type T interface`?
//
// That completeness is what earns error severity. The rule used to
// resolve same-module selectors only and assume every third-party
// `pkg.T` was concrete, "absorbing the false positives at warning
// severity". Measured on control-plane, that assumption produced 9 false
// findings out of 28 — `jetstream.JetStream` (7), `client.Client` and
// `audit.Store` — every one of which is declared an interface by the
// package that ships it. A rule that is wrong a third of the time cannot
// be promoted, and a rule nobody can promote is a rule authors scroll
// past; the two facts are the same fact. Resolving across module
// boundaries removes the class, and with it the hardcoded allow-list of
// forge's own interface types that existed to paper over the single most
// common instance (`orm.Context`, which forge's own CRUD generator
// writes into the Deps of every service it wires).
//
// Severity is error. The remaining findings are the real foot-gun —
// `Deps: *db.PostgresRepository`, a concrete type YOU own — and the fix
// is mechanical: declare a narrow interface at the consumer. It gates
// `forge lint`, NOT `forge generate`: preCodegenContractCheck runs the
// contract-NAMES rule alone, because that is the one whose violation
// makes generated code fail to compile. A concrete dep compiles fine; it
// just cannot be unit-tested, which is a lint verdict.

package contractcheck

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/linter/forgeconv"
)

// goListTimeout caps a single `go list` lookup. The command reads the
// module graph and touches no network (GOPROXY=off), so it returns in
// milliseconds; the bound exists so a wedged toolchain fails the lookup
// instead of hanging the linter.
const goListTimeout = 20 * time.Second

// lintDepsAreInterfaces walks rootDir/internal/ for packages declaring
// a `type Deps struct`, and fires on every concrete-typed field that
// isn't an interface. Returns findings in deterministic order (file,
// then line). A missing internal/ tree is not an error.
func lintDepsAreInterfaces(rootDir string) (forgeconv.Result, error) {
	internalDir := filepath.Join(rootDir, "internal")
	if _, err := os.Stat(internalDir); os.IsNotExist(err) {
		return forgeconv.Result{}, nil
	}

	lin := &depsLinter{
		rootDir:    rootDir,
		modulePath: readModulePath(rootDir),
		declCache:  map[string]typeDecls{},
		dirCache:   map[string]string{},
		nameCache:  map[string]string{},
	}

	var pkgDirs []string
	err := filepath.WalkDir(internalDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" || d.Name() == "node_modules" || d.Name() == "vendor" {
			return filepath.SkipDir
		}
		entries, readErr := os.ReadDir(p)
		if readErr != nil {
			return nil
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				pkgDirs = append(pkgDirs, p)
				break
			}
		}
		return nil
	})
	if err != nil {
		return forgeconv.Result{}, fmt.Errorf("walk %s: %w", internalDir, err)
	}
	sort.Strings(pkgDirs)

	var result forgeconv.Result
	for _, dir := range pkgDirs {
		findings, lintErr := lin.lintPkg(dir)
		if lintErr != nil {
			return forgeconv.Result{}, lintErr
		}
		result.Findings = append(result.Findings, findings...)
	}

	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].File != result.Findings[j].File {
			return result.Findings[i].File < result.Findings[j].File
		}
		if result.Findings[i].Line != result.Findings[j].Line {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].Rule < result.Findings[j].Rule
	})
	return result, nil
}

// depsLinter carries the per-run state the rule needs to resolve a
// selector type (`pkg.T`) to a real declaration: the module path from
// go.mod, and caches keyed on package directory so a widely-imported dep
// is read once, not once per consumer.
type depsLinter struct {
	rootDir    string
	modulePath string
	// declCache memoizes directory → the type facts parsed out of it
	// (interfaces, declared names, method counts).
	declCache map[string]typeDecls
	// dirCache memoizes import path → on-disk directory for packages
	// OUTSIDE this module, which cost a `go list` exec to locate. An
	// empty value is a cached "could not resolve", so an unresolvable
	// path is never re-execed.
	dirCache map[string]string
	// nameCache memoizes directory → package clause, for the qualifier
	// fallback in dirForQualifier.
	nameCache map[string]string
}

// lintPkg parses every non-test .go file in a package and checks the
// `type Deps struct` for non-interface fields. `Deps` is looked for
// across the whole package rather than in one named file because the
// scaffolds split it out of contract.go (service.go / adapter.go /
// client.go all declare it).
func (l *depsLinter) lintPkg(pkgDir string) ([]forgeconv.Finding, error) {
	rootDir := l.rootDir
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", pkgDir, err)
	}

	fset := token.NewFileSet()
	var (
		// fileForDeps holds the AST file containing `type Deps struct`
		// (if any) and its on-disk path so findings can point at the
		// right spot. Only the FIRST Deps struct in the package is
		// considered — multiple is unusual and likely a mistake the
		// user wants to learn about separately.
		fileForDeps *ast.File
		depsPath    string
	)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			// Test files often declare local fake structs; not what
			// the rule targets.
			continue
		}
		fp := filepath.Join(pkgDir, e.Name())
		file, parseErr := parser.ParseFile(fset, fp, nil, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			continue
		}
		if fileForDeps == nil {
			if findDepsStruct(file) != nil {
				fileForDeps = file
				depsPath = fp
			}
		}
	}

	// No `type Deps struct` — nothing this rule can say. That is the
	// scope: a package that never opted into the forge composition
	// shape is not being judged on it.
	if fileForDeps == nil {
		return nil, nil
	}

	deps := findDepsStruct(fileForDeps)
	if deps == nil || deps.Fields == nil {
		return nil, nil
	}

	fileImports := importAliases(fileForDeps)

	rel, relErr := filepath.Rel(rootDir, depsPath)
	if relErr != nil {
		rel = depsPath
	}

	var findings []forgeconv.Finding
	for _, field := range deps.Fields.List {
		// Anonymous embedded fields (no Names) — skip; embedding a
		// concrete type is a different smell that this rule doesn't
		// own.
		if len(field.Names) == 0 {
			continue
		}
		// Allow the standard *slog.Logger field: it's always concrete
		// and projects rarely mock it.
		if isLoggerField(field) {
			continue
		}
		if isLikelyInterfaceType(field.Type) {
			continue
		}
		// A FUNCTION-typed field is already a substitution seam, so there
		// is nothing for this rule to demand. See isFuncSeam.
		if isFuncSeam(field.Type) {
			continue
		}
		// A selector type (`pkg.T`) is resolved for real, in this module or
		// any other: find the package on disk and ask whether T is an
		// interface. Without this the rule fires on every cross-package dep
		// — including the ones already declared as interfaces, which is the
		// shape a correct Deps is SUPPOSED to have. Both of the findings it
		// produced on forge's own tree before same-module resolution
		// (`codegen.Parser`, `contract.Service`) were interfaces, and 9 of
		// the 28 on control-plane before off-module resolution were too. A
		// rule whose hits are false positives is a rule authors mute.
		if l.selectorResolvesToInterface(field.Type, fileImports) {
			continue
		}
		// Standard-library types are out of scope. The foot-gun this rule
		// targets is a concrete type YOU own — `*db.PostgresRepository` —
		// because that is the one a narrow interface declared at the
		// consumer can replace. A stdlib boundary has its own substitution
		// seam that is not an interface (`httptest.NewServer().Client()`
		// for *http.Client, fstest.MapFS for a filesystem), and forge's own
		// adapter scaffold ships `HTTPClient *http.Client` with exactly
		// that seam exercised in the test born beside it. A rule that
		// flags the shape forge itself scaffolds is wrong on its first
		// contact with a new project.
		if isStdlibType(field.Type, fileImports) {
			continue
		}
		// Config-shaped collections of primitives are DATA (allow-lists,
		// feature-flag rosters, scalar limits-by-key) rather than
		// behavior this package calls. They have no meaningful interface
		// equivalent — `[]string` doesn't get easier to mock by hiding
		// behind an interface. Skip them at the rule level so the
		// warning stays focused on real foot-guns (concrete struct
		// pointers, concrete adapter selectors).
		if isPrimitiveConfigShape(field.Type) {
			continue
		}
		// A concrete type that declares NO METHODS is data, not a
		// collaborator, and there is no interface to extract from it —
		// an interface over zero methods is the empty interface, which
		// asserts nothing and mocks nothing. This is the same judgement
		// isPrimitiveConfigShape makes about `[]string`, applied to the
		// struct form: control-plane's `*config.WorkspaceConfig` is a
		// YAML-deserialized bag of storage defaults and probe shapes with
		// 173-to-0 odds against it being a repository. Config-on-Deps is a
		// real design question, and forge-config-deps owns it — this rule
		// is about behavior you cannot fake.
		if l.concreteTypeHasNoMethods(field.Type, fileImports) {
			continue
		}
		// Report each concrete field separately so users see every
		// site that needs an interface lift.
		for _, n := range field.Names {
			line := fset.Position(n.NamePos).Line
			findings = append(findings, forgeconv.Finding{
				Rule:     string(RuleDepsAreInterfaces),
				Severity: forgeconv.SeverityError,
				File:     rel,
				Line:     line,
				Message: fmt.Sprintf(
					"Deps field %q has concrete type %s; deps should be interfaces so this package is testable with all-mock deps",
					n.Name, exprString(field.Type)),
				Remediation: "type the field with an interface. Look for an existing one first — the dep's own contract.go " +
					"interface (already mocked in its mock_gen.go by `forge generate`), a stdlib one like http.Handler, or, " +
					"if this package only FORWARDS the dep, the interface the real consumer one frame down already declares. " +
					"Only mint a new one when none fits, and then name just the methods this package calls. " +
					"If the field is `// forge:optional-dep`, assign it CONDITIONALLY (`if x != nil { deps.X = x }`): " +
					"a nil concrete pointer stored in an interface field is not a nil interface, so an unconditional " +
					"assignment turns your `if deps.X == nil` guard into a nil-receiver panic. " +
					"Skill: forge skill load service-layer",
			})
		}
	}
	return findings, nil
}

// findDepsStruct returns the *ast.StructType for the first
// `type Deps struct {...}` declaration in the file, or nil.
func findDepsStruct(f *ast.File) *ast.StructType {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.TYPE {
			continue
		}
		for _, spec := range gd.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "Deps" {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			return st
		}
	}
	return nil
}

// isLoggerField returns true for the canonical `Logger *slog.Logger`
// (or any field named exactly `Logger`). We carve loggers out because
// projects ship one shared *slog.Logger; mocking it is rare and
// usually hand-rolled rather than worth an interface. Config is treated
// the same way: bootstrap supplies one *config.Config singleton and
// tests typically construct an inline Config{...} value rather than
// fork an interface.
func isLoggerField(field *ast.Field) bool {
	for _, n := range field.Names {
		switch n.Name {
		case "Logger", "Config":
			return true
		}
	}
	return false
}

// isLikelyInterfaceType is a syntactic predicate: it returns true for
// expression shapes that the rule treats as "interface-like":
//
//   - `interface{...}` literal
//   - `any` (universe ident)
//   - bare ident type names (treated as same-package interface OR
//     domain marker; the rule errs on the side of permissive — a
//     concrete struct named locally would only be a violation if it's
//     a struct declared in this package, which is a flag-day decision
//     each project owns)
//
// Pointer types are explicitly NOT interface-like (the foot-gun
// case): `*adapter.Service` looks like an interface to the eye but is
// a concrete struct pointer.
//
// Selector types (`pkg.T`) are NOT interface-like: from the linter's
// vantage we can't always tell if `pkg.T` is `interface` or `struct`
// without resolving across packages. We default to "needs an
// interface lift" and rely on the warning's low severity to absorb
// the false-positive risk on legitimately-imported interfaces.
func isLikelyInterfaceType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.InterfaceType:
		return true
	case *ast.Ident:
		// `any` is universe-defined as `interface{}`.
		if t.Name == "any" {
			return true
		}
		// Same-package bare ident: treat as interface for permissive
		// scoring. A package that declares `type Source interface` in
		// contract.go and references `Source` in Deps is accepted; a
		// package that references a same-package struct here is a
		// false-negative, but the failure mode (tests can still mock
		// it) is exactly what the rule cares about. False-negative >
		// false-positive at warning severity.
		return true
	}
	return false
}

// isFuncSeam reports whether expr is a function type — `func() time.Time`,
// `func() string`, `func(context.Context, []byte) error`, any signature.
//
// A function value is ALREADY the substitution this rule asks for. The
// remedy the finding prescribes ("declare a narrow interface naming the
// methods you call") has no methods to name, and the interface it would
// mint — one method, one call site — is a wrapper around a value a test
// replaces in one line with a literal and no mock. That is the same
// judgement isPrimitiveConfigShape makes about `[]string` and
// concreteTypeHasNoMethods makes about a data struct: the field is not
// behaviour you cannot fake.
//
// It is also the shape forge itself wires. The Clock/IDGen seam
// (service-layer skill, "Deterministic time & IDs") is a Deps field typed
// exactly `func() time.Time` or `func() string`, filled BY TYPE in the
// generated compose.go (`Now: time.Now, // framework clock`) and defaulted
// again in the generated test harness. Firing on it made two forge
// mechanisms contradict each other, and the resolution a real run reached
// was to DELETE the seam from two packages to get a green gate — trading a
// testable clock for a passing lint. A rule that flags what forge's own
// codegen writes is wrong on its first contact with a scaffolded project.
//
// The exemption is the whole CLASS, not the two wired signatures: what
// makes a func field safe is that it is a func, not that forge knows how
// to fill it.
func isFuncSeam(expr ast.Expr) bool {
	_, ok := expr.(*ast.FuncType)
	return ok
}

// isStdlibType reports whether expr names a type from the Go standard
// library, through any number of pointer / slice / map / array wrappers.
//
// The standard-library test is the canonical one: an import path whose
// FIRST segment contains no dot is stdlib ("net/http", "log/slog"), while
// everything with a hostname up front ("github.com/...", "example.com/...")
// is not.
func isStdlibType(expr ast.Expr, imports map[string]string) bool {
	sel := underlyingSelector(expr)
	if sel == nil {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	importPath, ok := imports[pkgIdent.Name]
	if !ok {
		return false
	}
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// underlyingSelector peels pointer / slice / array / map-value wrappers off
// expr and returns the selector underneath, or nil when there isn't one.
func underlyingSelector(expr ast.Expr) *ast.SelectorExpr {
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.ArrayType:
			expr = t.Elt
		case *ast.MapType:
			expr = t.Value
		case *ast.SelectorExpr:
			return t
		default:
			return nil
		}
	}
}

// readModulePath returns the module path from rootDir/go.mod, or "" when
// there is no parseable go.mod. An unresolvable module path degrades the
// selector check to the conservative default (fire), which is the same
// answer the rule gave before resolution existed.
func readModulePath(rootDir string) string {
	b, err := os.ReadFile(filepath.Join(rootDir, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// importAliases maps the local package name used in selector expressions
// (explicit alias, else the last path segment) to its import path, for one
// file.
func importAliases(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		if imp.Path == nil {
			continue
		}
		p := strings.Trim(imp.Path.Value, `"`)
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		} else {
			alias = p
			if i := strings.LastIndex(alias, "/"); i >= 0 {
				alias = alias[i+1:]
			}
		}
		if alias == "_" || alias == "." {
			continue
		}
		out[alias] = p
	}
	return out
}

// selectorResolvesToInterface reports whether expr is a bare selector
// `pkg.T` naming a type declared as an interface — in this module or any
// other. It deliberately does not follow pointers: `*pkg.T` is a concrete
// pointer whatever T is (a pointer to an interface is a mistake, not a
// seam), so those keep firing.
//
// Same-module paths are answered from the rootDir walk with no exec. Every
// other import path is located with [depsLinter.externalPackageDir] and
// then read the same way, because "is this an interface" has one answer and
// a module boundary is not part of it. Guessing "concrete" for anything
// off-module is what made this rule wrong on `jetstream.JetStream`,
// `client.Client` and `audit.Store` — all three declared `type … interface`
// by the package that ships them.
func (l *depsLinter) selectorResolvesToInterface(expr ast.Expr, imports map[string]string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	dir := l.dirForQualifier(pkgIdent.Name, imports)
	if dir == "" {
		return false
	}
	return l.packageInterfaces(dir)[sel.Sel.Name]
}

// dirForQualifier maps the qualifier in `pkg.T` to the directory holding
// that package's sources.
//
// The fast path is the import map, which keys on the explicit alias or the
// import path's last segment. That is right almost always and wrong exactly
// when a module's directory name differs from its package clause —
// `github.com/nats-io/nats.go` is package `nats`, so `nats.Conn` finds
// nothing under the key "nats.go". On a miss, resolve each of the file's
// imports and ask the package itself what it is called. Without the
// fallback an unresolved qualifier defaults to "concrete", which at error
// severity is a build-gate failure on code that is already correct.
func (l *depsLinter) dirForQualifier(qualifier string, imports map[string]string) string {
	if importPath, ok := imports[qualifier]; ok {
		if dir := l.packageDir(importPath); dir != "" {
			return dir
		}
	}
	for alias, importPath := range imports {
		if alias == qualifier {
			continue // already tried above
		}
		// An explicit alias IS the qualifier by definition; if it did not
		// match, the package clause underneath is irrelevant. Only paths
		// whose last segment supplied the key can be lying about their name.
		if alias != path.Base(importPath) {
			continue
		}
		dir := l.packageDir(importPath)
		if dir != "" && l.packageName(dir) == qualifier {
			return dir
		}
	}
	return ""
}

// concreteTypeHasNoMethods reports whether expr names a type — through an
// optional pointer — that its own package declares with zero methods.
//
// Such a type is DATA. There is nothing to put in an interface, so the
// rule's remedy ("declare a narrow interface naming the methods you call")
// has no methods to name, and firing would demand a change that cannot be
// made. It resolves the type for real rather than guessing from the field
// name, so a `Config`-shaped name over something with 173 methods still
// fires and a repository never escapes by being called `Settings`.
func (l *depsLinter) concreteTypeHasNoMethods(expr ast.Expr, imports map[string]string) bool {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	dir := l.dirForQualifier(pkgIdent.Name, imports)
	if dir == "" {
		return false // unresolvable: keep the conservative answer
	}
	decls := l.packageTypeDecls(dir)
	if !decls.declared[sel.Sel.Name] {
		return false // not declared there; do not vouch for it
	}
	// An embedded field PROMOTES the embedded type's whole method set, so
	// `type Wrapper struct{ *sql.DB }` declares no methods and carries
	// hundreds. Counting only declarations would wave through exactly the
	// dep this rule exists to catch, so a struct that embeds anything is
	// never treated as data.
	if decls.embeds[sel.Sel.Name] {
		return false
	}
	return decls.methods[sel.Sel.Name] == 0
}

// packageName returns the package clause of dir's first parseable non-test
// .go file, memoized per run. Empty when dir holds no readable Go source.
func (l *depsLinter) packageName(dir string) string {
	if got, ok := l.nameCache[dir]; ok {
		return got
	}
	l.nameCache[dir] = ""
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.PackageClauseOnly)
		if parseErr != nil || f.Name == nil {
			continue
		}
		l.nameCache[dir] = f.Name.Name
		return f.Name.Name
	}
	return ""
}

// packageDir returns the on-disk directory holding importPath's sources, or
// "" when it cannot be located. A path inside this module is derived from
// the module path with no subprocess; anything else goes to
// externalPackageDir.
func (l *depsLinter) packageDir(importPath string) string {
	if l.modulePath != "" {
		if rel, ok := strings.CutPrefix(importPath, l.modulePath+"/"); ok {
			return filepath.Join(l.rootDir, filepath.FromSlash(rel))
		}
		if importPath == l.modulePath {
			return l.rootDir
		}
	}
	return l.externalPackageDir(importPath)
}

// externalPackageDir asks the go command where an off-module import path
// lives — the module cache, a `replace` directory, or a go.work member.
// The result is memoized per run (including the failure), so a widely
// imported dep costs one exec no matter how many Deps structs name it.
//
// GOPROXY=off makes this hermetic: a package that is not already on disk
// resolves to "", which falls back to the conservative "not an interface"
// answer rather than reaching for the network from inside a linter. -e
// keeps go list from failing the whole invocation over one bad path.
func (l *depsLinter) externalPackageDir(importPath string) string {
	if got, ok := l.dirCache[importPath]; ok {
		return got
	}
	l.dirCache[importPath] = "" // cache the failure up front

	ctx, cancel := context.WithTimeout(context.Background(), goListTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "list", "-e", "-f", "{{.Dir}}", "--", importPath)
	cmd.Dir = l.rootDir
	cmd.Env = append(os.Environ(), "GOPROXY=off")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return ""
	}
	l.dirCache[importPath] = dir
	return dir
}

// packageInterfaces returns the set of interface type names declared in
// dir's non-test .go files, memoized per run. A directory that does not
// parse yields an empty (cached) set — the rule then falls back to firing,
// which is the safe direction for a warning.
func (l *depsLinter) packageInterfaces(dir string) map[string]bool {
	return l.packageTypeDecls(dir).interfaces
}

// typeDecls is what one package directory tells the rule about its own
// type names: which are interfaces, which it declares at all, and how many
// methods each declared type carries.
type typeDecls struct {
	interfaces map[string]bool
	declared   map[string]bool
	methods    map[string]int
	// embeds marks struct types with at least one anonymous field, whose
	// promoted method set no declaration count can see.
	embeds map[string]bool
}

// packageTypeDecls parses dir's non-test .go files once and memoizes the
// answer, so a widely imported dep is read once per run rather than once
// per consumer. A directory that does not parse yields an empty (cached)
// result — every caller then falls back to firing, which is the safe
// direction.
func (l *depsLinter) packageTypeDecls(dir string) typeDecls {
	if got, ok := l.declCache[dir]; ok {
		return got
	}
	out := typeDecls{
		interfaces: map[string]bool{},
		declared:   map[string]bool{},
		methods:    map[string]int{},
		embeds:     map[string]bool{},
	}
	l.declCache[dir] = out

	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, parseErr := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if parseErr != nil {
			continue
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					out.declared[ts.Name.Name] = true
					if _, isIface := ts.Type.(*ast.InterfaceType); isIface {
						out.interfaces[ts.Name.Name] = true
					}
					if st, isStruct := ts.Type.(*ast.StructType); isStruct && hasEmbeddedField(st) {
						out.embeds[ts.Name.Name] = true
					}
				}
			case *ast.FuncDecl:
				if name := receiverTypeName(d); name != "" {
					out.methods[name]++
				}
			}
		}
	}
	return out
}

// hasEmbeddedField reports whether st declares at least one anonymous
// (embedded) field.
func hasEmbeddedField(st *ast.StructType) bool {
	if st.Fields == nil {
		return false
	}
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			return true
		}
	}
	return false
}

// receiverTypeName returns the bare type name a method is declared on
// (peeling the pointer and any generic type parameters), or "" when fn is
// a plain function rather than a method.
func receiverTypeName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	expr := fn.Recv.List[0].Type
	for {
		switch t := expr.(type) {
		case *ast.StarExpr:
			expr = t.X
		case *ast.IndexExpr: // Recv[T]
			expr = t.X
		case *ast.IndexListExpr: // Recv[T, U]
			expr = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// isPrimitiveConfigShape returns true for slice/map types whose element
// (and key, for maps) is a Go built-in primitive — the canonical shape
// for config DATA on a Deps struct. Examples that pass:
//
//	[]string, []int, []float64, []bool, [][]byte
//	map[string]string, map[string]int, ...
//
// Examples that don't (the rule still fires):
//
//	[]Source                 // slice of an interface type
//	[]*adapter.Client        // slice of concrete pointer
//	map[string]*config.Tier  // map to concrete pointer
//
// Rationale: these primitives have no meaningful interface equivalent;
// hiding `[]string` behind an interface adds friction without unlocking
// any test power. Recognizing the shape keeps the rule from training
// users to ignore noisy warnings on routine config fields.
func isPrimitiveConfigShape(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.ArrayType:
		// []byte is `*ast.ArrayType{Elt: Ident{"byte"}}`; [][]byte nests.
		return isPrimitiveType(t.Elt) || isByteSlice(t.Elt)
	case *ast.MapType:
		return isPrimitiveType(t.Key) && (isPrimitiveType(t.Value) || isByteSlice(t.Value))
	}
	return false
}

// isPrimitiveType returns true for Go built-in scalar types that are
// universally safe to ship by value on a Deps struct.
func isPrimitiveType(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	if !ok {
		return false
	}
	switch id.Name {
	case "string", "bool",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr", "byte", "rune",
		"float32", "float64",
		"complex64", "complex128":
		return true
	}
	return false
}

// isByteSlice returns true for the `[]byte` shape — common as an
// inline secret/key/seed, treated as primitive for our purposes.
func isByteSlice(expr ast.Expr) bool {
	at, ok := expr.(*ast.ArrayType)
	if !ok {
		return false
	}
	id, ok := at.Elt.(*ast.Ident)
	if !ok {
		return false
	}
	return id.Name == "byte"
}

// exprString returns a short human-readable rendering of an
// expression for inclusion in the lint message. Avoids dragging in
// go/printer for what is fundamentally a one-line tag.
func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	case *ast.InterfaceType:
		return "interface{...}"
	case *ast.FuncType:
		return "func(...)"
	case *ast.ChanType:
		return "chan " + exprString(t.Value)
	default:
		return fmt.Sprintf("%T", expr)
	}
}
