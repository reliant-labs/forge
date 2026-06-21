// File: internal/codegen/infra_assignability.go
//
// Infra-field resolution for the GENERATED injector (inject_gen.go). The
// owned *Infra struct (internal/app/providers.go) is the provider set the
// user supplies for everything forge cannot derive — DB pools, NATS, k8s
// clients, adapter wrappings, explicit interface bindings. Build fills a
// Deps field from an Infra field whose type is ASSIGNABLE to the Deps
// field's declared type (by go/types, in one shared universe), so a
// concrete *db.PostgresRepository on Infra fills a narrow audit.Repository
// Deps field — closing constraint-3's silent-drop (FORGE_SHAPE_REDESIGN
// §2).
//
// This is the by-TYPE analog of wire_gen's DepsAssignabilityMatcher (which
// matched Deps -> AppExtras by NAME then checked the type). Here resolution
// is purely by type: any Infra field assignable to the Deps field type is a
// candidate, with an exact-name Infra field preferred as the deterministic
// tie-break and the compile-time backstop.
//
// SINGLE TYPE UNIVERSE: internal/app and the component package are loaded
// in ONE packages.Load call — go/types identity is pointer identity, so a
// split load yields distinct *types.Named for literally-identical source
// types and AssignableTo reports false. Same rationale (and the same Mode
// shape) as deps_assignability.go.
//
// DETERMINISTIC FAIL-LOUD: when the universe can't be proven (project
// mid-edit, load error), an exact-name Infra field is still emitted as
// `infra.<Field>` (MatchUnavailable) so the Go compiler arbitrates a wrong
// wire loudly — never a silent typed-zero. Only when no Infra field name-
// matches AND none is provably assignable does resolution return "" (the
// caller then raises MissingProvider for a required collaborator).

package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/packages"
)

// InfraField is one exported field on the owned *Infra struct, parsed from
// internal/app via AST (the cheap path — matches ParseAppFields). Type is
// the pretty-printed declared type for the exact-string fast path.
type InfraField struct {
	Name string
	Type string
}

// parseInfraFields walks every non-test .go file in internal/app and
// returns the exported fields of the `Infra` struct. Returns an empty map
// when internal/app or the Infra struct doesn't exist yet (first generate,
// before providers.go is scaffolded) — every collaborator then falls to
// the compile-time backstop / MissingProvider path, which is the correct
// loud state.
func parseInfraFields(appDir string) (map[string]InfraField, error) {
	return parseStructFields(appDir, "Infra")
}

// parseStructFields walks every non-test .go file in dir and returns the
// exported fields of the named struct (keyed by field name). Returns an empty
// map when dir or the struct doesn't exist yet (first generate, before the
// file is scaffolded) — callers degrade to their loud/backstop path. Shared by
// parseInfraFields (Infra) and parseConfigFields (Config).
func parseStructFields(dir, structName string) (map[string]InfraField, error) {
	out := map[string]InfraField{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, err
	}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, parser.SkipObjectResolution)
		if perr != nil {
			continue
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != structName {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range st.Fields.List {
					if len(field.Names) == 0 {
						continue
					}
					typeStr := printType(fset, field.Type)
					for _, name := range field.Names {
						if !ast.IsExported(name.Name) {
							continue
						}
						if _, exists := out[name.Name]; !exists {
							out[name.Name] = InfraField{Name: name.Name, Type: typeStr}
						}
					}
				}
			}
		}
	}
	return out, nil
}

// InfraAssignabilityMatcher answers "which Infra field fills this Deps
// field type?" for a component. One instance per generate run; methods are
// safe for concurrent use within a generate (the cache mutex serializes
// loads).
type InfraAssignabilityMatcher struct {
	projectDir string
	mu         sync.Mutex
	universes  map[string]*infraUniverse // roleRoot|pkgDir -> universe
}

// infraUniverse is the cached result of one joint (internal/app +
// component) load. ok=false means assignability cannot be proven for any
// field this run (load error or type errors) — ResolveInfraField degrades
// to the exact-name backstop only.
type infraUniverse struct {
	ok          bool
	infraFields map[string]types.Type // Infra exported fields -> declared type
	depsFields  map[string]types.Type // component Deps exported fields -> declared type
}

// NewInfraAssignabilityMatcher returns a matcher rooted at projectDir (the
// directory containing go.mod / internal/app / internal/handlers / ...).
func NewInfraAssignabilityMatcher(projectDir string) *InfraAssignabilityMatcher {
	return &InfraAssignabilityMatcher{projectDir: projectDir, universes: map[string]*infraUniverse{}}
}

