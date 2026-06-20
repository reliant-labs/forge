package codegen

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// BootstrapComponentData represents a bootstrappable component (service, package, worker, operator).
//
// For nested internal packages (e.g. internal/mcp/database/contract.go), Package
// is the leaf Go-identifier ("database"), while ImportPath carries the full
// path under internal/ ("mcp/database") used to construct the import line. For
// top-level packages, ImportPath is the same as Package. FieldName must remain
// unique across all packages, so for nested entries it should encode the full
// path (e.g. "McpDatabase") to avoid collisions with sibling leaves. VarName is
// the lowerCamel form of FieldName and is used in the bootstrap template as a
// unique prefix for local Go variables (e.g. "mcpDatabaseImpl"); using FieldName
// avoids collisions when two nested packages share a leaf name.
type BootstrapComponentData struct {
	Name       string // e.g. "api", "cache", "email_sender"
	Package    string // e.g. "api" (Go package identifier — leaf of ImportPath for nested entries)
	ImportPath string // e.g. "api" or "mcp/database" (path under internal/, workers/, etc.)
	FieldName  string // e.g. "API" or "McpDatabase" (exported struct field, must be unique)
	VarName    string // e.g. "api" or "mcpDatabase" (unique lowerCamel local-var prefix in bootstrap)
	Fallible   bool   // true if New() returns (T, error)
	// Alias is the import alias used in bootstrap.go for this component's
	// Go package. Defaults to Package when there are no cross-role
	// collisions; gets a role-prefixed value (e.g. "svcBilling",
	// "pkgBilling") when a service Package matches an internal package
	// Package (or other cross-role pair). All bootstrap.go references
	// to the package's exported symbols must use Alias rather than
	// Package so the alias-rewrite is observed at every call site.
	Alias string
	// HasWebhooks is true when this service has webhooks declared in
	// forge.yaml. The bootstrap template uses this to emit a
	// `RegisterWebhookRoutes(mux, stack)` call after `RegisterHTTP(...)`,
	// so generated webhook routes get mounted on the mux without the user
	// having to hand-edit the user-owned `RegisterHTTP` body in
	// handlers/<svc>/service.go. Only populated for services; ignored
	// for packages, workers, and operators.
	HasWebhooks bool
	// HasLogger / HasConfig record whether this component's Deps struct
	// declares a Logger / Config field. The bootstrap template gates the
	// emission of those Deps-literal fields on these flags so a package
	// that doesn't consume them isn't forced to carry vestigial Logger /
	// Config fields just to keep the generated New(Deps{...}) call site
	// type-checking. Populated by inspectComponentDepsShape before
	// rendering; defaults to false (skip) when the source dir can't be
	// parsed (e.g. just-scaffolded component with no Deps yet).
	HasLogger bool
	HasConfig bool
	// AppFieldRefs lists Deps fields (other than Logger/Config) whose
	// names AND types match an AppExtras field. Bootstrap emits one
	// assignment per entry like `<DepsField>: app.<DepsField>`. Without
	// this, audit.New got only {Logger} even when audit.Deps.Repo and
	// app.Repo both existed — the package silently degraded (Log warn-
	// and-drops) until the next forge generate cycle. wire_gen has had
	// this logic for services; this brings packages to parity.
	AppFieldRefs []AppFieldRef
	// CanonicalAppField names the single App/AppExtras field whose
	// declared type is this internal package's Service interface (e.g.
	// AppExtras.DaemonService of type svcdaemon.Service). Populated by
	// inspectComponentDepsShape ONLY when the package's Deps struct has
	// at least one non-optional collaborator field that bootstrap cannot
	// auto-wire from App/AppExtras (no name match, or proven type
	// mismatch) — i.e. the construction is unexpressible from app
	// fields. When set, the bootstrap template emits
	//
	//	app.Packages.<FieldName> = app.<CanonicalAppField>
	//
	// instead of `<pkg>.New(<pkg>.Deps{...})`: user-owned setup.go
	// constructs the canonical, fully-wired instance (with deps that
	// have no AppExtras representation — inline URL builders,
	// env-derived strings, cross-package collaborators), and appkit
	// runs Setup BEFORE the package table, so the alias always observes
	// the setup.go assignment. Constructing a second instance here
	// instead would produce a half-built duplicate that panics in
	// validateDeps or silently no-ops — the cp-forge svcdaemon
	// hand-edit class (Deps.DaemonRepo/URLBuilder unwireable, boot
	// panic "Deps.DaemonRepo is required").
	//
	// Empty when: every Deps field auto-wires (keep constructing — the
	// enforcement/Checker shape), the only gaps are config scalars
	// (zero value is the documented degraded mode — the billing/APIKey
	// shape), the gap is `forge:optional-dep`-marked, no App/AppExtras
	// field has the package's Service type, or more than one does
	// (ambiguous — no deterministic canonical instance).
	CanonicalAppField string
	// ConnectPkg is the import alias of the generated Connect package for
	// this service (e.g. "echov1connect") — used by the bootstrap template
	// to reference the `<X>ServiceName` constant when building vanguard
	// REST services. Only populated for services and only when
	// `api.rest: true`; empty for non-services or when REST is off.
	ConnectPkg string
	// ProtoServiceName is the PascalCase proto service identifier (e.g.
	// "EchoService") used to look up the `<ProtoServiceName>Name` constant
	// in the connect-generated package. Combined with ConnectPkg, the
	// bootstrap template emits `<connectPkg>.<ProtoServiceName>Name` as
	// the Connect path passed to vanguard.NewService.
	ProtoServiceName string
}

// AppFieldRef pairs a package Deps field name with the app.<name>
// expression bootstrap should emit for it. Only emitted when the
// AppExtras field type EXACTLY matches the Deps field type (otherwise
// the compile fails with funding.Repository vs *db.PostgresRepository
// style mismatches).
type AppFieldRef struct {
	DepsField string // e.g. "Repo"
}

// Type aliases for backward compatibility and readability.
type BootstrapServiceData = BootstrapComponentData
type BootstrapPackageData = BootstrapComponentData
type BootstrapWorkerData = BootstrapComponentData

