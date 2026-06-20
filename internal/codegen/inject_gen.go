// File: internal/codegen/inject_gen.go
//
// The GENERATED injector — the keep-in-sync half of the hybrid DI model
// (FORGE_SHAPE_REDESIGN §2, the Google-Wire shape). GenerateInject emits
// internal/app/inject_gen.go: a forge-owned, regenerated-every-run
// `Build(infra *Infra) (*Services, error)` that constructs every
// registered component (service / worker / operator / internal package)
// in TYPE-topological order and calls each New(Deps) resolving every Deps
// field BY TYPE.
//
// This REPLACES the name-matching wire_gen resolution: where wire_gen
// matched a Deps field to `app.<FieldName>` by exact field name, Build
// matches a Deps field to a producer component or an Infra field by TYPE
// (using the same build_topo core + DepsAssignabilityMatcher the additive
// wave landed). The KEY distinction from the prior aborted pass: this
// injector is GENERATED, not scaffold-once — adding/removing a component
// is a regenerate, never a hand-edit.
//
// Resolution per Deps field, in priority order:
//
//  1. PRODUCER — another already-constructed component whose ServiceTypeKey
//     (package-qualified `<pkg>.Service`, pointer-tolerant) matches the
//     field's declared type. Drawn as a build_topo edge; emitted as the
//     producer's local var.
//  2. INFRA FIELD — a field on the owned *Infra struct (providers.go)
//     assignable to the Deps field type, proven at GENERATE time by the
//     DepsAssignabilityMatcher loaded over (internal/app, component) in
//     one packages.Load universe. A concrete *db.PostgresRepository on
//     Infra fills a narrow audit.Repository field (closes the constraint-3
//     silent-drop). Emitted as `infra.<Field>`.
//  3. CONVENTIONAL — Logger -> infra.Log, Config -> infra.Cfg.
//  4. MISSING — a required (non `forge:optional-dep`), non-scalar Deps
//     field that resolves to none of the above is LOUD:
//       (a) GENERATE-TIME: collected into MissingProvider; GenerateInject
//           returns an error naming the missing TYPE + the consuming
//           component + the Deps field, when the matcher could PROVE the
//           Infra struct has no assignable field.
//       (b) COMPILE-TIME backstop: when assignability is merely UNPROVEN
//           (project mid-edit / not type-checking — matcher policy in
//           deps_assignability.go), Build still emits `infra.<Field>` so
//           the Go compiler arbitrates. It NEVER emits a silent typed-zero
//           for a required field.
//
// Scalar Deps fields (string/int/bool/duration) are CONFIGURATION, not
// collaborators — they take the typed-zero with a config-block hint,
// mirroring wire_gen, and never raise a MissingProvider error.
//
// Each ServiceTypeKey is constructed exactly once (one local var per
// BuildPlan.Order entry) — per-binary singleton by construction.

package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// InjectComponentData is one component's rendered inputs for the Build
// injector body: the import line, the constructor selector, and the
// ordered Deps-literal assignments resolved by type.
type InjectComponentData struct {
	// FieldName is the exported field on *Services (e.g. "Billing",
	// "SvcBilling") — shared with the inventory and bootstrap naming.
	FieldName string
	// VarName is the lower-camel base name (e.g. "billing"); used for the
	// per-component authz var (`<VarName>Authz`).
	VarName string
	// LocalVar is the local variable Build binds the constructed instance
	// to — VarName + "Inst" so it never shadows the package import alias.
	LocalVar string
	// Alias is the import alias for the component's package.
	Alias string
	// ImportPath is the module-relative import path (e.g.
	// "internal/handlers/billing").
	ImportPath string
	// Package is the Go package clause (for the constructor selector and
	// doc comments).
	Package string
	// Fallible reports whether New returns (T, error). Build wraps the
	// error with the component name when true; assigns directly otherwise.
	Fallible bool
	// NeedsAuthzVar is true when the Deps struct declares an Authorizer
	// field. Build emits a `var authz` block with the dev-bypass swap.
	NeedsAuthzVar bool
	// Assignments are the per-Deps-field key/value pairs, in Deps
	// declaration order, for the New(Deps{...}) literal.
	Assignments []InjectAssignment
}

