// File: internal/codegen/deps_assignability.go
//
// Type-aware Deps→AppExtras field matcher used by:
//
//   - inspectComponentDepsShape (Matcher A, generator.go) — decides
//     whether to emit `<DepsField>: app.<DepsField>` from the bootstrap
//     template.
//   - wireExpressionForApp / wireExpressionFor (Matcher B, wire_gen.go) —
//     decides whether `wireXxxDeps` should resolve a Deps field to
//     `app.<Field>`.
//
// Both matchers historically compared the pretty-printed type STRINGS
// produced by printType (go/printer). Pure string compare has two
// well-known failure modes:
//
//   1. False-negative — silent drop. AppExtras.Repo is *db.Repo (wide
//      concrete) and Deps.Repo is myservice.Repository (narrow
//      interface) that *db.Repo implements. The strings differ, the wire
//      is silently skipped, the package constructs with a nil Repo, the
//      feature no-ops in production.
//
//   2. False-positive — compile error. AppExtras.Foo is *foo.A and
//      Deps.Foo is *bar.A. Matcher B (wire_gen) sees a name match and
//      emits `app.Foo` regardless of type; the rendered code fails to
//      compile.
//
// The DepsAssignabilityMatcher uses go/types via x/tools/go/packages to
// answer the real question — "is AppExtras.<Field> assignable to
// Deps.<Field>'s declared type?" — and degrades to string-compare when
// the project doesn't type-check (e.g. early scaffold). Failures of the
// load are NOT fatal; they fall through to the legacy string compare so
// codegen never regresses on a project that's mid-edit.
//
// Caching: one matcher per generate run. The same pkg/app and each
// roleRoot/<pkg>/ directory is loaded once via packages.Load, then
// reused for every Deps field. Without the cache we'd re-load the same
// packages O(fields) times per component.

package codegen

import (
	"go/types"
	"path/filepath"
	"sync"

	"golang.org/x/tools/go/packages"
)

// MatchKind classifies the result of one Deps-field → AppExtras-field
// match attempt. The two matchers (bootstrap and wire_gen) treat the
// kinds slightly differently — see the comment on Match below.
type MatchKind int

const (
	// MatchNoName — AppExtras has no field with this Deps field's name.
	// Both matchers treat this as "no wire, no error".
	MatchNoName MatchKind = iota
	// MatchExactString — the pretty-printed type strings are byte-equal.
	// Legacy fast path: no go/types load required. Both matchers wire.
	MatchExactString
	// MatchAssignable — types loaded and AppExtras.<F> is assignable to
	// Deps.<F>'s declared type per go/types (narrow-interface case).
	// Both matchers wire.
	MatchAssignable
	// MatchNameMismatch — name matches but the types are not assignable
	// (and not byte-equal). Bootstrap drops the wire and relies on the
	// post-generate lint to surface the gap; wire_gen drops the
	// app.<Field> resolution and falls through to typed-zero +
	// unresolved hint so the silent compile-error class becomes loud.
	MatchNameMismatch
	// MatchUnavailable — go/types load failed (project not buildable,
	// missing go.mod, package unloadable). The matcher falls back to
	// MatchExactString semantics: byte-equal strings wire, anything else
	// is treated as MatchNoName. This preserves the pre-matcher
	// behavior so codegen never regresses on a project mid-edit.
	MatchUnavailable
)

// DepsAssignabilityMatcher answers "is AppExtras.<FieldName> assignable
// to Deps.<FieldName>?" for a given roleRoot (e.g. "internal",
// "handlers", "workers", "operators") and package directory under it.
//
// One instance per generate run. Methods are safe for concurrent use
// within a single generate (the cache mutex serializes loads).
//
// Construction is cheap: NewDepsAssignabilityMatcher only stores the
// project dir. Packages are loaded lazily on the first Match call that
// requires them. A project that's missing pkg/app or whose source
// doesn't type-check still constructs fine and degrades to the
// string-compare fallback.
type DepsAssignabilityMatcher struct {
	projectDir string

	mu          sync.Mutex
	appLoaded   bool
	appPkg      *packages.Package
	appFields   map[string]types.Type // AppExtras + App promoted fields → declared type
	depsLoaded  map[string]bool       // key: roleRoot|pkgImportPath
	depsPkgs    map[string]*packages.Package
	depsFields  map[string]map[string]types.Type // key: roleRoot|pkgImportPath → field name → declared type
}

// NewDepsAssignabilityMatcher returns a matcher rooted at projectDir.
// projectDir is the directory containing go.mod / pkg/app / handlers /
// workers / operators / internal.
func NewDepsAssignabilityMatcher(projectDir string) *DepsAssignabilityMatcher {
	return &DepsAssignabilityMatcher{
		projectDir: projectDir,
		depsLoaded: map[string]bool{},
		depsPkgs:   map[string]*packages.Package{},
		depsFields: map[string]map[string]types.Type{},
	}
}