// inspectComponentDepsShape walks each component's source directory under
// roleRoot (e.g. "internal", "workers", "operators") and populates
// HasLogger / HasConfig / AppFieldRefs from the parsed Deps struct +
// AppExtras AST. A missing source dir or unparseable file falls through
// to defaults: best-effort, errors-as-default.
//
// Mutates each component in place so callers don't have to thread a
// result slice through bootstrap data assembly.
func inspectComponentDepsShape(components []BootstrapComponentData, projectDir, roleRoot string) {
	// Resolve AppExtras field types once for the whole batch so each
	// component can name-and-type-match without re-parsing pkg/app.
	appFields, _ := ParseAppFields(filepath.Join(projectDir, "pkg", "app"))
	appFieldTypes := map[string]string{}
	for _, f := range appFields {
		appFieldTypes[f.Name] = f.Type
	}

	// Type-aware backstop for the legacy string-compare matcher.
	// When the pretty-printed type strings differ but go/types says
	// the AppExtras field is assignable to the Deps field (narrow-
	// interface implemented by wide concrete), we wire anyway. When
	// the matcher reports NameMismatch, we deliberately DO NOT wire —
	// the post-generate bootstrap-deps-coverage lint surfaces the
	// gap loudly rather than silently dropping (this was the audit-
	// no-op bug class).
	matcher := NewDepsAssignabilityMatcher(projectDir)

	// Module path feeds canonicalAppServiceField's exact import-path
	// compare; empty (no go.mod — synthetic fixtures) falls back to a
	// suffix match there.
	modulePath, _ := GetModulePath(projectDir)

	for i := range components {
		dir := filepath.Join(projectDir, roleRoot, components[i].ImportPath)
		fields, err := ParseServiceDeps(dir)
		if err != nil || len(fields) == 0 {
			continue
		}
		// unwiredCollaborators counts non-optional, non-scalar Deps
		// fields that bootstrap cannot supply from App/AppExtras. When
		// non-zero, the generated New(Deps{...}) is a half-built
		// instance — see CanonicalAppField for the alias escape hatch.
		unwiredCollaborators := 0
		for _, f := range fields {
			switch f.Name {
			case "Logger":
				// Logger is the project's *slog.Logger. Gate emission
				// on the declared type matching to avoid stomping on a
				// package-local Logger type.
				if f.Type == "" || f.Type == "*slog.Logger" {
					components[i].HasLogger = true
				} else if !f.Optional && !isConfigScalarType(f.Type) {
					// Domain-local logger type (e.g. logr.Logger) that
					// bootstrap cannot supply.
					unwiredCollaborators++
				}
			case "Config":
				// HasConfig gates emission of `Config: cfg` in the
				// bootstrap template — but `cfg` is the project's
				// `*config.Config`. If the package declares a
				// domain-local Config (e.g. enforcement.Config) the
				// emit would produce a hard type-mismatch at codegen
				// time. Gate on the type-string matching the project
				// config so a domain Config field gets the typed-zero
				// default and the user wires it manually in setup.go
				// (or via an AppExtras field that matches the type).
				// FRICTION 2026-06-02: cp-forge layer-2 enforcement.
				if f.Type == "" || f.Type == "*config.Config" {
					components[i].HasConfig = true
				} else {
					// Domain-local Config — defer to the name+type
					// matcher (with assignability) like any other field.
					if matchedAppField(matcher, roleRoot, components[i].ImportPath, f.Name, f.Type, appFieldTypes) {
						components[i].AppFieldRefs = append(components[i].AppFieldRefs, AppFieldRef{DepsField: f.Name})
					} else if !f.Optional && !isConfigScalarType(f.Type) {
						unwiredCollaborators++
					}
				}
			default:
				// Name-match + assignability check. The legacy version
				// required byte-equal type strings, which silently
				// dropped the wire when funding.Deps.Repo was
				// funding.Repository (narrow interface) and
				// AppExtras.Repo was *db.PostgresRepository (concrete
				// impl). Using go/types via the matcher closes the
				// silent-drop class without losing the safety of the
				// no-name-match → no-emit invariant (the existing
				// optional-dep mechanism).
				if matchedAppField(matcher, roleRoot, components[i].ImportPath, f.Name, f.Type, appFieldTypes) {
					components[i].AppFieldRefs = append(components[i].AppFieldRefs, AppFieldRef{DepsField: f.Name})
				} else if !f.Optional && !isConfigScalarType(f.Type) {
					unwiredCollaborators++
				}
			}
		}
		// The construction is unexpressible from App/AppExtras fields:
		// at least one live collaborator stays unwired, so the emitted
		// New(Deps{...}) would be a half-built duplicate of whatever
		// the user constructs in setup.go (panicking in validateDeps or
		// silently no-opping). If the user maintains the canonical
		// instance on exactly one App/AppExtras field of this package's
		// Service type, alias the Packages slot to it instead — Setup
		// runs before the package table, so the assignment is always
		// observed. Packages-only: workers/operators have no
		// canonical-instance convention.
		if unwiredCollaborators > 0 && roleRoot == "internal" {
			components[i].CanonicalAppField = canonicalAppServiceField(
				filepath.Join(projectDir, "pkg", "app"),
				modulePath, roleRoot, components[i].ImportPath, components[i].Package)
		}
	}
}

// isConfigScalarType reports whether a Deps field type is a plain
// configuration scalar (string, bool, numeric, or a slice of those —
// e.g. APIKey string, PlansData []byte, AllowedHosts []string).
// Scalars are never auto-wired from App/AppExtras by name today, and
// their zero value is the package's documented degraded mode rather
// than a nil collaborator — so they don't count toward the
// "construction is unexpressible" trigger for CanonicalAppField.
func isConfigScalarType(t string) bool {
	t = strings.TrimPrefix(strings.TrimSpace(t), "[]")
	switch t {
	case "string", "bool", "byte", "rune",
		"int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "uintptr",
		"float32", "float64", "complex64", "complex128",
		"time.Duration":
		return true
	}
	return false
}