// InjectAssignment is one `Field: Expr,` line in a New(Deps{...}) literal.
type InjectAssignment struct {
	Field   string
	Expr    string
	Comment string
}

// MissingProvider records a required Deps field that resolved to no
// producer and no PROVEN-assignable Infra field. GenerateInject returns
// an error built from these (see the file header's two-tier loudness).
type MissingProvider struct {
	// Component is the consuming component's FieldName.
	Component string
	// Field is the Deps field name with no provider.
	Field string
	// Type is the declared field type with no provider.
	Type string
}

// InjectGenData is the rendered template input for inject_gen.go.tmpl.
type InjectGenData struct {
	Module            string
	NeedsAuthorizer   bool
	NeedsConfigImport bool
	// NeedsFmt gates the `fmt` import: it is only referenced in the fallible
	// (New returns error) construction branch, so a project with no fallible
	// component (incl. the zero-component case) must not import it or the
	// generated file fails to compile on an unused import.
	NeedsFmt bool
	// Order is the topo-sorted construction sequence (producers first).
	Order []InjectComponentData
	// HasCycle / CycleEdges drive the two-phase setter stub block.
	HasCycle   bool
	CycleEdges []BuildEdge
}

// GenerateInject emits internal/app/inject_gen.go. Returns nil with no
// file written when there are no components. Returns an error listing
// every MissingProvider when a required collaborator field resolves to no
// producer and the matcher PROVES the Infra struct has no assignable
// field.
//
// This is ADDITIVE in PASS 1: it writes alongside the existing
// pkg/app/wire_gen.go without retiring it. cmd-server adopts Build behind
// the feature gate; wire_gen + appkit.Run stay present but unused so the
// tree always builds. PASS 3 retires wire_gen once Build is proven.
func GenerateInject(in InjectGenInput) error {
	comps, err := assembleBuildComponents(in)
	if err != nil {
		return err
	}
	// No len(comps)==0 early-return: cmd/server.go imports internal/app
	// unconditionally, so the package must exist and compile even with zero
	// components. The template renders a valid empty Build over an empty
	// Services in that case.

	appDir := filepath.Join(in.ProjectDir, "internal", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}

	// Emit the typed Services registry struct (one field per component,
	// typed as its Service interface) so inject_gen + inventory_gen + the
	// owned providers/post_build all compile against the same registry.
	if err := generateInjectServices(comps, in.ModulePath, in.ProjectDir, in.Checksums); err != nil {
		return err
	}

	resolver := NewServiceKeyResolver(comps)
	plan := ComputeBuildPlan(comps, resolver)

	// Producer lookup: FieldName -> the producer's local var NAME in Build.
	// The local var is suffixed "Inst" so it never shadows the component's
	// package import alias (var `item` would shadow import `item`), which
	// would make a later `item.X` reference in the same function refer to
	// the value, not the package.
	producerVar := map[string]string{}
	for _, c := range comps {
		producerVar[c.FieldName] = injectVarName(c.VarName)
	}

	// The Infra struct's exported fields — the owned provider set. Parsed
	// from internal/app/providers.go (+ any sibling .go in internal/app
	// that declares `type Infra struct`). Empty when providers.go hasn't
	// been scaffolded yet (first generate) — every collaborator then falls
	// to the compile-time backstop, which is the correct loud state.
	infraFields, err := parseInfraFields(appDir)
	if err != nil {
		return fmt.Errorf("parse internal/app for Infra fields: %w", err)
	}

	matcher := NewInfraAssignabilityMatcher(in.ProjectDir)

	var (
		rendered        []InjectComponentData
		missing         []MissingProvider
		needsAuthorizer bool
		needsConfig     bool
		needsFmt        bool
	)

	for _, c := range plan.Order {
		rc := InjectComponentData{
			FieldName:  c.FieldName,
			VarName:    c.VarName,
			LocalVar:   injectVarName(c.VarName),
			Alias:      c.Alias,
			ImportPath: c.ImportPath,
			Package:    c.compPackage,
			Fallible:   c.compFallible,
		}
		if c.compFallible {
			needsFmt = true
		}
		for _, df := range c.Deps {
			needsConfig = needsConfig || df.Name == "Config"
			if df.Name == "Authorizer" {
				rc.NeedsAuthzVar = true
				needsAuthorizer = true
			}
			expr, comment, miss := resolveInjectField(df, c, producerVar, resolver, infraFields, matcher, in.RoleRoot(c))
			if miss != nil {
				missing = append(missing, *miss)
			}
			rc.Assignments = append(rc.Assignments, InjectAssignment{
				Field:   df.Name,
				Expr:    expr,
				Comment: comment,
			})
		}
		rendered = append(rendered, rc)
	}

	if len(missing) > 0 {
		return missingProviderError(missing)
	}

	data := InjectGenData{
		Module:            in.ModulePath,
		NeedsAuthorizer:   needsAuthorizer,
		NeedsConfigImport: needsConfig,
		NeedsFmt:          needsFmt,
		Order:             rendered,
		HasCycle:          plan.HasCycle(),
		CycleEdges:        plan.CycleEdges,
	}

	content, err := templates.ProjectTemplates().Render("inject_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render inject_gen.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(in.ProjectDir, filepath.Join("internal", "app", "inject_gen.go"), content, in.Checksums); err != nil {
		return fmt.Errorf("write internal/app/inject_gen.go: %w", err)
	}
	return nil
}

