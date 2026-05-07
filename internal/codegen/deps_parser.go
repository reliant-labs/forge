package codegen

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"strings"
)

// DepsField describes one field of a service's `Deps` struct as parsed
// from handlers/<svc>/service.go (or any non-test .go file in the dir).
//
// The wire_gen codegen consumes these to emit one assignment per field
// in the per-service `wireXxxDeps(app, cfg)` function. Type is the
// pretty-printed Go expression (selector / star / ident / etc.) so
// downstream consumers can do simple string contains checks
// (e.g. "*sql.DB", "orm.Context") without re-walking the AST.
type DepsField struct {
	// Name is the exported Go field name as written in the Deps struct,
	// e.g. "Logger", "Config", "Authorizer", "Repo", "Audit", "DB".
	Name string

	// Type is the pretty-printed Go type expression, e.g. "*slog.Logger",
	// "*config.Config", "middleware.Authorizer", "orm.Context", "*sql.DB",
	// "Repository". Used by wire_gen to emit zero-values when no
	// producer matches and to render TODO comments that name the type
	// the user needs to wire.
	Type string

	// Optional is true when the field's doc / inline comment carries
	// the `// forge:optional-dep` marker. Optional fields are
	// intentionally allowed to be nil at construction time:
	//   - validateDeps should NOT enforce them (the user manages
	//     `if s.deps.X != nil { ... }` per RPC as idiomatic Go).
	//   - wire_gen emits the typed zero silently — no TODO comment,
	//     no contribution to the UNRESOLVED header — when no producer
	//     matches.
	// The marker exists because some Deps fields are legitimately
	// optional (rollback-only NATS publisher, optional gateway
	// features, etc.) and the default "must resolve" treatment forces
	// users to either fake-wire them or drop them from validateDeps
	// entirely. Both defeat the design intent.
	Optional bool
}

// ParseServiceDeps reads handlers/<svc>/<*>.go (skipping test files) and
// returns the ordered list of fields declared on the package-level
// `Deps` struct. Returns an empty slice if the directory doesn't exist
// or no Deps struct is found — caller treats those as "service has no
// rich deps to wire".
//
// Modeled on DetectDepsDBField (same fast AST walk, same skip-test-files
// behavior). Kept as a separate function rather than overloaded so
// DetectDepsDBField stays a constant-time predicate that doesn't have
// to allocate a slice.
func ParseServiceDeps(dir string) ([]DepsField, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		// ParseComments so we can read the `// forge:optional-dep`
		// marker that may sit in the field's doc-comment slot. Without
		// it field.Doc / field.Comment are nil and the marker would be
		// invisible to wire_gen / validateDeps.
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			continue
		}

		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Name.Name != "Deps" {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				return collectDepsFields(fset, structType), nil
			}
		}
	}
	return nil, nil
}

// HasOptionalDepMarker returns true if any line in the comment text
// is *exactly* the `forge:optional-dep` directive (after stripping
// leading slashes / whitespace and trailing whitespace). The strict
// match — directive must be the whole line, not embedded inside
// surrounding prose — prevents documentation that references the
// marker (e.g. an example block in the scaffolded service.go that
// says "tag it with the `// forge:optional-dep` marker on the line
// above") from being interpreted as the marker itself.
//
// Exported so the lint rule (forgeconv-optional-dep-marker-position)
// can share the exact same recognition logic the parser uses — the
// rule is "the marker is on a Deps field; anywhere else is a typo /
// misuse" and the lint check needs to scan for the marker in places
// it shouldn't be.
func HasOptionalDepMarker(text string) bool {
	const needle = "forge:optional-dep"
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.Trim(line, "/ \t")
		if trimmed == needle {
			return true
		}
	}
	return false
}