// canonicalAppServiceField finds the single exported App/AppExtras
// field whose declared type is `<pkg>.Service` for the internal
// package at <module>/<roleRoot>/<importPath>. Returns "" unless
// exactly one such field exists (zero → nothing to alias; two or more
// → ambiguous, no deterministic canonical instance).
//
// Resolution follows each pkg/app file's own import table so renamed
// imports work (cp-forge declares `internaluser "…/internal/user"` and
// types the field `internaluser.Service`). A plain import's qualifier
// is the package's real package-clause name (pkgName, disk-resolved by
// the caller), NOT the directory leaf — the two may legally differ.
// When modulePath is empty (no go.mod — synthetic fixtures), import
// paths match by "/<roleRoot>/<importPath>" suffix instead.
func canonicalAppServiceField(appDir, modulePath, roleRoot, importPath, pkgName string) string {
	entries, err := os.ReadDir(appDir)
	if err != nil {
		return ""
	}
	want := ""
	if modulePath != "" {
		want = modulePath + "/" + roleRoot + "/" + importPath
	}
	suffix := "/" + roleRoot + "/" + importPath

	fset := token.NewFileSet()
	seen := map[string]struct{}{}
	var fieldNames []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(appDir, entry.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			continue
		}
		// Qualifiers in THIS file that refer to the target package.
		quals := map[string]struct{}{}
		for _, imp := range file.Imports {
			if imp.Path == nil {
				continue
			}
			p := strings.Trim(imp.Path.Value, `"`)
			matched := p == want
			if want == "" {
				matched = strings.HasSuffix(p, suffix)
			}
			if !matched {
				continue
			}
			if imp.Name != nil {
				if imp.Name.Name == "_" || imp.Name.Name == "." {
					continue
				}
				quals[imp.Name.Name] = struct{}{}
			} else {
				quals[pkgName] = struct{}{}
			}
		}
		if len(quals) == 0 {
			continue
		}
		for _, decl := range file.Decls {
			genDecl, ok := decl.(*ast.GenDecl)
			if !ok || genDecl.Tok != token.TYPE {
				continue
			}
			for _, spec := range genDecl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || (typeSpec.Name.Name != "App" && typeSpec.Name.Name != "AppExtras") {
					continue
				}
				structType, ok := typeSpec.Type.(*ast.StructType)
				if !ok || structType.Fields == nil {
					continue
				}
				for _, field := range structType.Fields.List {
					sel, ok := field.Type.(*ast.SelectorExpr)
					if !ok || sel.Sel == nil || sel.Sel.Name != "Service" {
						continue
					}
					qual, ok := sel.X.(*ast.Ident)
					if !ok {
						continue
					}
					if _, ok := quals[qual.Name]; !ok {
						continue
					}
					for _, name := range field.Names {
						if !name.IsExported() {
							continue
						}
						if _, dup := seen[name.Name]; dup {
							continue
						}
						seen[name.Name] = struct{}{}
						fieldNames = append(fieldNames, name.Name)
					}
				}
			}
		}
	}
	if len(fieldNames) == 1 {
		return fieldNames[0]
	}
	return ""
}

// matchedAppField is the central decision for "should bootstrap emit
// `<Field>: app.<Field>` for this Deps field?". It consults the
// type-aware matcher with a string-compare pre-check.
//
// Returns true when:
//   - the matcher confirms assignability (narrow-interface case), or
//   - the matcher confirms exact-string match (legacy fast path), or
//   - the matcher is unavailable (assignability unprovable: load
//     failure, project mid-edit). Wiring the name match here is the
//     deterministic fail-loud policy (deps_assignability.go header):
//     the compiler arbitrates a wrong wire loudly, while dropping it
//     silently un-wires a live collaborator with no signal. It also
//     matches what wire_gen's consumer does for Unavailable, so
//     bootstrap and wire_gen can't diverge on the same project state.
//     (The previous Unavailable fallback — wire only on byte-equal
//     type strings — made bootstrap output depend on whether the
//     project type-checked mid-pipeline: kalshi FORGE_BACKLOG #13's
//     nondeterminism class.)
//
// Returns false when:
//   - no AppExtras field of the same name exists (the optional-dep
//     invariant), or
//   - name matches but types are PROVEN not assignable (NameMismatch
//     in a single shared type universe — the lint surfaces the gap).
func matchedAppField(m *DepsAssignabilityMatcher, roleRoot, pkgDir, depsName, depsType string, appByName map[string]string) bool {
	appType, hasName := appByName[depsName]
	if !hasName {
		return false
	}
	kind := m.Match(roleRoot, pkgDir, depsName, depsType, appType, true)
	switch kind {
	case MatchExactString, MatchAssignable:
		return true
	case MatchNameMismatch:
		// Intentional silence here — the lint reports loudly. We don't
		// emit a wire that won't compile, AND we don't pretend the
		// field doesn't exist (which would have skipped validateDeps).
		return false
	case MatchUnavailable:
		// Unproven ≠ mismatched. Wire the name match; see the policy
		// note in the function doc.
		return true
	default:
		return false
	}
}

// GenerateBootstrap generates pkg/app/bootstrap.go from the bootstrap.go.tmpl template.
//
// hasDatabase gates the DB field + setupDatabase wiring; ormEnabled gates the
// ORM field + generated ORM client construction. The two are separate
// concerns: a project may configure a DB driver (for migrations, raw SQL,
// sqlc) without opting into the generated forge ORM. The ORM field is
// dropped when no proto/db/ entity definitions exist so `App.ORM` can never
// be silently nil in user code.
//
// Reads forge.yaml to detect `binary: shared` mode; in that mode the rendered
// `BootstrapOnly` lazily constructs each service inside its name-gated block
// instead of constructing all up-front, so per-service cobra subcommands
// (`./<bin> api`) actually scope-down their dependency graph.
//
// cs is the project's checksum tracker — passing it keeps pkg/app/bootstrap.go
// out of `forge audit`'s tracked-files drift report. A nil cs is tolerated.
//
// webhookServices is keyed by snake-case service package name (e.g.
// "admin_server") and indicates which services have webhooks declared in
// forge.yaml. The bootstrap template emits `RegisterWebhookRoutes(mux,
// stack)` after `RegisterHTTP(...)` for those services so generated
// webhook routes get auto-mounted on the mux without the user having to
// hand-edit the user-owned `RegisterHTTP` body. Pass nil if no services
// have webhooks. (2026-04-30 LLM-port: introduced as part of the
// auto-wire fix — pre-fix, projects had to manually edit service.go to
// call s.RegisterWebhookRoutes(mux, stack).)
// BootstrapFeatures carries the per-project feature toggles that
// influence what bootstrap.go's body emits. Added as a struct (not yet
// another bool) so future feature flags can land additively without
// re-shaping the GenerateBootstrap signature; today the struct only
// carries diagnostics + strict-wiring, but the same pattern absorbs
// later flags cleanly.
type BootstrapFeatures struct {
	// DiagnosticsEnabled is true when the project opts in to
	// `features.diagnostics: true`. Drives whether the template emits
	// the diagnostics.Default.Boot(emitter) call after Setup. Default
	// false so existing projects don't suddenly start logging warns on
	// regen.
	DiagnosticsEnabled bool

	// StrictWiringEnabled is true when the project opts in to
	// `features.strict_wiring: true`. Implies DiagnosticsEnabled at the
	// wire site — strict-mode wraps the LogEmitter in StrictEmitter so
	// any registered diagnostic exits the process after the summary
	// line. Default false.
	StrictWiringEnabled bool

	// AllServiceNames is the project's COMPLETE Connect-service
	// inventory (runtime kebab names, e.g. "admin-server") — including
	// services the caller filtered out of the services slice because
	// pkg/app/services.go does not register them (types-only). The
	// template renders it into BootstrapOnly's registration guard: a
	// filter name in this inventory that has no row in the
	// RegisteredServices result fails with a pointed error naming
	// pkg/app/services.go instead of appkit's generic unknown-name
	// warning. The guard checks the LIVE def.Services rows, so it stays
	// truthful even when services.go is edited without a regenerate.
	AllServiceNames []string
}