// resolveInjectField resolves one Deps field to the Go expression Build
// should emit, following the priority order in the file header. The third
// return is non-nil when the field is a required collaborator with a
// PROVEN-missing provider (generate-time loud error).
func resolveInjectField(df DepsField, c BuildComponent, producerVar map[string]string, resolver TypeResolver, infraFields map[string]InfraField, matcher *InfraAssignabilityMatcher, roleRoot string) (expr, comment string, miss *MissingProvider) {
	// 1. PRODUCER — another component produces this type (by-type edge).
	if prodField := resolver.Resolve(df.Type); prodField != "" && prodField != c.FieldName {
		if v, ok := producerVar[prodField]; ok {
			return v, "in-process " + df.Type, nil
		}
	}

	// 3. CONVENTIONAL — Logger / Config from Infra. (Checked before the
	// generic Infra-field path so the canonical sources stay stable
	// regardless of whether the user renamed the Infra Log/Cfg fields.)
	switch df.Name {
	case "Logger":
		return "infra.Log", "", nil
	case "Config":
		return "infra.Cfg", "", nil
	case "Authorizer":
		// Filled by the per-component `var <varname>Authz` block the
		// template emits (dev-bypass swap handled there).
		return c.VarName + "Authz", "dev-bypass swap in development", nil
	}

	// 2. INFRA FIELD — an Infra field assignable to df.Type. Prefer a
	// proven assignable field; fall back to an exact-name Infra field as
	// the compile-time backstop.
	if field, kind := matcher.ResolveInfraField(roleRoot, c.compImportLeaf, df.Name, df.Type, infraFields); field != "" {
		switch kind {
		case MatchAssignable, MatchExactString:
			return "infra." + field, "", nil
		case MatchUnavailable:
			// Compile-time backstop: emit infra.<Field> and let the Go
			// compiler arbitrate (never a silent typed-zero for a required
			// field). Same deterministic fail-loud policy as wire_gen.
			return "infra." + field, "compile-time backstop (assignability unproven)", nil
		}
	}

	// Scalar fields are configuration, not collaborators — typed-zero with
	// the config-block hint; never a MissingProvider.
	if zeroValueLiteral(df.Type) != "nil" {
		return zeroValueLiteral(df.Type), scalarConfigHint(df, c), nil
	}

	// Optional collaborator with no provider: typed nil, silent (the user
	// opted into "may be nil"). Required: typed nil + loud MissingProvider.
	if df.Optional {
		return "nil", "optional — no provider", nil
	}
	return "nil", "TODO: no provider for " + df.Type,
		&MissingProvider{Component: c.FieldName, Field: df.Name, Type: df.Type}
}

