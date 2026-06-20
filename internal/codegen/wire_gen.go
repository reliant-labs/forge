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

// wire_gen.go — codegen for pkg/app/wire_gen.go.
//
// Background: forge's pre-2026-05-07 DI shape was a two-phase
// Bootstrap→Setup→ApplyDeps dance. Bootstrap constructed each service
// with the bare-Deps trio (Logger / Config / Authorizer), registered
// the service on the mux, then called user-owned Setup(). Setup wired
// rich deps (Repo, Audit, etc.) by calling ApplyDeps on each
// already-registered *Service. The mux captured the original pointer,
// so reassigning app.Services.X = X.New(...) had no runtime effect —
// hence the in-place mutation contract on ApplyDeps.
//
// The two-phase pattern broke validateDeps. Day-5 of the polish phase
// added validateDeps so any Deps field declared `required` would error
// at construction time, eliminating per-RPC nil-checks. But putting
// `if d.Repo == nil` in validateDeps makes Bootstrap's bare-Deps New()
// fail before Setup ever runs. cpnext hit this on 6 services and
// reverted Repo from validateDeps as a workaround.
//
// Fix: move dep wiring from runtime mutation (ApplyDeps) into
// codegen (wire_gen.go). Setup now only builds infrastructure (DB
// pool, NATS connection, audit sink) and assigns it onto
// user-extendable fields of *App. Bootstrap calls
// `wireXxxDeps(app, cfg)` to assemble the full Deps and passes it
// straight into `xxx.New(deps)`. validateDeps gates the *complete*
// dep set at startup; ApplyDeps disappears.
//
// The producer-resolution rules are deliberately small to keep the
// codegen path narrow:
//
//   1. Logger     → logger.With("service", svc.Name)  (bootstrap arg)
//   2. Config     → cfg                                (bootstrap arg)
//   3. Authorizer → middleware.Authorizer constructed in wire_gen,
//                   with devMode swap to middleware.DevAuthorizer{}
//   4. DB orm.Context  → app.ORMContext() (when ORM is enabled; nil-safe)
//   5. Otherwise: look up app.<DepFieldName> by exact-name match.
//      If a matching exported App field exists, wire it.
//   6. Config blocks by TYPE: a field typed `config.<Block>` /
//      `*config.<Block>` (a component config block generated from
//      proto/config) wires to `cfg.<Field>` / `&cfg.<Field>` when
//      exactly one root Config field has that type. Zero matches fall
//      through; multiple matches are a hard error listing candidates.
//   7. Nothing matched: emit a typed zero + TODO and warn (compile
//      passes only when validateDeps doesn't require the field — a
//      clean error path). Scalar fields get the config-block hint:
//      scalars are configuration, not collaborators.
//
// Adding new conventional sources should mean *one* new case in
// wireExpressionFor below + one line in pkg/app/CONVENTIONS.md.
// Resist the temptation to add forge-yaml-driven mappings; the whole
// point of resolving against the live App struct is that users
// extend wire_gen by adding fields to App, not by editing forge.yaml.

// WireGenServiceData carries one service's wire_gen template inputs.
type WireGenServiceData struct {
	// FieldName is the exported Go field name on the Services struct
	// (e.g. "AdminServer"). wire_gen uses it as the suffix on the
	// generated function name: wireAdminServerDeps.
	FieldName string

	// Package is the snake-case Go package name of the handler
	// (e.g. "admin_server"). Matches the directory under handlers/.
	Package string

	// Alias is the import alias used for the handler package in
	// wire_gen.go. Mirrors BootstrapComponentData.Alias so cross-role
	// collisions ("svc" vs "pkg") surface the same alias both files.
	Alias string

	// Name is the runtime-facing kebab-case name (e.g. "admin-server")
	// — used as the slog `service` attribute when constructing the
	// service-scoped logger.
	Name string

	// NeedsAuthzVar is true when the Deps struct has an Authorizer
	// field. Skips the var-decl block in the template when no
	// Authorizer is present (rare, but library-style services hit it).
	// The rendered block reads cfg.Mode().IsDev() itself for the
	// DevAuthorizer swap — there is no devMode parameter threading.
	NeedsAuthzVar bool

	// Assignments are the per-Deps-field key/value pairs the template
	// emits inside the struct literal. Order matches the order of
	// fields in the Deps struct so the rendered file is stable
	// across regenerates.
	Assignments []WireAssignment

	// UnresolvedFields lists Deps fields that wire_gen could not
	// match to an App field or a conventional source. The template
	// emits these into a header comment so users see the warning
	// without grep-ing the file. Compile still succeeds (the
	// assignment uses zero-value), but validateDeps will reject the
	// service at startup if the user marked the field required.
	UnresolvedFields []WireUnresolved

	// ImportPath is the project-relative path used in the wire_gen
	// import line (e.g. "internal/handlers/billing", "internal/workers/idle_detector",
	// "internal/operators/workspace_controller"). Lets one template render
	// services, workers, and operators without role-specific blocks.
	ImportPath string

	// LoggerAttrKey is the slog attribute key the wireXxxDeps function
	// uses when constructing the per-component scoped logger
	// (`logger.With(<key>, runtimeName)`). "service" for handlers,
	// "worker" for periodic-task workers, "operator" for k8s
	// reconcilers — matches the bootstrap scoping convention so log
	// queries by component-kind keep working post-wire-gen.
	LoggerAttrKey string
}