// leaderElectionID derives a Kubernetes-valid leader-election lease name
// from the project's module path. Lease names are k8s resource names and
// must satisfy DNS-1123 label rules (lowercase alphanumerics and '-');
// the raw module path ("github.com/acme/control-plane") contains slashes
// and dots, which the API server rejects with "may not contain '/'". Use
// the module's final path element, lowercased with every invalid rune
// squashed to '-', suffixed with "-leader" (e.g. "control-plane-leader").
func leaderElectionID(modulePath string) string {
	base := modulePath
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.ToLower(base)
	var b strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		slug = "forge"
	}
	return slug + "-leader"
}

func GenerateBootstrap(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, databaseDriver string, ormEnabled bool, projectDir string, configFields map[string]bool, webhookServices map[string]bool, features BootstrapFeatures, cs *checksums.FileChecksums) error {
	hasDatabase := databaseDriver != ""
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	restEnabled := projectAPIRESTEnabled(projectDir)

	var bootstrapSvcs []BootstrapServiceData
	var connectImports []string
	for _, svc := range services {
		// Disk-first: the handler directory + its package clause are the
		// source of truth for the import line and the package selector.
		// Synthesis (naming.ServicePackage) only kicks in when the directory
		// hasn't been scaffolded yet. See disk_resolver.go for the full
		// rationale (kalshi-trader broken-imports bug class).
		res, err := ResolveServiceComponent(projectDir, svc.Name)
		if err != nil {
			return err
		}
		pkg := res.PackageName
		fallible, _ := DetectFallibleConstructor(res.Dir)
		// FieldName mirrors the PascalCase proto-service name without the
		// "Service" suffix ("AdminServerService" -> "AdminServer"). We derive
		// it from svc.Name (which retains separators / PascalCase boundaries)
		// rather than from pkg, because pkg may be a legacy compact clause
		// ("adminserver") that ToPascalCase can't split back into words.
		fieldName := naming.ToPascalCase(strings.TrimSuffix(svc.Name, "Service"))
		if fieldName == "" {
			fieldName = naming.ToPascalCase(svc.Name)
		}
		// Runtime name is the kebab form of the original svc.Name (proto
		// PascalCase) — matches what cobra subcommands pass to runServer
		// (cmd-services-gen.go.tmpl uses {{.Name}} which is the
		// kebab form from forge.yaml). Pre-2026-04-30 this was derived
		// from the snake-case package name, which silently dropped the cobra
		// invocation under shared-binary mode (admin-server vs admin_server).
		// The kebab form preserves the original word boundaries that the
		// compact pkg form loses.
		runtimeName := naming.ToKebabCase(strings.TrimSuffix(svc.Name, "Service"))
		// ConnectPkg + ProtoServiceName drive the vanguard.NewService call
		// site when REST is enabled. The proto service name is the proto
		// handler name (e.g. "EchoService") which matches the `<X>Name`
		// constant exposed by connect-generated packages.
		//
		// Prefer the proto's declared go_package + PkgName so a service
		// whose proto lives outside the `services/<svc>/v1` convention
		// (e.g. several services collected in a single shared.proto under
		// gen/reliant/v1) still emits the correct *v1connect import. Fall
		// back to the convention only when both descriptor fields are
		// empty — synthetic test fixtures and pre-descriptor scaffolds.
		// The fallback synthesizes from svc.Name (NOT the disk-resolved
		// pkg): the gen/ path mirrors the proto layout, not the handler
		// dir's possibly-legacy package clause.
		var connectPkg, connectImport string
		if svc.GoPackage != "" && svc.PkgName != "" {
			connectPkg = svc.PkgName + "connect"
			connectImport = svc.GoPackage + "/" + connectPkg
		} else {
			synth := naming.ServicePackage(svc.Name)
			connectPkg = synth + "v1connect"
			connectImport = modulePath + "/gen/services/" + synth + "/v1/" + connectPkg
		}
		protoServiceName := fieldName + "Service"
		if restEnabled {
			connectImports = append(connectImports, connectImport)
		}
		bootstrapSvcs = append(bootstrapSvcs, BootstrapServiceData{
			Name:       runtimeName,
			Package:    pkg,
			ImportPath: res.ImportLeaf,
			// Use ToPascalCase so multi-word service packages produce the
			// same exported field name as the unit/integration test templates,
			// which call `app.NewTest{{.ServiceName | pascalCase}}` (e.g.
			// "admin_server" -> "AdminServer").
			FieldName: fieldName,
			VarName:   lowerFirst(fieldName),
			Fallible:  fallible,
			Alias:     pkg,
			// webhookServices is keyed by the SYNTHESIZED package name
			// (discoverWebhookServices uses naming.ServicePackage on the
			// forge.yaml spelling) — keep the lookup keyed the same way
			// regardless of what the on-disk package clause turned out to be.
			HasWebhooks:      webhookServices[naming.ServicePackage(svc.Name)],
			ConnectPkg:       connectPkg,
			ProtoServiceName: protoServiceName,
		})
	}

	// Resolve cross-role import-alias collisions (e.g. service "billing"
	// vs internal package "billing"). When unique, Alias == Package and
	// the bootstrap import line + symbol references emit unchanged.
	AssignBootstrapAliases(bootstrapSvcs, packages, workers, operators)

	// Probe each component's Deps struct so the bootstrap template can
	// emit only the Logger / Config fields that actually exist, and
	// auto-wire AppExtras-name-and-type-matching fields. Without this,
	// the template's hardcoded `Logger: ..., Config: cfg` lines force
	// every internal package to declare those fields even when the
	// package doesn't read them (the "Deps shape coupling" friction from
	// the control-plane migration), and the audit-no-op silent-drop fires
	// when audit.Deps.Repo and AppExtras.Repo both exist but bootstrap
	// emits only Logger.
	inspectComponentDepsShape(packages, projectDir, "internal")
	inspectComponentDepsShape(workers, projectDir, "workers")
	inspectComponentDepsShape(operators, projectDir, "operators")

	hasFallible := hasFallibleConstructor(bootstrapSvcs, packages, workers, operators)

	if configFields == nil {
		configFields = DefaultConfigFieldNames()
	}

	binaryShared := projectBinaryShared(projectDir)

	// The guard inventory defaults to the row services when the caller
	// didn't supply the full inventory (initial scaffold, unit tests):
	// every row service is part of the project's service inventory.
	allServiceNames := features.AllServiceNames
	if allServiceNames == nil {
		for _, s := range bootstrapSvcs {
			allServiceNames = append(allServiceNames, s.Name)
		}
	}

	data := struct {
		Module              string
		Services            []BootstrapServiceData
		Packages            []BootstrapPackageData
		Workers             []BootstrapWorkerData
		Operators           []BootstrapOperatorData
		HasDatabase         bool
		DatabaseDriver      string
		OrmEnabled          bool
		HasFallible         bool
		BinaryShared        bool
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
		LeaderElectionID    string
		AllServiceNames     []string
	}{
		Module:              modulePath,
		LeaderElectionID:    leaderElectionID(modulePath),
		Services:            bootstrapSvcs,
		Packages:            packages,
		Workers:             workers,
		Operators:           operators,
		HasDatabase:         hasDatabase,
		DatabaseDriver:      databaseDriver,
		OrmEnabled:          ormEnabled,
		HasFallible:         hasFallible,
		BinaryShared:        binaryShared,
		ConfigFields:        configFields,
		RESTEnabled:         restEnabled,
		ConnectImports:      connectImports,
		DiagnosticsEnabled:  features.DiagnosticsEnabled,
		StrictWiringEnabled: features.StrictWiringEnabled,
		AllServiceNames:     allServiceNames,
	}

	content, err := templates.ProjectTemplates().Render("bootstrap.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "bootstrap.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/bootstrap.go: %w", err)
	}

	// pkg/app/services_gen.go — the per-service row constructors
	// (serviceRow<X>). Tier-1, regenerated every run, emitted even with
	// zero row services (header-only file) so a project whose last
	// service goes types-only can't be left with a stale constructor
	// referencing a deleted handlers/ dir.
	rowsContent, err := templates.ProjectTemplates().Render("services_gen.go.tmpl", struct {
		Module         string
		Services       []BootstrapServiceData
		RESTEnabled    bool
		ConnectImports []string
	}{
		Module:         modulePath,
		Services:       bootstrapSvcs,
		RESTEnabled:    restEnabled,
		ConnectImports: connectImports,
	})
	if err != nil {
		return fmt.Errorf("render services_gen.go.tmpl: %w", err)
	}
	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "services_gen.go"), rowsContent, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/services_gen.go: %w", err)
	}

	// pkg/app/services.go — the user-owned registration list. Scaffolded
	// ONCE (never overwritten, same rule as setup.go): when missing, it
	// lists every current row service, which preserves the pre-existing
	// "declared ⇒ served" behavior with zero semantic change. From then
	// on the user (or their agent) owns the list — deleting a row is the
	// types-only opt-out.
	if err := GenerateServicesRegistry(modulePath, bootstrapSvcs, projectDir); err != nil {
		return err
	}
	return nil
}