// collectDepsFields flattens the Deps struct into one DepsField per
// declared name. A single field declaration like `A, B *slog.Logger`
// emits two fields with the same Type. Anonymous (embedded) fields are
// skipped — wire_gen wouldn't know what to name on the assignment side.
//
// The Optional bit is set when either field.Doc (the comment block
// directly above the field) or field.Comment (the inline trailing
// comment on the same line) contains `// forge:optional-dep`. Multi-
// name field declarations (`A, B *T`) propagate the marker to every
// name — they share the same comment slot.
func collectDepsFields(fset *token.FileSet, st *ast.StructType) []DepsField {
	var out []DepsField
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			continue
		}
		typeStr := printType(fset, field.Type)
		optional := false
		if field.Doc != nil && HasOptionalDepMarker(field.Doc.Text()) {
			optional = true
		}
		if !optional && field.Comment != nil && HasOptionalDepMarker(field.Comment.Text()) {
			optional = true
		}
		for _, name := range field.Names {
			if !ast.IsExported(name.Name) {
				continue
			}
			out = append(out, DepsField{
				Name:     name.Name,
				Type:     typeStr,
				Optional: optional,
			})
		}
	}
	return out
}

// printType pretty-prints an ast.Expr as the Go source it came from.
// Uses go/printer so selector expressions ("orm.Context") and pointer
// types ("*slog.Logger") render verbatim instead of needing a custom
// switch over every possible Expr kind.
func printType(fset *token.FileSet, expr ast.Expr) string {
	var buf strings.Builder
	if err := printer.Fprint(&buf, fset, expr); err != nil {
		return ""
	}
	return buf.String()
}

// AppField describes one exported field on the project-generated *App
// struct (pkg/app/bootstrap.go) plus any user-extension files in the
// same package. wire_gen consumes these to resolve unconventional
// service Deps fields by name → app.<Field>.
//
// Type is parallel to DepsField.Type so callers can do an exact
// string-equal match when they want to be conservative, or a contains
// check when they want to tolerate alias differences.
type AppField struct {
	Name string
	Type string
}

// ParseAppFields walks every non-test .go file in pkg/app and returns
// the union of fields reachable as `app.<Name>` on a *App. The set
// includes:
//
//   - Direct exported fields of the `App` struct itself (forge-owned in
//     pkg/app/app_gen.go: Services, Workers, Operators, Packages, DB,
//     ORM).
//   - Direct exported fields of the `AppExtras` struct (user-owned in
//     pkg/app/app_extras.go). AppExtras is embedded into App as a
//     pointer; Go's field promotion rules make those fields reachable
//     via `app.<Field>` at the call site even though they live on a
//     different struct. wire_gen treats them identically.
//
// Anonymous (embedded) fields on App are skipped from the result —
// they don't have a usable selector name on their own, and the only
// embedded type we generate (*AppExtras) is unwrapped above.
//
// Returns an empty slice if pkg/app doesn't exist yet (initial scaffold
// path) — caller treats that as "wire_gen has nothing to look up by
// name" and falls back to the conventional set.
func ParseAppFields(appDir string) ([]AppField, error) {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	// Collect by struct name across all files in pkg/app, then unify.
	// This is the cleanest way to support the "App in app_gen.go +
	// AppExtras in app_extras.go" split without picking a file order.
	byStruct := map[string][]AppField{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(appDir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if typeSpec.Name.Name != "App" && typeSpec.Name.Name != "AppExtras" {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, field := range structType.Fields.List {
					if len(field.Names) == 0 {
						// Anonymous/embedded — skip. The only one we
						// expect on App is *AppExtras, whose fields
						// we collect from the AppExtras struct itself.
						continue
					}
					typeStr := printType(fset, field.Type)
					for _, name := range field.Names {
						if !ast.IsExported(name.Name) {
							continue
						}
						byStruct[typeSpec.Name.Name] = append(
							byStruct[typeSpec.Name.Name],
							AppField{Name: name.Name, Type: typeStr},
						)
					}
				}
			}
		}
	}

	// Merge App + AppExtras. App fields take precedence on collision —
	// a forge-owned field name (e.g. "DB") shouldn't be shadowed by an
	// accidental same-name field on AppExtras.
	seen := map[string]bool{}
	var fields []AppField
	for _, f := range byStruct["App"] {
		if seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		fields = append(fields, f)
	}
	for _, f := range byStruct["AppExtras"] {
		if seen[f.Name] {
			continue
		}
		seen[f.Name] = true
		fields = append(fields, f)
	}
	return fields, nil
}