// ResolveInfraField returns the Infra field name that should fill the named
// Deps field, plus the MatchKind classifying the proof:
//
//   - MatchAssignable   — an Infra field is PROVEN assignable to depsType.
//   - MatchExactString  — an exact-name Infra field has a byte-equal type
//     string (no go/types load needed).
//   - MatchUnavailable  — an exact-name Infra field exists but assignability
//     is unproven; emit it as the compile-time backstop.
//   - (empty field, MatchNoName) — no Infra field fills this type; the
//     caller raises MissingProvider for a required collaborator.
//
// Priority: an exact-name Infra field whose type is byte-equal (fast path);
// then any provably-assignable Infra field (the narrow-interface case),
// with a deterministic pick when several are assignable; then an exact-name
// Infra field as the unproven backstop.
func (m *InfraAssignabilityMatcher) ResolveInfraField(roleRoot, pkgDir, depsFieldName, depsType string, infraFields map[string]InfraField) (string, MatchKind) {
	// Fast path: exact-name + byte-equal type string. No load required.
	if f, ok := infraFields[depsFieldName]; ok && f.Type == depsType {
		return f.Name, MatchExactString
	}

	// Type-checked path: load internal/app + the component jointly.
	m.mu.Lock()
	defer m.mu.Unlock()
	key := roleRoot + "|" + pkgDir
	u, loaded := m.universes[key]
	if !loaded {
		u = m.loadUniverseLocked(roleRoot, pkgDir)
		m.universes[key] = u
	}

	if u.ok {
		depsT, ok := u.depsFields[depsFieldName]
		if ok {
			// Prefer the exact-name Infra field when it is assignable.
			if infraT, ok := u.infraFields[depsFieldName]; ok && infraAssignable(infraT, depsT) {
				return depsFieldName, MatchAssignable
			}
			// Otherwise any assignable Infra field, deterministic by name.
			var names []string
			for n := range u.infraFields {
				names = append(names, n)
			}
			sort.Strings(names)
			for _, n := range names {
				if infraAssignable(u.infraFields[n], depsT) {
					return n, MatchAssignable
				}
			}
		}
	}

	// Backstop: an exact-name Infra field exists but assignability is
	// unproven. Emit it; the compiler arbitrates. (Never reached when the
	// fast path already matched a byte-equal type.)
	if f, ok := infraFields[depsFieldName]; ok {
		return f.Name, MatchUnavailable
	}
	return "", MatchNoName
}

// infraAssignable reports whether infraT is assignable to depsT, covering
// the interface-implementation case explicitly (the narrow-interface fill).
func infraAssignable(infraT, depsT types.Type) bool {
	if types.AssignableTo(infraT, depsT) {
		return true
	}
	if iface, ok := depsT.Underlying().(*types.Interface); ok && types.Implements(infraT, iface) {
		return true
	}
	return false
}

// loadUniverseLocked runs ONE packages.Load covering both internal/app and
// roleRoot/pkgDir, then indexes each side's fields. Caller holds m.mu.
func (m *InfraAssignabilityMatcher) loadUniverseLocked(roleRoot, pkgDir string) *infraUniverse {
	u := &infraUniverse{}
	absProject, err := filepath.Abs(m.projectDir)
	if err != nil {
		return u
	}
	appDir := filepath.Join(absProject, "internal", "app")
	compDir := filepath.Join(absProject, filepath.FromSlash(roleRoot), filepath.FromSlash(pkgDir))

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedDeps | packages.NeedImports |
			packages.NeedSyntax,
		Dir: absProject,
	}
	appPattern := "./" + path.Join("internal", "app")
	compPattern := "./" + path.Join(filepath.ToSlash(roleRoot), filepath.ToSlash(pkgDir))
	pkgs, err := packages.Load(cfg, appPattern, compPattern)
	if err != nil || len(pkgs) == 0 {
		return u
	}
	var appPkg, compPkg *packages.Package
	for _, p := range pkgs {
		dir := packageDir(p)
		switch {
		case sameDir(dir, appDir):
			appPkg = p
		case sameDir(dir, compDir):
			compPkg = p
		}
	}
	if appPkg == nil || len(appPkg.Errors) > 0 || appPkg.Types == nil {
		return u
	}
	if compPkg == nil || len(compPkg.Errors) > 0 || compPkg.Types == nil {
		return u
	}
	u.ok = true
	u.infraFields = collectInfraFieldTypes(appPkg)
	u.depsFields = collectDepsFieldTypes(compPkg)
	return u
}

// collectInfraFieldTypes walks internal/app's loaded types for the `Infra`
// struct and returns its exported fields by name. Mirrors
// collectAppFieldTypes but for the owned Infra struct.
func collectInfraFieldTypes(pkg *packages.Package) map[string]types.Type {
	out := map[string]types.Type{}
	obj := pkg.Types.Scope().Lookup("Infra")
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
		if !f.Exported() || f.Anonymous() {
			continue
		}
		if _, exists := out[f.Name()]; exists {
			continue
		}
		out[f.Name()] = f.Type()
	}
	return out
}