// BootstrapTestServiceData holds data for a single service in the bootstrap testing template.
//
// ProtoConnectImportPath and ProtoConnectPkg are derived from the proto file's
// declared `go_package` rather than from the service name. This makes the
// generated testing.go correct even when the proto file's go_package doesn't
// follow the convention `<module>/gen/services/<svc>/v1/<svc>v1` (for example
// when multiple proto services live in a single proto file and share one go
// package, or when the package name has a custom alias).
type BootstrapTestServiceData struct {
	Name    string // e.g. "api"
	Package string // e.g. "api" (Go package CLAUSE — may differ from the dir name)
	// ImportPath is the handler directory leaf under handlers/ as it exists
	// ON DISK (e.g. "engine_shadow"), resolved by ResolveServiceComponent.
	// The testing.go template builds its import line from this, never from
	// Package — a dir may legally declare a package name that differs from
	// its directory name, and only the directory name belongs in the path.
	ImportPath             string
	FieldName              string // e.g. "API" (exported struct field)
	ProtoServiceName       string // e.g. "ApiService" (proto service name for connect client)
	ProtoConnectImportPath string // e.g. "github.com/foo/bar/gen/services/api/v1/apiv1connect"
	ProtoConnectPkg        string // e.g. "apiv1connect" (Go identifier used at call sites)
	Fallible               bool   // true if New() returns (T, error)
	HasDB                  bool   // true if Deps struct has a DB orm.Context field
	// Alias mirrors BootstrapComponentData.Alias — when an internal package
	// shares its leaf-name with this service's package, both get role-prefixed
	// aliases ("svcBilling" vs "pkgBilling") so the generated testing.go imports
	// don't collide.
	Alias string
	// VarName is the lowerCamel form of FieldName, used as the testConfig
	// field name (e.g. `c.billingDeps`). Defaults to lowerFirst(Package);
	// becomes "svcBilling" when there's a cross-role collision so the
	// services-range and packages-range testConfig fields stay distinct.
	VarName string
	// AutoStubs lists the per-Deps-field synthesized interface stubs the
	// template should emit and inject as defaults inside NewTest<Svc>.
	// Each entry corresponds to a Deps field whose Go type is an interface
	// declared locally in the handler package (so the testing.go file can
	// reference the interface via the imported handler package alias).
	// Optional-dep fields are excluded — those stay nil to preserve the
	// "graceful dev-mode degrade" semantics. See ParseLocalInterfaces +
	// HasOptionalDepMarker for the detection rules.
	AutoStubs []DepsAutoStub
	// UnresolvedStubs lists Deps fields whose type is a cross-package
	// selector forge couldn't resolve (alias not in imports, package
	// can't load, type isn't an interface). The template emits a
	// `// TODO: stub <type>` line next to NewTest<Svc> so the user
	// sees a visible reminder to hand-roll an override via
	// With<Svc>Deps(...). Empty when every selector resolved cleanly.
	UnresolvedStubs []UnresolvedAutoStub
}

// UnresolvedAutoStub is one Deps field whose cross-package selector
// type couldn't be turned into a synthesized stub. Surfaces in the
// generated testing.go as a TODO comment so the user knows to
// hand-roll an override.
type UnresolvedAutoStub struct {
	// FieldName is the Deps field name as declared.
	FieldName string
	// TypeExpr is the unresolved type expression as written in the
	// Deps struct (e.g. "external.Client").
	TypeExpr string
}