// WireAssignment is one `Field: Expr,` line in the rendered
// wireXxxDeps return literal.
type WireAssignment struct {
	Field   string
	Expr    string
	Comment string // optional inline `// ...` annotation
}

// WireUnresolved is a Deps field that wire_gen had to leave at zero
// value because no producer matched. Surfaces in the wire_gen.go
// header comment so users see the warning without having to grep.
type WireUnresolved struct {
	Name string
	Type string
	Hint string
}

// PlaceholderResolver describes one typed `resolve<Field>(app) <Type>`
// helper the wire_gen template emits at file scope. The helper bridges
// an `any`-typed AppExtras field (during a parallel-lane port where the
// real type lives in a sibling lane that hasn't merged yet) to the
// typed Deps field that consumes it.
//
// One PlaceholderResolver is generated per AppExtras placeholder
// referenced by at least one Deps field. The set is deduped by Name so
// multi-service consumption of the same placeholder emits one helper.
type PlaceholderResolver struct {
	// Name is the AppExtras field name (also the Deps field name —
	// resolution is exact-match by name).
	Name string

	// TargetType is the type the helper returns; matches the
	// `forge:placeholder: <Type>` annotation on the AppExtras field.
	TargetType string
}

// UnresolvedPlaceholder is an AppExtras field that carries the
// `forge:placeholder` marker but is still typed `any`. The build-time
// gate (lint + forge generate) treats these as ERRORS — they're the
// failure mode the placeholder annotation exists to surface.
type UnresolvedPlaceholder struct {
	// FieldName is the AppExtras field name.
	FieldName string

	// CurrentType is the type as declared today (typically "any").
	CurrentType string

	// TargetType is the type the user promised to tighten to.
	TargetType string
}

// GenerateWireGen emits pkg/app/wire_gen.go. Returns nil with no file
// written when there are no services AND no workers AND no operators.
//
// Resolution order for each Deps field (services, workers, operators):
//  1. Conventional names (Logger, Config, Authorizer, DB) get hardcoded
//     sources matching pkg/app/CONVENTIONS.md.
//  2. Other field names are matched exact-case against existing *App
//     fields. A match emits `app.<Field>`.
//  3. Fields typed as a generated config block (`config.<Block>` /
//     `*config.<Block>` from proto/config) resolve by TYPE to the unique
//     root Config field of that type → `cfg.<Field>` / `&cfg.<Field>`.
//     Multiple Config fields of the same block type are a hard error.
//  4. No match emits a typed-zero-value placeholder (e.g. `""` for
//     string, `0` for int, `false` for bool, `nil` for everything else)
//     plus a header-comment note. Compile still succeeds; validateDeps
//     surfaces the gap at startup if the field is marked required.
//     Scalar fields get a pointed hint to declare a config block —
//     scalars are configuration, not collaborators.
//
// Workers and operators get the same wire treatment as services so the
// per-RPC `if s.deps.X == nil` pattern is also gone for periodic-task
// and reconciler code paths. Bootstrap calls
// `wireWorker<X>Deps` / `wireOperator<X>Deps` and passes the resulting
// Deps straight into worker.New / operator.New, mirroring the service
// path. Authorizer is service-specific and not emitted for workers or
// operators (their Deps don't have an authz field by convention).
//
// packages/workers/operators are passed through so wire_gen and
// bootstrap derive identical FieldName values via ResolveCollisionNaming
// — `Services.SvcBilling` on the bootstrap side calls
// `wireSvcBillingDeps` from this file when "billing" collides with
// internal/billing.
func GenerateWireGen(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, projectDir string, ormEnabled bool, cs *checksums.FileChecksums) error {
	_, err := GenerateWireGenData(services, packages, workers, operators, modulePath, projectDir, ormEnabled, cs)
	return err
}

// WireGenData is the per-component wire-resolution result GenerateWireGenData
// returns. Used by downstream codegen (diagnostics_gen.go) that needs to know
// which Deps fields landed at typed-zero so the runtime registry can name them
// at boot. The shape mirrors the template inputs used internally — same
// FieldName / UnresolvedFields semantics.
type WireGenData struct {
	Services  []WireGenServiceData
	Workers   []WireGenServiceData
	Operators []WireGenServiceData
}

