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
//   4. DB orm.Context  → app.ORM    (when ORM is enabled)
//   5. Otherwise: look up app.<DepFieldName> by exact-name match.
//      If a matching exported App field exists, wire it. If not,
//      emit `nil /* TODO: ... */` and warn (compile passes only when
//      validateDeps doesn't require the field — a clean error path).
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
	NeedsAuthzVar bool

	// NeedsDevMode is true when wire_gen needs the devMode bool param —
	// matches NeedsAuthzVar today (the only conditional consumer of
	// devMode is the authz swap), kept as a separate flag so future
	// devMode-gated fields don't have to reuse the authz hook.
	NeedsDevMode bool

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
	// import line (e.g. "handlers/billing", "workers/idle_detector",
	// "operators/workspace_controller"). Lets one template render
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
//   1. Conventional names (Logger, Config, Authorizer, DB) get hardcoded
//      sources matching pkg/app/CONVENTIONS.md.
//   2. Other field names are matched exact-case against existing *App
//      fields. A match emits `app.<Field>`.
//   3. No match emits a typed-zero-value placeholder (e.g. `""` for
//      string, `0` for int, `false` for bool, `nil` for everything else)
//      plus a header-comment note. Compile still succeeds; validateDeps
//      surfaces the gap at startup if the field is marked required.
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
	if len(services) == 0 && len(workers) == 0 && len(operators) == 0 {
		return nil
	}

	appDir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0755); err != nil {
		return err
	}

	// Parse the existing App struct so we can resolve unconventional
	// Deps field names. Empty/missing is fine — the conventional rules
	// still cover the bare-Deps trio.
	appFields, err := ParseAppFields(appDir)
	if err != nil {
		return fmt.Errorf("parse pkg/app for App fields: %w", err)
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
		return fmt.Errorf("%s", msg.String())
	}

	// Resolvers collected across all services, workers, operators —
	// deduped by AppExtras field name so the same placeholder
	// referenced from multiple components emits one helper.
	resolverNeeds := map[string]PlaceholderResolver{}

	// Build the collision-aware naming map ONCE, shared with bootstrap.
	// We synthesize service "components" from ServiceDef just so the
	// counts include service packages; bootstrap does the same in
	// GenerateBootstrap before calling AssignBootstrapAliases.
	svcComponents := make([]BootstrapServiceData, 0, len(services))
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		svcComponents = append(svcComponents, BootstrapServiceData{Package: pkg})
	}
	counts := CollisionCounts(svcComponents, packages, workers, operators)

	var wireSvcs []WireGenServiceData
	needsAuthorizerImport := false
	for _, svc := range services {
		pkg := toServicePackage(svc.Name)
		// Services use ToPascalCase for FieldName — matches
		// GenerateBootstrap's per-service mapping. The collision branch
		// (rare) gets the "SvcXxx" prefix from ResolveCollisionNaming.
		alias, fieldName := ResolveCollisionNaming(pkg, naming.ToPascalCase(pkg), "svc", counts)
		runtimeName := strings.ReplaceAll(pkg, "_", "-")

		handlerDir := filepath.Join(projectDir, "handlers", pkg)
		depsFields, parseErr := ParseServiceDeps(handlerDir)
		if parseErr != nil {
			// Best-effort: a parse failure here means the handler dir
			// has a syntactically broken Deps struct. We log and move
			// on so wire_gen.go is still emitted (with no entry for
			// this service) and the user sees the real error from
			// the regular Go compile step.
			fmt.Fprintf(os.Stderr, "Warning: parsing %s Deps: %v\n", pkg, parseErr)
			depsFields = nil
		}

		data := WireGenServiceData{
			FieldName:    fieldName,
			Package:      pkg,
			Alias:        alias,
			Name:         runtimeName,
			ImportPath:   "handlers/" + pkg,
			LoggerAttrKey: "service",
		}

		// Track whether we have an Authorizer field — drives the
		// `var authz` block + the `devMode` parameter in the rendered
		// function signature.
		for _, df := range depsFields {
			if df.Name == "Authorizer" {
				data.NeedsAuthzVar = true
				data.NeedsDevMode = true
				needsAuthorizerImport = true
				break
			}
		}

		for _, df := range depsFields {
			expr, comment, unresolved := wireExpressionForApp(df, appFieldByName, ormEnabled, runtimeName, resolverNeeds)
			// Optional fields that fall through to the typed-zero
			// branch get the silent treatment: no inline TODO comment,
			// no contribution to the UNRESOLVED header. The user
			// explicitly opted in to "may be nil" via
			// `// forge:optional-dep`, so warning every regenerate
			// would be noise.
			if df.Optional && unresolved != "" {
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
	wireWorkers, err := buildWireComponentData(workers, "wkr", "workers", "worker", projectDir, appFieldByName, ormEnabled, counts, resolverNeeds)
	if err != nil {
		return fmt.Errorf("build worker wire data: %w", err)
	}
	wireOperators, err := buildWireComponentData(operators, "op", "operators", "operator", projectDir, appFieldByName, ormEnabled, counts, resolverNeeds)
	if err != nil {
		return fmt.Errorf("build operator wire data: %w", err)
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
		return fmt.Errorf("render wire_gen.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(projectDir, filepath.Join("pkg", "app", "wire_gen.go"), content, cs, true); err != nil {
		return fmt.Errorf("write pkg/app/wire_gen.go: %w", err)
	}
	return nil
}

// buildWireComponentData constructs WireGenServiceData entries for
// workers or operators. Same shape as the per-service loop, factored
// out because workers and operators differ only in the role prefix,
// the directory under projectDir ("workers"/"operators"), and the
// per-component logger-attribute key ("worker"/"operator").
//
// Returns an empty slice (not nil) when comps is empty so range over the
// result is a no-op without nil-check ceremony at the call site.
func buildWireComponentData(comps []BootstrapComponentData, rolePrefix, subdir, loggerAttrKey, projectDir string, appFieldByName map[string]AppField, ormEnabled bool, counts map[string]int, resolverNeeds map[string]PlaceholderResolver) ([]WireGenServiceData, error) {
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
		runtimeName := strings.ReplaceAll(c.Package, "_", "-")

		compDir := filepath.Join(projectDir, subdir, c.Package)
		depsFields, parseErr := ParseServiceDeps(compDir)
		if parseErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: parsing %s/%s Deps: %v\n", subdir, c.Package, parseErr)
			depsFields = nil
		}

		data := WireGenServiceData{
			FieldName:     fieldName,
			Package:       c.Package,
			Alias:         alias,
			Name:          runtimeName,
			ImportPath:    subdir + "/" + c.Package,
			LoggerAttrKey: loggerAttrKey,
		}

		for _, df := range depsFields {
			expr, comment, unresolved := wireExpressionForApp(df, appFieldByName, ormEnabled, runtimeName, resolverNeeds)
			// Workers/operators are not expected to declare Authorizer
			// (no inbound RPCs), so they don't get the devMode hook.
			// If a Deps struct does declare one, we honor it and set
			// NeedsAuthzVar — keeps the codegen consistent if a project
			// invents a worker that exposes an HTTP listener.
			if df.Name == "Authorizer" {
				data.NeedsAuthzVar = true
				data.NeedsDevMode = true
			}
			// Optional Deps fields get the silent treatment — see the
			// service-loop comment above for the full rationale.
			if df.Optional && unresolved != "" {
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
func wireExpressionForApp(df DepsField, appFields map[string]AppField, ormEnabled bool, runtimeName string, resolverNeeds map[string]PlaceholderResolver) (expr, comment, unresolvedHint string) {
	switch df.Name {
	case "Logger":
		return fmt.Sprintf("logger.With(\"service\", %q)", runtimeName), "", ""
	case "Config":
		return "cfg", "", ""
	case "Authorizer":
		return "authz", "devMode swap to middleware.DevAuthorizer in development", ""
	case "DB":
		switch {
		case strings.Contains(df.Type, "orm.Context") && ormEnabled:
			return "app.ORM", "*orm.Client implements orm.Context", ""
		case strings.Contains(df.Type, "sql.DB"):
			return "app.DB", "", ""
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
				""
		}
		return "app." + df.Name, "", ""
	}

	hint := fmt.Sprintf("add `%s %s` to AppExtras in pkg/app/app_extras.go, then assign `app.%s = ...` in pkg/app/setup.go",
		df.Name, df.Type, df.Name)
	return zeroValueLiteral(df.Type), "TODO: wire " + df.Name + " — see header comment", hint
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
		//   - Type "orm.Context" with ORM enabled → app.ORM (which
		//     implements orm.Context). When ORM is *not* enabled but
		//     the type is still orm.Context, the project is
		//     mid-migration — emit a TODO.
		//   - Type "*sql.DB" → app.DB.
		switch {
		case strings.Contains(df.Type, "orm.Context") && ormEnabled:
			return "app.ORM", "*orm.Client implements orm.Context", ""
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
	// paste straight into pkg/app/app_extras.go: name + Go type as it
	// appears in the Deps struct. If the type is unexported or comes
	// from a non-imported package, the user still has to add the
	// import — the fix is "obvious from the build error" once the
	// field declaration is in place.
	hint := fmt.Sprintf("add `%s %s` to AppExtras in pkg/app/app_extras.go, then assign `app.%s = ...` in pkg/app/setup.go",
		df.Name, df.Type, df.Name)
	return zeroValueLiteral(df.Type), "TODO: wire " + df.Name + " — see header comment", hint
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