// DepsAutoStub describes one synthesized interface implementation
// emitted into the generated pkg/app/testing.go for a service-owned
// Deps field. The stub satisfies the field's interface with zero-value
// returns; it exists so NewTest<Svc>(t) can construct the Service even
// when the field is required by validateDeps. Tests that exercise real
// behavior continue to override via With<Svc>Deps(...).
type DepsAutoStub struct {
	// FieldName is the Deps field name as declared (e.g. "Repo").
	FieldName string
	// StubType is the unqualified Go identifier the template should use
	// for the synthesized stub struct (e.g. "stubApiRepo"). Generated
	// from the service alias + field name so two services with the same
	// Deps-field name don't collide at the package level.
	StubType string
	// InterfaceQualified is the package-qualified type expression used
	// when injecting the stub into Deps in NewTest<Svc>.
	//
	// For locally-declared interfaces this carries the literal "<alias>."
	// placeholder so the caller can substitute the post-collision
	// service alias ("svcBilling" vs "billing"). For cross-package
	// interfaces (CrossPackage = true) the prefix is already the
	// declaring package's alias (e.g. "repo.Repository") and must NOT
	// be re-aliased — the service alias is irrelevant to it.
	InterfaceQualified string
	// Methods are the interface's flattened method set rendered for
	// the template's stub-emit loop.
	Methods []InterfaceMethod
	// CrossPackage flags stubs whose interface lives in a package
	// other than the handler's. The caller uses this to skip the
	// "<alias>." rewrite and to fold the stub's ExtraImports into
	// the file's import block.
	CrossPackage bool
	// ExtraImports lists every package the stub's method signatures
	// reference (including the interface's own package). Only populated
	// when CrossPackage = true. The bootstrap_testing assembler
	// deduplicates these across stubs into the top-level
	// ExtraImports field on the template data.
	ExtraImports []ExtraImport
}

// GenerateBootstrapTesting generates pkg/app/testing.go from the bootstrap_testing.go.tmpl template.
//
// cs is the project's checksum tracker — passing it keeps pkg/app/testing.go
// recorded so `forge audit` doesn't flag stale state on it. A nil cs is tolerated.
func GenerateBootstrapTesting(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, multiTenantEnabled bool, projectDir string, cs *checksums.FileChecksums) error {
	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	var testSvcs []BootstrapTestServiceData
	anyServiceHasDB := false
	for _, svc := range services {
		// Disk-first: same handler-dir + package-clause resolution as
		// GenerateBootstrap so testing.go's import lines / aliases can
		// never disagree with bootstrap.go's (see disk_resolver.go).
		res, resErr := ResolveServiceComponent(projectDir, svc.Name)
		if resErr != nil {
			return resErr
		}
		pkg := res.PackageName
		handlerDir := res.Dir
		fallible, _ := DetectFallibleConstructor(handlerDir)
		hasDB, _ := DetectDepsDBField(handlerDir)
		if hasDB {
			anyServiceHasDB = true
		}
		// Auto-stub: walk the service's Deps fields, find any interface-
		// typed required fields and queue a synthesized stub. Handles
		// both locally-declared interfaces AND cross-package selector
		// types (e.g. repo.Repository) — the selector path loads the
		// imported package via go/types and walks its method set. The
		// bare-Deps trio (Logger / Config / Authorizer) and the DB
		// orm.Context field are handled by the existing default fill
		// so they're excluded from auto-stubbing here.
		//
		// unresolvedStubs surfaces selector types we couldn't resolve
		// (package failed to load, alias not in imports, type isn't
		// an interface) so the template can emit a TODO comment.
		autoStubs, unresolvedStubs := computeAutoStubs(handlerDir, pkg)
		// Derive Connect package path/name from the proto's declared
		// go_package + PkgName instead of guessing from the service name.
		// This is what lets a service whose proto moved (e.g. from
		// services/daemon_token/v1 to reliant/v1) regenerate testing.go
		// with the correct *v1connect import — the convention path
		// would still point at the old gen/services/<svc>/v1 location.
		//
		// Falls back to the convention path only when BOTH descriptor
		// fields are empty (synthetic test fixtures, pre-descriptor
		// scaffolds). Falling back on GoPackage alone left connectPkg as
		// the literal "connect" when PkgName happened to be empty.
		connectPkg := svc.PkgName + "connect"
		connectImport := svc.GoPackage + "/" + connectPkg
		if svc.GoPackage == "" || svc.PkgName == "" {
			// Synthesize from svc.Name (NOT the disk-resolved pkg): the
			// gen/ path mirrors the proto layout, not the handler dir's
			// possibly-legacy package clause.
			synth := naming.ServicePackage(svc.Name)
			connectImport = modulePath + "/gen/services/" + synth + "/v1/" + synth + "v1connect"
			connectPkg = synth + "v1connect"
		}
		testSvcs = append(testSvcs, BootstrapTestServiceData{
			Name:                   pkg,
			Package:                pkg,
			ImportPath:             res.ImportLeaf,
			FieldName:              naming.ToPascalCase(pkg),
			ProtoServiceName:       svc.Name,
			ProtoConnectImportPath: connectImport,
			ProtoConnectPkg:        connectPkg,
			Fallible:               fallible,
			HasDB:                  hasDB,
			Alias:                  pkg, // overwritten below if there's a cross-role collision
			VarName:                pkg, // overwritten below if there's a cross-role collision
			AutoStubs:              autoStubs,
			UnresolvedStubs:        unresolvedStubs,
		})
	}

	// Resolve cross-role import-alias collisions. Build a count over the
	// services + packages namespace (workers/operators don't appear in
	// testing.go imports, but we still pass them so the alias derivation
	// matches bootstrap.go's exactly). When colliding, also rewrite
	// FieldName so the generated `NewTest<FieldName>` and `With<FieldName>Deps`
	// helpers don't collide on the same Go identifier.
	pkgCount := map[string]int{}
	for _, s := range testSvcs {
		pkgCount[s.Package]++
	}
	for _, p := range packages {
		pkgCount[p.Package]++
	}
	for i := range testSvcs {
		if pkgCount[testSvcs[i].Package] > 1 {
			testSvcs[i].Alias = "svc" + upperFirst(testSvcs[i].Package)
			testSvcs[i].FieldName = "Svc" + upperFirst(testSvcs[i].Package)
			testSvcs[i].VarName = lowerFirst(testSvcs[i].FieldName)
		}
		// Rewrite the AutoStubs' qualified interface refs to use the
		// post-collision alias. The unqualified stub-type identifier
		// already carries an UpperCamel(Package) prefix so collisions
		// across services are impossible by construction.
		//
		// CrossPackage stubs skip this rewrite — their interface lives
		// in a different package whose alias has nothing to do with
		// this service's alias. Their InterfaceQualified is already
		// the resolved "<pkg>.<TypeName>" form from
		// ResolveCrossPkgInterface.
		alias := testSvcs[i].Alias
		for j, stub := range testSvcs[i].AutoStubs {
			if stub.CrossPackage {
				continue
			}
			// Replace the placeholder "<alias>." prefix with the resolved
			// alias. computeAutoStubs writes "<alias>." literally so the
			// rewrite is a single string-replace per field.
			testSvcs[i].AutoStubs[j].InterfaceQualified = strings.Replace(stub.InterfaceQualified,
				"<alias>.", alias+".", 1)
		}
	}
	// Apply the same collision rule to packages so the testing.go import
	// alias for a colliding internal package matches the bootstrap.go alias.
	// Take a defensive copy so we don't mutate the caller's slice.
	pkgsCopy := append([]BootstrapPackageData(nil), packages...)
	for i := range pkgsCopy {
		if pkgCount[pkgsCopy[i].Package] > 1 {
			pkgsCopy[i].Alias = "pkg" + upperFirst(pkgsCopy[i].Package)
			pkgsCopy[i].FieldName = "Pkg" + upperFirst(pkgsCopy[i].Package)
			pkgsCopy[i].VarName = lowerFirst(pkgsCopy[i].FieldName)
		} else if pkgsCopy[i].Alias == "" {
			pkgsCopy[i].Alias = pkgsCopy[i].Package
		}
	}
	// Probe each package's Deps AST so testing.go emits the same Deps
	// shape as bootstrap.go. Without this call, removing Logger from a
	// package's Deps regenerates bootstrap.go cleanly but breaks
	// testing.go (the v2 migration of control-plane reproduced this).
	inspectComponentDepsShape(pkgsCopy, projectDir, "internal")
	packages = pkgsCopy

	// Dedupe Connect package imports: when multiple proto services share one
	// proto file (and thus one go_package), they share one connect import.
	// Without dedupe the generated testing.go would contain duplicate imports
	// and fail to compile.
	connectImportSet := make(map[string]struct{}, len(testSvcs))
	var connectImports []string
	for _, s := range testSvcs {
		if _, seen := connectImportSet[s.ProtoConnectImportPath]; seen {
			continue
		}
		connectImportSet[s.ProtoConnectImportPath] = struct{}{}
		connectImports = append(connectImports, s.ProtoConnectImportPath)
	}

	// Collect the union of every cross-package import any auto-stub
	// needs. The bootstrap_testing template emits these in a dedicated
	// import block so the rendered file can reference cross-package
	// interface types and their method-signature dependencies without
	// the user wiring up the import by hand. Deterministic ordering is
	// preserved by SortedNeededImports — the union is then re-sorted
	// here so the final file stays diff-stable.
	extraImports := mergeExtraImports(testSvcs)

	data := struct {
		Module             string
		Services           []BootstrapTestServiceData
		ConnectImports     []string
		Packages           []BootstrapPackageData
		MultiTenantEnabled bool
		AnyServiceHasDB    bool
		HasMigrationsFS    bool
		ExtraImports       []ExtraImport
	}{
		Module:             modulePath,
		Services:           testSvcs,
		ConnectImports:     connectImports,
		Packages:           packages,
		MultiTenantEnabled: multiTenantEnabled,
		AnyServiceHasDB:    anyServiceHasDB,
		// Same predicate GenerateMigrate uses for db/embed.go: when the
		// project carries SQL migrations, emit the opt-in NewMigratedTestDB
		// helper (testkit.NewMigratedPostgresDB over forgedb.MigrationsFS)
		// so generated CRUD/handler tests can start with the real schema
		// instead of the bare default database.
		HasMigrationsFS:    projectHasSQLMigrations(projectDir),
		ExtraImports:       nil, // filled below after duplicate-filtering
	}

	// Filter ExtraImports against everything the template ALREADY
	// imports. Stub method signatures routinely reference packages the
	// static import block carries unconditionally (`context` is the
	// canonical case: the template emits `"context"` whenever there are
	// services, and any stub method taking ctx adds `context "context"`
	// to ExtraImports — two declarations of the same name, which fails
	// `go build`). Rather than hand-mirroring the template's conditional
	// import logic here (and drifting the moment the template changes),
	// render a baseline WITHOUT extras, parse its import block, and drop
	// every extra whose declared name + path are already present. An
	// extra whose alias collides with a different path is kept as-is:
	// that's a genuine user-level package-name collision the compile
	// error should surface, not something to silently rewrite.
	if len(extraImports) > 0 {
		baseline, berr := templates.ProjectTemplates().Render("bootstrap_testing.go.tmpl", data)
		if berr == nil {
			extraImports = filterAlreadyImported(extraImports, baseline)
		}
	}
	data.ExtraImports = extraImports

	content, err := templates.ProjectTemplates().Render("bootstrap_testing.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render bootstrap_testing.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "testing.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/testing.go: %w", err)
	}
	return nil
}