// GenerateWireGenData is the variant of GenerateWireGen that also
// returns the per-component WireGenData. Callers that need the
// post-resolution view of the project (diagnostics codegen, future
// tooling) use this; the GenerateWireGen entry point is the
// backwards-compatible thin wrapper.
func GenerateWireGenData(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, projectDir string, ormEnabled bool, cs *checksums.FileChecksums) (WireGenData, error) {
	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return WireGenData{}, nil
	}

	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return WireGenData{}, err
	}

	// Parse the existing App struct so we can resolve unconventional
	// Deps field names. Empty/missing is fine — the conventional rules
	// still cover the bare-Deps trio.
	appFields, err := ParseAppFields(appDir)
	if err != nil {
		return WireGenData{}, fmt.Errorf("parse pkg/app for App fields: %w", err)
	}
	// appFieldByName is the lookup wireExpressionForApp consumes —
	// carries Type AND Placeholder so the resolver can emit a typed
	// `resolve<Field>(app)` accessor when a sibling lane hasn't
	// landed the real type yet.
	appFieldByName := map[string]AppField{}
	for _, f := range appFields {
		appFieldByName[f.Name] = f
	}

	// Build-time gate: any AppExtras field carrying `forge:placeholder`
	// AND still typed `any` is the unresolved-placeholder failure mode.
	// The annotation says "this should be tightened to <Type> once the
	// sibling lane lands"; if the field is still `any` at codegen time,
	// the lane never landed (or landed and was forgotten), and wire_gen
	// must fail loudly rather than emit a silent type-asserting accessor
	// that may panic at runtime.
	//
	// Once the user tightens the field declaration to the target type,
	// the marker becomes a no-op (the field type matches the target;
	// the generated resolver compiles either way).
	var unresolvedPlaceholders []UnresolvedPlaceholder
	for _, f := range appFields {
		if f.Placeholder == "" {
			continue
		}
		// "any" with surrounding whitespace is the only shape that
		// counts as unresolved. A field already typed as the target
		// (or any other concrete type) is considered tightened — the
		// marker is then redundant but harmless.
		if strings.TrimSpace(f.Type) == "any" || strings.TrimSpace(f.Type) == "interface{}" {
			unresolvedPlaceholders = append(unresolvedPlaceholders, UnresolvedPlaceholder{
				FieldName:   f.Name,
				CurrentType: f.Type,
				TargetType:  f.Placeholder,
			})
		}
	}
	if len(unresolvedPlaceholders) > 0 {
		var msg strings.Builder
		msg.WriteString("forge:placeholder annotations have not been tightened to their target types — wire_gen cannot emit a typed accessor.\n\n")
		msg.WriteString("The following AppExtras fields are still typed `any` despite carrying a placeholder marker:\n\n")
		for _, up := range unresolvedPlaceholders {
			fmt.Fprintf(&msg, "  - %s: declared `%s`, marker promises `%s`\n", up.FieldName, up.CurrentType, up.TargetType)
		}
		msg.WriteString("\nFix: open pkg/app/app_extras.go, change the field declaration from\n")
		msg.WriteString("    <Field> any `forge:placeholder:\"<Type>\"`\n")
		msg.WriteString("(or its comment-shape equivalent) to\n")
		msg.WriteString("    <Field> <Type>\n")
		msg.WriteString("then re-run `forge generate`. The marker becomes a no-op once the field type matches its target.\n")
		return WireGenData{}, fmt.Errorf("%s", msg.String())
	}

	// Resolvers collected across all services, workers, operators —
	// deduped by AppExtras field name so the same placeholder
	// referenced from multiple components emits one helper.
	resolverNeeds := map[string]PlaceholderResolver{}

	// Project-wide assignability matcher — shared across services,
	// workers, operators so pkg/app + each component package gets
	// loaded at most once per generate. The matcher is consulted only
	// when a Deps field has a name match against AppExtras but the
	// pretty-printed type strings differ — exactly the case the
	// legacy name-only resolution got wrong.
	matcher := NewDepsAssignabilityMatcher(projectDir)

	// Component config-block index: generated pkg/config block type name
	// → root Config field(s) of that type, derived from the same forge
	// descriptor GenerateConfigLoader consumes so wire_gen and
	// pkg/config/config.go always agree on the block shape. Consulted
	// only when a Deps field resolves to nothing else — a field typed
	// `config.<Block>` / `*config.<Block>` wires to `cfg.<Field>` /
	// `&cfg.<Field>` when exactly one Config field has the block type.
	configBlocks := loadConfigBlockIndex(projectDir)

	// Resolve each service's handler dir + package clause from disk ONCE
	// (shared by the collision counts and the per-service loop below).
	// Disk-first: the import line and the package selector must reflect
	// what's actually under handlers/, not a re-synthesis of the proto
	// name — see disk_resolver.go for the broken-imports bug class this
	// kills. Synthesis only applies to not-yet-scaffolded services.
	svcResolved := make([]ResolvedComponent, 0, len(services))
	for _, svc := range services {
		res, err := ResolveServiceComponent(projectDir, svc.Name)
		if err != nil {
			return WireGenData{}, err
		}
		svcResolved = append(svcResolved, res)
	}

	// Build the collision-aware naming map ONCE, shared with bootstrap.
	// We synthesize service "components" from the resolved packages just
	// so the counts include service packages; bootstrap does the same in
	// GenerateBootstrap before calling AssignBootstrapAliases.
	svcComponents := make([]BootstrapServiceData, 0, len(services))
	for _, res := range svcResolved {
		svcComponents = append(svcComponents, BootstrapServiceData{Package: res.PackageName})
	}
	counts := CollisionCounts(svcComponents, packages, workers, operators)

	var wireSvcs []WireGenServiceData
	needsAuthorizerImport := false
	for i, svc := range services {
		res := svcResolved[i]
		pkg := res.PackageName
		// FieldName derives from svc.Name (which retains separators /
		// PascalCase boundaries) exactly like GenerateBootstrap's
		// per-service mapping — bootstrap.go calls wire<FieldName>Deps,
		// so the two MUST agree even for multi-word names where the
		// compact package form ("adminserver") can't be split back into
		// words. The collision branch (rare) gets the "SvcXxx" prefix
		// from ResolveCollisionNaming.
		fallbackFieldName := naming.ToPascalCase(strings.TrimSuffix(svc.Name, "Service"))
		if fallbackFieldName == "" {
			fallbackFieldName = naming.ToPascalCase(svc.Name)
		}
		alias, fieldName := ResolveCollisionNaming(pkg, fallbackFieldName, "svc", counts)
		// Runtime (slog `service` attr) name: kebab of the original
		// svc.Name — matches the BootstrapServiceData.Name bootstrap
		// derives, so log queries by service name line up across both
		// generated files.
		runtimeName := naming.ToKebabCase(strings.TrimSuffix(svc.Name, "Service"))
		if runtimeName == "" {
			runtimeName = naming.ToKebabCase(svc.Name)
		}

		handlerDir := res.Dir
		depsFields, parseErr := ParseServiceDeps(handlerDir)
		if parseErr != nil {
			// Intentional soft warning (no --strict promotion): a parse
			// failure here means the handler dir has a syntactically
			// broken Deps struct. We log and move on so wire_gen.go is
			// still emitted (with no entry for this service) and the
			// user sees the canonical error from the regular Go
			// compile step. Promoting to fatal here would mask the
			// richer Go-compiler diagnostic with a redundant codegen
			// abort. Lives in internal/codegen (no pipelineContext
			// reach), so no --strict thread.
			fmt.Fprintf(os.Stderr, "Warning: parsing %s Deps: %v\n", pkg, parseErr)
			depsFields = nil
		}

		data := WireGenServiceData{
			FieldName:     fieldName,
			Package:       pkg,
			Alias:         alias,
			Name:          runtimeName,
			ImportPath:    "internal/handlers/" + res.ImportLeaf,
			LoggerAttrKey: "service",
		}

		// Track whether we have an Authorizer field — drives the
		// `var authz` block (with its cfg.Mode().IsDev() DevAuthorizer
		// swap) in the rendered function body.
		for _, df := range depsFields {
			if df.Name == "Authorizer" {
				data.NeedsAuthzVar = true
				needsAuthorizerImport = true
				break
			}
		}

		// pkgDir is the on-disk directory leaf (res.ImportLeaf), not the
		// package clause — the matcher loads the package by path.
		svcTC := &appFieldTypeChecker{matcher: matcher, roleRoot: "internal/handlers", pkgDir: res.ImportLeaf}
		for _, df := range depsFields {
			expr, comment, unresolved, provenMismatch := wireExpressionForApp(df, appFieldByName, ormEnabled, runtimeName, resolverNeeds, svcTC)
			// Config-block resolution by TYPE: a fallthrough field whose
			// type is a generated config block (`config.<Block>` /
			// `*config.<Block>`) wires to the unique Config field of that
			// type. Runs only when nothing else matched — an explicit
			// AppExtras name match keeps precedence — and never on a
			// proven name-match mismatch (that's a misconfiguration the
			// user must see, not a fallthrough).
			if unresolved != "" && !provenMismatch {
				bexpr, bcomment, ok, berr := resolveConfigBlock(df, runtimeName, configBlocks)
				if berr != nil {
					return WireGenData{}, berr
				}
				if ok {
					expr, comment, unresolved = bexpr, bcomment, ""
				}
			}
			// Optional fields that fall through to the typed-zero
			// branch get the silent treatment: no inline TODO comment,
			// no contribution to the UNRESOLVED header. The user
			// explicitly opted in to "may be nil" via
			// `// forge:optional-dep`, so warning every regenerate
			// would be noise.
			//
			// EXCEPT on a proven name-match type mismatch: that's a
			// misconfiguration (AppExtras holds a value the user meant
			// to wire, typed incompatibly), not the intentional-nil
			// state — staying loud is what surfaces the silent-downgrade
			// class from kalshi FORGE_BACKLOG #13.
			if df.Optional && unresolved != "" && !provenMismatch {
				comment = ""
				unresolved = ""
			}
			data.Assignments = append(data.Assignments, WireAssignment{
				Field:   df.Name,
				Expr:    expr,
				Comment: comment,
			})
			if unresolved != "" {
				data.UnresolvedFields = append(data.UnresolvedFields, WireUnresolved{
					Name: df.Name,
					Type: df.Type,
					Hint: unresolved,
				})
			}
		}

		wireSvcs = append(wireSvcs, data)
	}

	// Workers and operators get wireXxxDeps too. They reuse the same
	// WireGenServiceData carrier — the template treats them identically
	// other than the import-path prefix and the per-component logger
	// attribute key ("worker" / "operator" instead of "service").
	wireWorkers, err := buildWireComponentData(workers, "wkr", "internal/workers", "worker", projectDir, appFieldByName, ormEnabled, counts, resolverNeeds, matcher, configBlocks)
	if err != nil {
		return WireGenData{}, fmt.Errorf("build worker wire data: %w", err)
	}
	wireOperators, err := buildWireComponentData(operators, "op", "internal/operators", "operator", projectDir, appFieldByName, ormEnabled, counts, resolverNeeds, matcher, configBlocks)
	if err != nil {
		return WireGenData{}, fmt.Errorf("build operator wire data: %w", err)
	}

	// Sort resolvers by name so the rendered file is stable across
	// regenerates (map iteration is intentionally random).
	resolvers := make([]PlaceholderResolver, 0, len(resolverNeeds))
	for _, r := range resolverNeeds {
		resolvers = append(resolvers, r)
	}
	sort.Slice(resolvers, func(i, j int) bool {
		return resolvers[i].Name < resolvers[j].Name
	})

	tmplData := struct {
		Module                string
		Services              []WireGenServiceData
		Workers               []WireGenServiceData
		Operators             []WireGenServiceData
		NeedsAuthorizerImport bool
		Resolvers             []PlaceholderResolver
	}{
		Module:                modulePath,
		Services:              wireSvcs,
		Workers:               wireWorkers,
		Operators:             wireOperators,
		NeedsAuthorizerImport: needsAuthorizerImport,
		Resolvers:             resolvers,
	}

	content, err := templates.ProjectTemplates().Render("wire_gen.go.tmpl", tmplData)
	if err != nil {
		return WireGenData{}, fmt.Errorf("render wire_gen.go.tmpl: %w", err)
	}

	if err := writeForgeOwned(projectDir, filepath.Join("pkg", "app", "wire_gen.go"), content, cs); err != nil {
		return WireGenData{}, fmt.Errorf("write pkg/app/wire_gen.go: %w", err)
	}
	return WireGenData{
		Services:  wireSvcs,
		Workers:   wireWorkers,
		Operators: wireOperators,
	}, nil
}