// Match resolves whether the named Deps field should be wired from
// AppExtras. roleRoot is "internal" / "handlers" / "workers" /
// "operators"; pkgDir is the directory name under roleRoot (e.g.
// "audit", "billing"); depsFieldName is the Go field name on the
// package's Deps struct; appNameKnown reports whether AppExtras has a
// same-name field (as parsed by ParseAppFields — the cheap AST path);
// depsTypeStr / appTypeStr are the pretty-printed type strings the
// legacy matchers already had.
//
// The matcher uses the cheap inputs to avoid a go/types load when the
// answer is unambiguous (no name match, or byte-equal strings). It
// only loads packages when the strings differ AND a name match exists
// — exactly the case the legacy compare got wrong.
func (m *DepsAssignabilityMatcher) Match(roleRoot, pkgDir, depsFieldName, depsTypeStr, appTypeStr string, appNameKnown bool) MatchKind {
	if !appNameKnown {
		return MatchNoName
	}
	if depsTypeStr == appTypeStr {
		return MatchExactString
	}

	// Strings differ — we need the type checker to decide. Load
	// pkg/app and roleRoot/pkgDir lazily.
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.appLoaded {
		m.loadAppLocked()
	}
	if m.appPkg == nil || m.appFields == nil {
		return MatchUnavailable
	}
	appT, ok := m.appFields[depsFieldName]
	if !ok {
		// AST said the name was there but the type checker disagrees
		// (e.g. the field is on a promoted struct we didn't model).
		// Be safe and decline rather than emit a wire that won't compile.
		return MatchUnavailable
	}

	key := roleRoot + "|" + pkgDir
	if !m.depsLoaded[key] {
		m.loadDepsLocked(roleRoot, pkgDir)
	}
	depsT, ok := m.depsFields[key][depsFieldName]
	if !ok {
		return MatchUnavailable
	}

	if types.AssignableTo(appT, depsT) {
		return MatchAssignable
	}
	// Interface impl is the common other shape: depsT is an interface
	// and appT implements it (covered by AssignableTo above, but be
	// explicit in case of named-vs-underlying surprises).
	if iface, ok := depsT.Underlying().(*types.Interface); ok && types.Implements(appT, iface) {
		return MatchAssignable
	}
	return MatchNameMismatch
}

// loadAppLocked loads pkg/app's types and indexes the AppExtras +
// App-promoted exported fields by name. Called once per matcher
// lifetime; caller holds m.mu.
func (m *DepsAssignabilityMatcher) loadAppLocked() {
	m.appLoaded = true
	appDir := filepath.Join(m.projectDir, "pkg", "app")
	pkg := loadPackageDir(appDir)
	if pkg == nil {
		return
	}
	m.appPkg = pkg
	m.appFields = collectAppFieldTypes(pkg)
}

// loadDepsLocked loads roleRoot/pkgDir and indexes its Deps struct's
// fields by name. Called once per (roleRoot, pkgDir) pair per matcher
// lifetime; caller holds m.mu.
func (m *DepsAssignabilityMatcher) loadDepsLocked(roleRoot, pkgDir string) {
	key := roleRoot + "|" + pkgDir
	m.depsLoaded[key] = true
	dir := filepath.Join(m.projectDir, roleRoot, pkgDir)
	pkg := loadPackageDir(dir)
	if pkg == nil {
		return
	}
	m.depsPkgs[key] = pkg
	m.depsFields[key] = collectDepsFieldTypes(pkg)
}

// loadPackageDir runs packages.Load against dir. Returns nil on any
// load failure or type error — callers degrade to MatchUnavailable.
//
// Note we reuse the exact Mode shape from ResolveCrossPkgInterface
// (NeedName|NeedTypes|NeedTypesInfo|NeedDeps|NeedImports|NeedSyntax) —
// that combination has already been battle-tested against forge's
// workspace setup. cfg.Dir = the directory we're loading, so module
// resolution honors the project's go.mod / go.work.
func loadPackageDir(dir string) *packages.Package {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedDeps | packages.NeedImports | packages.NeedSyntax,
		Dir: dir,
	}
	pkgs, err := packages.Load(cfg, ".")
	if err != nil || len(pkgs) == 0 {
		return nil
	}
	p := pkgs[0]
	// Refuse partial type info — see ResolveCrossPkgInterface's
	// identical comment for the rationale.
	if len(p.Errors) > 0 || p.Types == nil {
		return nil
	}
	return p
}

// collectAppFieldTypes walks pkg/app's loaded types and returns a map
// from field name → declared type for every exported field on the App
// struct AND every exported field on AppExtras (which App embeds as a
// pointer; Go's promotion makes those reachable as app.<Field>).
//
// Mirrors ParseAppFields's AST-level coverage but operates on
// go/types so the resulting Type values can feed AssignableTo /
// Implements.
func collectAppFieldTypes(pkg *packages.Package) map[string]types.Type {
	out := map[string]types.Type{}
	scope := pkg.Types.Scope()
	for _, name := range []string{"App", "AppExtras"} {
		obj := scope.Lookup(name)
		if obj == nil {
			continue
		}
		tn, ok := obj.(*types.TypeName)
		if !ok {
			continue
		}
		st, ok := tn.Type().Underlying().(*types.Struct)
		if !ok {
			continue
		}
		for f := range st.Fields() {
			if !f.Exported() {
				continue
			}
			if f.Anonymous() {
				// Embedded (most importantly *AppExtras on App). We
				// pick those fields up via the AppExtras pass above.
				continue
			}
			// First write wins so App's own fields shadow AppExtras
			// when they collide — matches Go's field-promotion rules.
			if _, exists := out[f.Name()]; exists {
				continue
			}
			out[f.Name()] = f.Type()
		}
	}
	return out
}

// collectDepsFieldTypes walks the loaded package's types scope for a
// type named "Deps" and returns its exported fields by name. Empty
// when no Deps struct is declared (legitimate for component packages
// that don't take deps yet).
func collectDepsFieldTypes(pkg *packages.Package) map[string]types.Type {
	out := map[string]types.Type{}
	obj := pkg.Types.Scope().Lookup("Deps")
	if obj == nil {
		return out
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return out
	}
	st, ok := tn.Type().Underlying().(*types.Struct)
	if !ok {
		return out
	}
	for f := range st.Fields() {
		if !f.Exported() {
			continue
		}
		out[f.Name()] = f.Type()
	}
	return out
}