// projectHasSQLMigrations reports whether db/migrations/ contains at
// least one .sql file — the same predicate the CLI uses to decide whether
// GenerateMigrate embeds db/embed.go's MigrationsFS. Keeping the two in
// agreement matters: bootstrap_testing.go.tmpl only imports forgedb when
// this is true, and forgedb.MigrationsFS only exists when GenerateMigrate
// saw migrations.
func projectHasSQLMigrations(projectDir string) bool {
	entries, err := os.ReadDir(filepath.Join(projectDir, "db", "migrations"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			return true
		}
	}
	return false
}

// mergeExtraImports folds every cross-package stub's ExtraImports
// into one deterministic deduplicated slice. Path is the dedupe key —
// two stubs that both depend on "x/y/z" produce a single import line.
// Conflict on the alias (same path with different aliases across two
// stubs) is resolved first-wins, matching the order computeAutoStubs
// returns and the import-collection inside ResolveCrossPkgInterface
// (which uses the imported package's declared name, so a conflict
// would already be a real package-rename collision the user would
// have to resolve regardless).
func mergeExtraImports(services []BootstrapTestServiceData) []ExtraImport {
	seen := map[string]string{}
	for _, s := range services {
		for _, stub := range s.AutoStubs {
			if !stub.CrossPackage {
				continue
			}
			for _, imp := range stub.ExtraImports {
				if _, ok := seen[imp.Path]; ok {
					continue
				}
				seen[imp.Path] = imp.Alias
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]ExtraImport, 0, len(seen))
	for path, alias := range seen {
		out = append(out, ExtraImport{Path: path, Alias: alias})
	}
	// Deterministic order — codegen must be reproducible for the
	// checksums-based "no spurious diff" guarantee. SortedNeededImports
	// sorts by Path; we do the same here so the merged slice stays
	// in lockstep with the per-stub view.
	sortExtraImports(out)
	return out
}

// filterAlreadyImported drops every ExtraImport whose declared name AND
// path already appear in src's import block (src is a rendered Go file —
// the bootstrap_testing baseline rendered without extras). Matching on
// the (name, path) pair rather than path alone is deliberate: Go permits
// the same path under two different aliases (the cross-role collision
// case where a package import is aliased `pkgLedger` while a stub
// references the package's declared name), so a path-only filter would
// remove an import the stub's method signatures still need. When src
// doesn't parse, the extras are returned unchanged — the subsequent
// `go build` validation step owns the failure.
func filterAlreadyImported(extras []ExtraImport, src interface{}) []ExtraImport {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "testing.go", src, parser.ImportsOnly|parser.SkipObjectResolution)
	if err != nil {
		return extras
	}
	declared := map[string]string{}
	for _, imp := range f.Imports {
		path, perr := importPathFromSpec(imp)
		if perr != nil {
			continue
		}
		name := aliasForImport(imp, path)
		if _, ok := declared[name]; !ok {
			declared[name] = path
		}
	}
	var out []ExtraImport
	for _, e := range extras {
		if p, ok := declared[e.Alias]; ok && p == e.Path {
			continue
		}
		out = append(out, e)
	}
	return out
}