// buildWireComponentData constructs WireGenServiceData entries for
// workers or operators. Same shape as the per-service loop, factored
// out because workers and operators differ only in the role prefix,
// the directory under projectDir ("workers"/"operators"), and the
// per-component logger-attribute key ("worker"/"operator").
//
// Returns an empty slice (not nil) when comps is empty so range over the
// result is a no-op without nil-check ceremony at the call site.
func buildWireComponentData(comps []BootstrapComponentData, rolePrefix, subdir, loggerAttrKey, projectDir string, appFieldByName map[string]AppField, ormEnabled bool, counts map[string]int, resolverNeeds map[string]PlaceholderResolver, matcher *DepsAssignabilityMatcher, configBlocks map[string][]string) ([]WireGenServiceData, error) {
	if len(comps) == 0 {
		return nil, nil
	}
	out := make([]WireGenServiceData, 0, len(comps))
	for _, c := range comps {
		// Use the pre-computed FieldName as the no-collision fallback.
		// WorkerDataFromNames / OperatorDataFromNames built it via
		// ToExportedFieldName, which is what bootstrap.go expects in the
		// emitted Workers / Operators struct field references. Matching
		// here keeps wire_gen ↔ bootstrap aligned.
		alias, fieldName := ResolveCollisionNaming(c.Package, c.FieldName, rolePrefix, counts)
		// Runtime (slog attr) name: kebab of the user-facing forge.yaml
		// name (c.Name), which retains the original word boundaries.
		// Pre-disk-first this derived from c.Package — fine when Package
		// was the compact synthesis, but Package is now the on-disk
		// package CLAUSE, which may not match the user-facing name at all.
		runtimeName := strings.ReplaceAll(c.Name, "_", "-")
		if runtimeName == "" {
			runtimeName = strings.ReplaceAll(c.Package, "_", "-")
		}

		// c.ImportPath is the on-disk directory leaf (disk-first, from
		// WorkerDataFromNames / OperatorDataFromNames) — the only valid
		// base for BOTH the Deps AST probe and the generated import line.
		// c.Package is the package clause and may legally differ from the
		// directory name.
		compDir := filepath.Join(projectDir, subdir, filepath.FromSlash(c.ImportPath))
		depsFields, parseErr := ParseServiceDeps(compDir)
		if parseErr != nil {
			// Intentional soft warning — same rationale as the service
			// branch above: a broken Deps struct surfaces a clearer
			// error from `go build`. No --strict thread because we're
			// in internal/codegen (no pipelineContext reach). Names the
			// on-disk dir (c.ImportPath) — that's the path that was
			// actually parsed.
			fmt.Fprintf(os.Stderr, "Warning: parsing %s/%s Deps: %v\n", subdir, c.ImportPath, parseErr)
			depsFields = nil
		}

		data := WireGenServiceData{
			FieldName:     fieldName,
			Package:       c.Package,
			Alias:         alias,
			Name:          runtimeName,
			ImportPath:    subdir + "/" + c.ImportPath,
			LoggerAttrKey: loggerAttrKey,
		}

		// pkgDir is the on-disk directory leaf (c.ImportPath), not the
		// package clause — the matcher loads the package by path.
		compTC := &appFieldTypeChecker{matcher: matcher, roleRoot: subdir, pkgDir: c.ImportPath}
		for _, df := range depsFields {
			expr, comment, unresolved, provenMismatch := wireExpressionForApp(df, appFieldByName, ormEnabled, runtimeName, resolverNeeds, compTC)
			// Config-block resolution by TYPE — same rules as the
			// service loop above: fallthrough-only, unique-match,
			// ambiguity is a hard error.
			if unresolved != "" && !provenMismatch {
				bexpr, bcomment, ok, berr := resolveConfigBlock(df, runtimeName, configBlocks)
				if berr != nil {
					return nil, berr
				}
				if ok {
					expr, comment, unresolved = bexpr, bcomment, ""
				}
			}
			// Workers/operators are not expected to declare Authorizer
			// (no inbound RPCs), so they rarely hit this hook.
			// If a Deps struct does declare one, we honor it and set
			// NeedsAuthzVar — keeps the codegen consistent if a project
			// invents a worker that exposes an HTTP listener.
			if df.Name == "Authorizer" {
				data.NeedsAuthzVar = true
			}
			// Optional Deps fields get the silent treatment — see the
			// service-loop comment above for the full rationale (and
			// for why a PROVEN name-match mismatch stays loud).
			if df.Optional && unresolved != "" && !provenMismatch {
				comment = ""
				unresolved = ""
			}
			data.Assignments = append(data.Assignments, WireAssignment{
				Field:   df.Name,
				Expr:    expr,
				Comment: comment,
			})
			if unresolved != "" {
				data.UnresolvedFields = append(data.UnresolvedFields, WireUnresolved{
					Name: df.Name,
					Type: df.Type,
					Hint: unresolved,
				})
			}
		}
		out = append(out, data)
	}
	return out, nil
}