// scalarConfigHint mirrors wire_gen's unresolvedDepHint for the scalar
// case: a scalar Deps field is configuration and belongs in a config
// block, not the Infra provider set.
func scalarConfigHint(df DepsField, c BuildComponent) string {
	return fmt.Sprintf("TODO: %s is configuration — declare a config block (see forge architecture skill)", df.Name)
}

// missingProviderError builds the loud generate-time error naming every
// required collaborator field with no provider, with the exact remediation
// (add an assignable field to Infra in internal/app/providers.go).
func missingProviderError(missing []MissingProvider) error {
	sort.Slice(missing, func(i, j int) bool {
		if missing[i].Component != missing[j].Component {
			return missing[i].Component < missing[j].Component
		}
		return missing[i].Field < missing[j].Field
	})
	var b strings.Builder
	b.WriteString("the generated injector (internal/app/inject_gen.go) has Deps fields with no provider.\n\n")
	b.WriteString("Each required collaborator must resolve to either another registered component (by its Service interface type) or a field on the owned *Infra struct (internal/app/providers.go). The following could not be resolved:\n\n")
	for _, m := range missing {
		fmt.Fprintf(&b, "  - %s.Deps.%s (%s) has no provider: add a field of an assignable type to Infra in internal/app/providers.go (or its construction to OpenInfra), then re-run `forge generate`.\n", m.Component, m.Field, m.Type)
	}
	return fmt.Errorf("%s", b.String())
}

// injectVarName returns the local variable name Build uses for a
// component's constructed instance: the lower-camel base + "Inst". The
// suffix guarantees the var never shadows the component's package import
// alias (which equals the base name for single-word packages).
func injectVarName(base string) string { return base + "Inst" }

// qualifyConstructorType turns the package-local constructor return type
// (e.g. "*Service", "Service", "*Controller") into the alias-qualified form
// the internal/app package must reference (e.g. "*item.Service",
// "item.Service"). It inserts `<alias>.` before the leading type identifier,
// after any leading pointer stars. Empty ctorType (no parseable New, or a
// not-yet-scaffolded component) falls back to "*<alias>.Service" — the
// bootstrap default.
func qualifyConstructorType(ctorType, alias string) string {
	t := strings.TrimSpace(ctorType)
	if t == "" {
		return "*" + alias + ".Service"
	}
	stars := ""
	for strings.HasPrefix(t, "*") {
		stars += "*"
		t = strings.TrimSpace(t[1:])
	}
	// Already qualified (selector form pkg.Name) — leave as-is (rare:
	// a New that returns another package's type). Otherwise qualify.
	if strings.Contains(t, ".") {
		return stars + t
	}
	return stars + alias + "." + t
}

// generateInjectServices emits internal/app/app_services_gen.go: the typed
// Services struct, one field per component typed as its Service interface.
// Stable component order (FieldName) so the file is byte-stable.
func generateInjectServices(comps []BuildComponent, modulePath, projectDir string, cs *checksums.FileChecksums) error {
	type svcField struct {
		FieldName  string
		Alias      string
		ImportPath string
		FieldType  string
	}
	sorted := make([]BuildComponent, len(comps))
	copy(sorted, comps)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].FieldName < sorted[j].FieldName })
	fields := make([]svcField, 0, len(sorted))
	for _, c := range sorted {
		ft := c.compFieldType
		if ft == "" {
			ft = "*" + c.Alias + ".Service"
		}
		fields = append(fields, svcField{FieldName: c.FieldName, Alias: c.Alias, ImportPath: c.ImportPath, FieldType: ft})
	}
	data := struct {
		Module     string
		Components []svcField
	}{Module: modulePath, Components: fields}
	content, err := templates.ProjectTemplates().Render("app_services_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render app_services_gen.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(projectDir, filepath.Join("internal", "app", "app_services_gen.go"), content, cs); err != nil {
		return fmt.Errorf("write internal/app/app_services_gen.go: %w", err)
	}
	return nil
}