// sortExtraImports puts an ExtraImport slice in canonical Path order.
// Factored out of mergeExtraImports so the tests can reuse it on
// hand-built slices without re-running the merge.
func sortExtraImports(s []ExtraImport) {
	// import "sort" once; the codegen package already imports it.
	if len(s) < 2 {
		return
	}
	// Trivial insertion sort: codegen runs are dominated by I/O and
	// template execution, the import list is typically <10 elements,
	// and we want to avoid pulling another import for this one call.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].Path > s[j].Path; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// bareDepsFieldNames is the set of Deps field names the bootstrap_testing
// template fills in by default — these never need an auto-stub. Keep
// the list narrow: anything else (Repo, Audit, Stripe, Email, ...)
// flows through the auto-stub path when its type is a local interface.
var bareDepsFieldNames = map[string]bool{
	"Logger":     true,
	"Config":     true,
	"Authorizer": true,
	"DB":         true,
}

// computeAutoStubs walks a service's handler directory, parses its
// Deps struct + locally-declared interfaces, and returns one
// DepsAutoStub for every required Deps field whose type is an
// interface forge can satisfy with a zero-value stub. The bare-Deps
// trio (Logger / Config / Authorizer) and the optional-dep set are
// excluded — those are either filled in by the template's existing
// default-merge step or deliberately left nil.
//
// Two type shapes are handled:
//
//  1. Bare-identifier types ("Repository") that resolve to an
//     interface declared in the handler package. InterfaceQualified
//     is "<alias>." + name so the caller can substitute the
//     post-collision service alias.
//
//  2. Selector types ("repo.Repository") that resolve to an
//     interface in an imported package. ResolveCrossPkgInterface
//     does the heavy lifting (alias → import path → go/packages
//     load → types.Interface walk). On success, the stub carries
//     CrossPackage = true and ExtraImports listing every package
//     its method signatures reference.
//
// Unresolvable selector types (alias mismatch, package can't load,
// type isn't an interface) fall through to the existing
// "field stays nil" behavior. This is the soft-fail design: tests
// that hit a nil dependency see the usual nil-deref, the user
// either overrides the field via With<Svc>Deps or hand-rolls a
// stub — both are existing escape valves.
func computeAutoStubs(handlerDir, _ string) ([]DepsAutoStub, []UnresolvedAutoStub) {
	fields, err := ParseServiceDeps(handlerDir)
	if err != nil || len(fields) == 0 {
		return nil, nil
	}
	locals, _ := ParseLocalInterfaces(handlerDir)
	var stubs []DepsAutoStub
	var unresolved []UnresolvedAutoStub
	for _, f := range fields {
		if bareDepsFieldNames[f.Name] {
			continue
		}
		if f.Optional {
			continue
		}
		t := strings.TrimSpace(f.Type)

		// (1) Bare-identifier interface declared in this package.
		if iface, ok := locals[t]; ok {
			stubs = append(stubs, DepsAutoStub{
				FieldName:          f.Name,
				StubType:           "", // resolved below from handlerDir's package
				InterfaceQualified: "<alias>." + iface.Name,
				Methods:            iface.Methods,
			})
			continue
		}

		// (2) Selector type — resolve across the import boundary.
		// We only handle the simple `<pkg>.<TypeName>` shape; pointer
		// (`*pkg.T`), slice, map, and chan decorations on an interface
		// are not idiomatic and stay on the hand-roll path.
		if dot := strings.IndexByte(t, '.'); dot > 0 && !strings.ContainsAny(t, "*[]<>(){}") {
			pkgAlias := t[:dot]
			typeName := t[dot+1:]
			res, ok := ResolveCrossPkgInterface(handlerDir, pkgAlias, typeName)
			if !ok {
				// Soft-fail: record the selector so the template can
				// surface a TODO comment. The field still stays nil
				// at construction time — the comment exists purely to
				// nudge the user toward With<Svc>Deps overrides or
				// a hand-rolled stub.
				unresolved = append(unresolved, UnresolvedAutoStub{
					FieldName: f.Name,
					TypeExpr:  t,
				})
				continue
			}
			stubs = append(stubs, DepsAutoStub{
				FieldName:          f.Name,
				StubType:           "", // resolved below
				InterfaceQualified: res.PackageName + "." + typeName,
				Methods:            res.Methods,
				CrossPackage:       true,
				ExtraImports:       SortedNeededImports(res.NeededImports),
			})
		}
	}
	// Make stub-type names predictable + collision-free across the
	// file: stub<UpperPackage><FieldName>. handlerDir's last segment
	// IS the service Go-package name (naming.ServicePackage), so use it
	// directly. The CrossPackage flag does not affect the stub-type
	// identifier — it lives in pkg/app, not in the imported package,
	// so the service-name prefix still gives us per-service uniqueness.
	pkg := filepath.Base(handlerDir)
	for i := range stubs {
		stubs[i].StubType = "stub" + upperFirst(pkg) + stubs[i].FieldName
	}
	return stubs, unresolved
}

// hasFallibleConstructor returns true if any service, package, worker, operator, or function has a fallible constructor.
func hasFallibleConstructor(services []BootstrapServiceData, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData) bool {
	for _, s := range services {
		if s.Fallible {
			return true
		}
	}
	for _, p := range packages {
		if p.Fallible {
			return true
		}
	}
	for _, w := range workers {
		if w.Fallible {
			return true
		}
	}
	for _, o := range operators {
		if o.Fallible {
			return true
		}
	}
	return false
}