// wireExpressionForApp is the placeholder-aware resolver. When a Deps
// field name matches an AppExtras field carrying `forge:placeholder:
// <Type>`, the resolver registers a typed accessor `resolve<Field>(app)
// <Type>` in resolverNeeds and emits a call to it from the wireXxxDeps
// return literal. The accessor compiles whether app.<Field> is typed
// `any` (during the cross-lane port) or already typed `<Type>` (after
// the user tightens the declaration).
//
// All other resolution rules (conventional names, exact-name app
// match, typed-zero fallback) match wireExpressionFor exactly — the
// placeholder branch is purely additive.
//
// The fourth return, provenMismatch, is true ONLY when the field
// name-matched an App/AppExtras field and the type checker PROVED (in
// a single shared type universe — see deps_assignability.go) that the
// app side is not assignable to the Deps side. Callers use it to keep
// the typed-zero fallout LOUD even for `forge:optional-dep` fields: a
// name-matched type conflict is a misconfiguration the user must see,
// not the intentional "may be nil" state the optional marker opts
// into. (kalshi FORGE_BACKLOG #13: optional worker deps were silently
// downgraded from app.<Field> to nil with no TODO and no lint finding.)
func wireExpressionForApp(df DepsField, appFields map[string]AppField, ormEnabled bool, runtimeName string, resolverNeeds map[string]PlaceholderResolver, tm *appFieldTypeChecker) (expr, comment, unresolvedHint string, provenMismatch bool) {
	switch df.Name {
	case "Logger":
		return fmt.Sprintf("logger.With(\"service\", %q)", runtimeName), "", "", false
	case "Config":
		return "cfg", "", "", false
	case "Authorizer":
		return "authz", "devMode swap to middleware.DevAuthorizer in development", "", false
	case "DB":
		switch {
		case strings.Contains(df.Type, "orm.Context") && ormEnabled:
			// ORMContext() (app_gen.go) returns a TRUE nil interface when
			// the client was never constructed — `app.ORM` directly would
			// wrap a nil *orm.Client into a typed-nil that defeats
			// validateDeps' `== nil` gate and panics on the first RPC.
			return "app.ORMContext()", "nil-safe orm.Context accessor (validateDeps catches absence at boot)", "", false
		case strings.Contains(df.Type, "sql.DB"):
			return "app.DB", "", "", false
		}
	}

	if af, ok := appFields[df.Name]; ok {
		// Placeholder-tagged AppExtras field: emit a typed accessor
		// reference. The accessor itself is rendered at file scope from
		// the deduped resolverNeeds map. We register the entry here so
		// the template knows to emit it.
		if af.Placeholder != "" {
			resolverNeeds[df.Name] = PlaceholderResolver{
				Name:       df.Name,
				TargetType: af.Placeholder,
			}
			return fmt.Sprintf("resolve%s(app)", df.Name),
				fmt.Sprintf("typed accessor for forge:placeholder %s → %s", df.Name, af.Placeholder),
				"", false
		}
		// Name matches. Decide whether the types are compatible. The
		// legacy code emitted `app.<Field>` on name alone, which
		// produced a compile error when AppExtras.<Field> was typed
		// incompatibly with Deps.<Field> (the cp-forge audit-no-op bug
		// class). When the type checker PROVES a genuine mismatch
		// (single shared type universe — see deps_assignability.go),
		// fall through to the unresolved-typed-zero path so the failure
		// is LOUD (TODO comment + UNRESOLVED header line + lint
		// finding) rather than a compile error at a downstream call
		// site.
		//
		// When assignability is merely UNPROVEN (project mid-edit, load
		// failure), IsNameMismatch returns false and we wire the name
		// match — the deterministic fail-loud policy: the compiler
		// arbitrates a wrong wire loudly, and the rendered output no
		// longer flip-flops between structural and steady-state regens.
		if tm != nil && tm.IsNameMismatch(df.Name, df.Type, af.Type) {
			hint := fmt.Sprintf("AppExtras.%s is %s but Deps.%s wants %s — types are not assignable; align AppExtras to %s or re-construct %s in pkg/app/setup.go",
				df.Name, af.Type, df.Name, df.Type, df.Type, df.Name)
			return zeroValueLiteral(df.Type),
				fmt.Sprintf("TODO: wire %s — AppExtras.%s type (%s) is not assignable to %s", df.Name, df.Name, af.Type, df.Type),
				hint, true
		}
		return "app." + df.Name, "", "", false
	}

	return zeroValueLiteral(df.Type), "TODO: wire " + df.Name + " — see header comment", unresolvedDepHint(df, runtimeName), false
}