// GenerateProviders writes internal/app/providers.go ONCE — the owned
// Infra + OpenInfra (scaffold-once, never overwritten; same os.Stat guard
// as GenerateSetup). The injector fills component Deps from Infra fields by
// type; the user grows this file as the injector reports missing providers.
func GenerateProviders(modulePath, databaseDriver string, ormEnabled bool, projectDir string) error {
	appDir := filepath.Join(projectDir, "internal", "app")
	path := filepath.Join(appDir, "providers.go")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	data := struct {
		Module      string
		HasDatabase bool
		OrmEnabled  bool
	}{Module: modulePath, HasDatabase: databaseDriver != "", OrmEnabled: ormEnabled}
	content, err := templates.ProjectTemplates().Render("providers.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render providers.go.tmpl: %w", err)
	}
	return writeUserScaffold(path, content)
}

// GeneratePostBuild writes internal/app/post_build.go ONCE — the owned
// two-phase wiring hook (scaffold-once, no-op default). Runs after Build.
func GeneratePostBuild(projectDir string) error {
	appDir := filepath.Join(projectDir, "internal", "app")
	path := filepath.Join(appDir, "post_build.go")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return err
	}
	content, err := templates.ProjectTemplates().Render("post_build.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render post_build.go.tmpl: %w", err)
	}
	return writeUserScaffold(path, content)
}

// GenerateLifecycle emits internal/app/lifecycle_gen.go: the supervised-
// component surface (WorkerList / OperatorList / HasOperators / RunOperators)
// over the constructed *Services. Where the Mount inventory is the HTTP
// surface, this is the worker/operator surface the cmd layer packs into
// serverkit.Server. Returns nil with no file written when there are no
// components.
func GenerateLifecycle(in InjectGenInput) error {
	comps, err := assembleBuildComponents(in)
	if err != nil {
		return err
	}
	// No len(comps)==0 early-return: cmd/server.go reads app.WorkerList /
	// app.OperatorList / app.RunOperators over *Services, so lifecycle_gen.go
	// must exist even with zero supervised components (the template emits
	// valid no-op WorkerList/OperatorList/RunOperators in that case).

	type lifeComp struct {
		Name       string
		FieldName  string
		Alias      string
		ImportPath string
		FieldType  string
	}
	var workers, operators []lifeComp
	for _, c := range comps {
		lc := lifeComp{Name: c.Name, FieldName: c.FieldName, Alias: c.Alias, ImportPath: c.ImportPath, FieldType: c.compFieldType}
		switch c.compRoleRoot {
		case "internal/workers":
			workers = append(workers, lc)
		case "internal/operators":
			operators = append(operators, lc)
		}
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].FieldName < workers[j].FieldName })
	sort.Slice(operators, func(i, j int) bool { return operators[i].FieldName < operators[j].FieldName })

	data := struct {
		Module           string
		LeaderElectionID string
		Workers          []lifeComp
		Operators        []lifeComp
	}{
		Module:           in.ModulePath,
		LeaderElectionID: leaderElectionID(in.ModulePath),
		Workers:          workers,
		Operators:        operators,
	}

	content, err := templates.ProjectTemplates().Render("lifecycle_gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render lifecycle_gen.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(in.ProjectDir, filepath.Join("internal", "app", "lifecycle_gen.go"), content, in.Checksums); err != nil {
		return fmt.Errorf("write internal/app/lifecycle_gen.go: %w", err)
	}
	return nil
}

// ── BuildComponent assembly ──────────────────────────────────────────

// InjectGenInput carries everything GenerateInject needs to assemble the
// component set. Mirrors the bootstrap inputs so the two derive identical
// FieldName / alias values (one source of truth: ResolveCollisionNaming).
type InjectGenInput struct {
	GenContext
	Services  []ServiceDef
	Packages  []BootstrapPackageData
	Workers   []BootstrapWorkerData
	Operators []BootstrapOperatorData
}

// RoleRoot returns the role-root directory the assignability matcher loads
// the component's package from, keyed by the component's role. The role is
// encoded on the assembled BuildComponent (compRoleRoot).
func (in InjectGenInput) RoleRoot(c BuildComponent) string { return c.compRoleRoot }