// wireExpressionFor maps one DepsField to the Go expression wire_gen
// should emit on the right-hand side. The third return is a hint
// added to the wire_gen.go header comment when no producer matched —
// empty when resolution succeeded.
//
// The mapping deliberately does NOT consult forge.yaml or any
// project-config file: the live App struct (parsed from pkg/app) and
// the live Deps struct (parsed from handlers/<svc>) are the only
// inputs. This keeps wire_gen extension a one-step ergonomic — add a
// field to App + setup it in setup.go, and the next regenerate picks
// it up by name — instead of forcing users to also edit forge.yaml.
//
// appFields here is the legacy-shape map from name → type used by
// external callers (and the pre-placeholder unit tests). The richer
// shape (AppField with Placeholder) is consumed by wireExpressionForApp
// above; this thin wrapper exists so existing callers / tests keep
// working without a signature churn.
// appFieldTypeChecker is the per-component lens onto the project-wide
// DepsAssignabilityMatcher. wireExpressionForApp consults it when it
// has a name match but the AppExtras and Deps type strings differ —
// the answer decides between "emit `app.<F>`" (assignable) and "emit
// typed zero + loud UNRESOLVED hint" (mismatch). nil disables the
// type-aware path entirely (legacy behavior: name-only match).
type appFieldTypeChecker struct {
	matcher  *DepsAssignabilityMatcher
	roleRoot string
	pkgDir   string
}

// IsNameMismatch reports true iff the matcher can PROVE AppExtras's
// type is genuinely NOT assignable to the Deps field's declared type
// (both sides type-checked in one shared universe). Returns false when
// the matcher is unavailable (load failure, no pkg/app, project
// mid-edit) — per the deterministic fail-loud policy in
// deps_assignability.go's header, unproven name matches WIRE so the
// compiler arbitrates; only a proven mismatch drops to typed-zero.
// This is what makes wire_gen.go output a pure function of the on-disk
// project state instead of flip-flopping with whether pkg/app happened
// to type-check mid-pipeline (kalshi FORGE_BACKLOG #13).
func (c *appFieldTypeChecker) IsNameMismatch(depsName, depsType, appType string) bool {
	if c == nil || c.matcher == nil {
		return false
	}
	kind := c.matcher.Match(c.roleRoot, c.pkgDir, depsName, depsType, appType, true)
	return kind == MatchNameMismatch
}

func wireExpressionFor(df DepsField, appFields map[string]string, ormEnabled bool, runtimeName string) (expr, comment, unresolvedHint string) {
	switch df.Name {
	case "Logger":
		// Always sourced from the bootstrap-supplied logger so the
		// per-service service-key attribute lands on every log line.
		// We don't fall back to app.Logger here because bootstrap
		// passes the logger as a function arg — keeping the
		// dependency on the arg makes the call site self-documenting.
		return fmt.Sprintf("logger.With(\"service\", %q)", runtimeName), "", ""
	case "Config":
		return "cfg", "", ""
	case "Authorizer":
		// Set up by the per-function `var authz` block — the
		// template renders that block when NeedsAuthzVar is true.
		return "authz", "devMode swap to middleware.DevAuthorizer in development", ""
	case "DB":
		// Two cases:
		//   - Type "orm.Context" with ORM enabled → app.ORMContext()
		//     (nil-safe accessor; see wireExpressionForApp). When ORM is
		//     *not* enabled but the type is still orm.Context, the
		//     project is mid-migration — emit a TODO.
		//   - Type "*sql.DB" → app.DB.
		switch {
		case strings.Contains(df.Type, "orm.Context") && ormEnabled:
			return "app.ORMContext()", "nil-safe orm.Context accessor (validateDeps catches absence at boot)", ""
		case strings.Contains(df.Type, "sql.DB"):
			return "app.DB", "", ""
		}
	}

	// Fall through: try to match the Deps field name against an
	// exported field on the live *App struct. Exact-case match keeps
	// the resolution unambiguous — alias differences ("Stripe" vs
	// "stripe") would silently collide otherwise.
	//
	// Note: the legacy shape ignores placeholder-tagged fields. The
	// caller in this package uses wireExpressionForApp instead, which
	// emits the typed `resolve<Field>(app)` accessor when a placeholder
	// is set. wireExpressionFor is retained for tests / external
	// consumers that pre-date the placeholder annotation.
	if _, ok := appFields[df.Name]; ok {
		return "app." + df.Name, "", ""
	}

	// No producer matched. Emit a typed zero-value placeholder so the
	// rendered file compiles even when the field is a non-pointer
	// scalar (string/int/bool/etc.) — `nil` would not. The exact
	// literal is chosen by zeroValueLiteral against the Deps field's
	// type string. validateDeps surfaces the unresolved field at
	// startup if the user marked it required; otherwise the zero value
	// is the legitimate "not configured" state.
	//
	// The hint is shaped as a literal one-line action the LLM/user can
	// paste straight into pkg/app/app_extras.go (or, for scalar fields,
	// into proto/config — see unresolvedDepHint). If the type is
	// unexported or comes from a non-imported package, the user still
	// has to add the import — the fix is "obvious from the build error"
	// once the field declaration is in place.
	return zeroValueLiteral(df.Type), "TODO: wire " + df.Name + " — see header comment", unresolvedDepHint(df, runtimeName)
}