// assembleBuildComponents parses every component's Deps + disk-resolves
// its package identity into the []BuildComponent build_topo orders. The
// ServiceTypeKey each component PRODUCES is `<pkg>.Service` (the strict
// contract-name convention — one Service per component package), pointer-
// tolerant via the resolver. Conventional leaf workers/operators with no
// Service interface get ServiceTypeKey="" (they produce no edges).
func assembleBuildComponents(in InjectGenInput) ([]BuildComponent, error) {
	// Resolve service packages from disk once (import line + package
	// clause), shared with the collision counts — exactly as wire_gen does.
	svcResolved := make([]ResolvedComponent, 0, len(in.Services))
	for _, svc := range in.Services {
		res, err := ResolveServiceComponent(in.ProjectDir, svc.Name)
		if err != nil {
			return nil, err
		}
		svcResolved = append(svcResolved, res)
	}
	svcComponents := make([]BootstrapServiceData, 0, len(in.Services))
	for _, res := range svcResolved {
		svcComponents = append(svcComponents, BootstrapServiceData{Package: res.PackageName})
	}
	counts := CollisionCounts(svcComponents, in.Packages, in.Workers, in.Operators)

	var comps []BuildComponent

	// Services.
	for i, svc := range in.Services {
		res := svcResolved[i]
		pkg := res.PackageName
		fallbackField := naming.ToPascalCase(strings.TrimSuffix(svc.Name, "Service"))
		if fallbackField == "" {
			fallbackField = naming.ToPascalCase(svc.Name)
		}
		alias, fieldName := ResolveCollisionNaming(pkg, fallbackField, "svc", counts)
		runtimeName := naming.ToKebabCase(strings.TrimSuffix(svc.Name, "Service"))
		if runtimeName == "" {
			runtimeName = naming.ToKebabCase(svc.Name)
		}
		deps, _ := ParseServiceDeps(res.Dir)
		fallible := false
		ctorType := ""
		if res.FromDisk {
			fallible, _ = DetectFallibleConstructor(res.Dir)
			ctorType, _ = DetectConstructorType(res.Dir)
		}
		comps = append(comps, BuildComponent{
			Name:           runtimeName,
			FieldName:      fieldName,
			VarName:        lowerFirst(fieldName),
			Alias:          alias,
			ImportPath:     "internal/handlers/" + res.ImportLeaf,
			ServiceTypeKey: pkg + ".Service",
			Deps:           deps,
			compPackage:    pkg,
			compFallible:   fallible,
			compRoleRoot:   "internal/handlers",
			compImportLeaf: res.ImportLeaf,
			compFieldType:  qualifyConstructorType(ctorType, alias),
		})
	}

	// Internal packages, workers, operators share the same shape.
	addRole := func(role string, role4 string, datas []BootstrapComponentData) error {
		for _, c := range datas {
			alias, fieldName := ResolveCollisionNaming(c.Package, c.FieldName, role4, counts)
			compDir := filepath.Join(in.ProjectDir, role, filepath.FromSlash(c.ImportPath))
			deps, _ := ParseServiceDeps(compDir)
			ctorType, _ := DetectConstructorType(compDir)
			comps = append(comps, BuildComponent{
				Name:           c.Name,
				FieldName:      fieldName,
				VarName:        lowerFirst(fieldName),
				Alias:          alias,
				ImportPath:     role + "/" + c.ImportPath,
				ServiceTypeKey: c.Package + ".Service",
				Deps:           deps,
				compPackage:    c.Package,
				compFallible:   c.Fallible,
				compRoleRoot:   role,
				compImportLeaf: c.ImportPath,
				compFieldType:  qualifyConstructorType(ctorType, alias),
			})
		}
		return nil
	}
	if err := addRole("internal", "pkg", in.Packages); err != nil {
		return nil, err
	}
	if err := addRole("internal/workers", "wkr", in.Workers); err != nil {
		return nil, err
	}
	if err := addRole("internal/operators", "op", in.Operators); err != nil {
		return nil, err
	}

	return comps, nil
}