// loadConfigBlockIndex reads the project's parsed config messages (from
// the forge descriptor — the same source GenerateConfigLoader consumes)
// and returns generated block type name → root Config field GoNames
// declaring that type, in declaration order. Returns nil when the
// project has no descriptor / no config blocks — block resolution then
// simply never matches, which is the pre-feature behavior.
func loadConfigBlockIndex(projectDir string) map[string][]string {
	messages, err := ParseConfigProto(filepath.Join(projectDir, "proto", "config"))
	if err != nil || len(messages) == 0 {
		return nil
	}
	idx := map[string][]string{}
	for _, ref := range ConfigBlocksFromMessages(messages) {
		idx[ref.TypeName] = append(idx[ref.TypeName], ref.FieldName)
	}
	if len(idx) == 0 {
		return nil
	}
	return idx
}

// resolveConfigBlock resolves a Deps field to a component config block
// by TYPE. The field type must be exactly `config.<Block>` (value) or
// `*config.<Block>` (pointer) where <Block> is a generated block type —
// the `config` qualifier is the conventional import name of the
// project's generated pkg/config, and requiring it keeps the match
// deterministic (a same-named type from another package never
// false-positives; an unconventional import alias falls through to the
// normal unresolved path).
//
// Unique-match policy:
//
//	exactly one Config field of the type → wire `cfg.<Field>` /
//	                                       `&cfg.<Field>`
//	zero                                 → no match (normal unresolved path)
//	multiple                             → hard error listing candidates;
//	                                       type-based resolution cannot
//	                                       pick one deterministically
//
// componentName is the runtime-facing component name, used only in the
// ambiguity error message.
func resolveConfigBlock(df DepsField, componentName string, configBlocks map[string][]string) (expr, comment string, ok bool, err error) {
	if len(configBlocks) == 0 {
		return "", "", false, nil
	}
	t := strings.TrimSpace(df.Type)
	ptr := strings.HasPrefix(t, "*")
	t = strings.TrimSpace(strings.TrimPrefix(t, "*"))
	const qualifier = "config."
	if !strings.HasPrefix(t, qualifier) {
		return "", "", false, nil
	}
	typeName := strings.TrimPrefix(t, qualifier)
	candidates := configBlocks[typeName]
	if len(candidates) == 0 {
		return "", "", false, nil
	}
	if len(candidates) > 1 {
		return "", "", false, fmt.Errorf(
			"wire %s: Deps.%s is typed %s, but %d Config fields hold config block %s (%s) — type-based resolution needs exactly one; give each component its own block message in proto/config, or wire the field explicitly via AppExtras",
			componentName, df.Name, df.Type, len(candidates), typeName, strings.Join(candidates, ", "))
	}
	expr = "cfg." + candidates[0]
	if ptr {
		expr = "&" + expr
	}
	return expr, fmt.Sprintf("config block %s (proto/config)", typeName), true, nil
}

// unresolvedDepHint renders the header-comment remediation for a Deps
// field no producer matched. Two shapes:
//
//   - Scalar fields (string/int/bool/float/time.Duration/...) are
//     CONFIGURATION, not collaborators — the AppExtras two-step would
//     just hand-project config forever (the kalshi WTIPersistMaxPerTick
//     friction, fr-ad24278452). The hint walks through declaring a
//     component config block instead.
//   - Everything else gets the classic AppExtras + setup.go two-step.
func unresolvedDepHint(df DepsField, runtimeName string) string {
	if zeroValueLiteral(df.Type) != "nil" {
		block := naming.ToPascalCase(runtimeName) + "Config"
		field := naming.ToSnakeCase(strings.TrimSuffix(block, "Config"))
		return fmt.Sprintf(
			"scalar Deps fields are configuration — declare `message %s { ... }` in proto/config/v1/config.proto, compose it on AppConfig (`%s %s = <next tag>;`), and replace `%s %s` with a typed field `Cfg config.%s`; wire_gen resolves it from cfg by type (see the forge architecture skill, \"Component config blocks\")",
			block, block, field, df.Name, df.Type, block)
	}
	return fmt.Sprintf("add `%s %s` to AppExtras in pkg/app/app_extras.go, then assign `app.%s = ...` in pkg/app/setup.go",
		df.Name, df.Type, df.Name)
}

// zeroValueLiteral returns the Go source literal that represents the
// zero value of the given pretty-printed type expression. The mapping
// is intentionally narrow: only the scalar kinds Go can express as a
// single short literal. Composite types (struct{}, [N]T, etc.) and
// every pointer / interface / slice / map / channel / function fall
// through to "nil" — which is the right zero value for the latter
// group and a "compile error points right at the assignment" for the
// former (rare, and the message is exactly what the user wants).
//
// The check is on the source-string form rather than the AST kind so
// callers don't need a *types.Info — the wire_gen pipeline has the
// pretty-printed Deps types already, never re-parses, and stays cheap.
func zeroValueLiteral(typeExpr string) string {
	t := strings.TrimSpace(typeExpr)
	switch t {
	case "string":
		return `""`
	case "bool":
		return "false"
	case "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64",
		"uintptr", "byte", "rune",
		"float32", "float64",
		"complex64", "complex128":
		return "0"
	case "time.Duration":
		// A frequent enough Deps shape (timeouts, intervals) that
		// hardcoding the well-known case avoids a confusing
		// `nil` → `cannot use nil as time.Duration` build error.
		// Anything else aliased from time.* still falls through to nil
		// and surfaces the same error at the assignment.
		return "0"
	}
	return "nil"
}
