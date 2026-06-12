package templates

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestRenderTemplate_StripsBuildIgnoreFromRenderedOutput(t *testing.T) {
	content, err := WebhookTemplates().Render("webhook_routes_gen.go.tmpl", map[string]any{
		"Package":  "tasks",
		"Webhooks": []map[string]any{{"Name": "github", "PascalName": "Github"}},
	})
	if err != nil {
		t.Fatalf("WebhookTemplates().Render() error = %v", err)
	}

	rendered := string(content)
	if strings.HasPrefix(rendered, "//go:build ignore") {
		t.Fatal("rendered template should not retain //go:build ignore header")
	}
	if !strings.Contains(rendered, "func (s *Service) RegisterWebhookRoutes") {
		t.Fatal("rendered template should include webhook route registration")
	}
}

// TestBootstrapTemplate_ZeroServices verifies that bootstrap.go.tmpl renders
// valid Go code when all component lists (services, workers, operators,
// packages) are empty — the "CLI-only" project scenario.
func TestBootstrapTemplate_ZeroServices(t *testing.T) {
	data := struct {
		Module   string
		Services []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Packages []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Workers []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		Operators []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
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
		AllServiceNames     []string
	}{
		Module:       "example.com/myproject",
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap.go.tmpl: %v", err)
	}

	rendered := string(content)

	// Must contain key functions
	if !strings.Contains(rendered, "func Bootstrap(") {
		t.Fatal("missing Bootstrap function")
	}
	if !strings.Contains(rendered, "func BootstrapOnly(") {
		t.Fatal("missing BootstrapOnly function")
	}
	if !strings.Contains(rendered, "func (a *App) Shutdown(") {
		t.Fatal("missing Shutdown method")
	}

	// Must NOT contain service-specific imports
	if strings.Contains(rendered, "pkg/middleware") {
		t.Fatal("zero-service bootstrap should not import middleware")
	}

	// Regression for forge-new-empty-services-unused-locals (originally
	// the runAll declaration, now devMode after the 2026-06 appkit table
	// migration): `devMode :=` exists purely to feed the per-service
	// wireXxxDeps closures. When there are no services, those closures
	// are never emitted and the declaration becomes a "declared and not
	// used" compile error. go/parser doesn't flag unused locals so the
	// ParseFile check below cannot catch this on its own; guard the
	// literal declaration string instead.
	if strings.Contains(rendered, "devMode := ") {
		t.Fatal("zero-service bootstrap must not declare devMode (no service closure emitted; would fail 'declared and not used')")
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}

// TestBootstrapTemplate_WithServicesStillDeclaresRunAll guards the other side
// of the forge-new-empty-services-unused-locals fix: when services ARE
// configured, the per-service Construct closures consume `devMode`, and
// the def table must hand the names filter to appkit.Run (which owns
// the "empty names slice == mount everything" identity used by
// `./<bin> server` since the 2026-06 appkit table migration).
func TestBootstrapTemplate_WithServicesStillDeclaresRunAll(t *testing.T) {
	type svc struct {
		Name, Package, ImportPath, FieldName, Alias string
		Fallible, HasWebhooks                       bool
		ConnectPkg, ProtoServiceName                string
	}
	data := struct {
		Module              string
		HasDatabase         bool
		DatabaseDriver      string
		OrmEnabled          bool
		LeaderElectionID    string
		Services            []svc
		Packages            []struct{}
		Workers             []struct{}
		Operators           []struct{}
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
		AllServiceNames     []string
	}{
		Module:           "example.com/myproject",
		LeaderElectionID: "myproject-leader",
		Services: []svc{
			{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "apihandler"},
		},
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap.go.tmpl: %v", err)
	}
	rendered := string(content)

	if !strings.Contains(rendered, "devMode := ") {
		t.Fatal("bootstrap with services must declare devMode for the wireXxxDeps closures")
	}
	// Since the registration-in-code rework, the per-service Construct
	// closures (which consume devMode via wireXxxDeps) live in
	// services_gen.go; bootstrap consumes devMode by handing it to the
	// user-owned RegisteredServices row list.
	if !strings.Contains(rendered, "RegisteredServices(app, cfg, logger, devMode, opts...)") {
		t.Fatal("bootstrap with services must consume devMode via the RegisteredServices call")
	}
	if !strings.Contains(rendered, "appkit.Run(def, mux, logger, appkit.Options{Only: names})") {
		t.Fatal("bootstrap must delegate name filtering to appkit.Run via Options.Only")
	}
}

// TestBootstrapTemplate_LoudFilterBanner pins the loud-by-default contract
// for the `./<bin> server <name>...` subcommand filter. The banner names
// BOTH registered and excluded services/workers/operators so a user who
// typo'd a name or forgot a service in the args sees it at boot instead of
// chasing 404s through CORS/proxy/auth. Silent-skip here was a real
// debug-time-sink; this test stops a future template edit from
// reintroducing it.
//
// Since the 2026-06 appkit table migration the banner BEHAVIOR (known-set
// computation, unknown-name warning, registered/excluded Warn) lives in
// appkit.Run — pinned by pkg/appkit's filter-banner tests. What the
// generated file must still guarantee is the DATA the banner is computed
// from: a Name row for every service, worker, and operator, and the
// names slice handed to appkit.Run via Options.Only. Dropping a
// component kind from the def table would silently drop it from the
// excluded report, defeating the whole point — exactly the regression
// this test originally guarded.
func TestBootstrapTemplate_LoudFilterBanner(t *testing.T) {
	type svc struct {
		Name, Package, ImportPath, FieldName, Alias, VarName string
		Fallible, HasWebhooks                                bool
	}
	type wkr struct {
		Name, Package, ImportPath, FieldName, Alias, VarName string
		Fallible                                             bool
	}
	type op struct {
		Name, Package, ImportPath, FieldName, Alias, VarName string
		Fallible                                             bool
	}
	data := struct {
		Module              string
		HasDatabase         bool
		DatabaseDriver      string
		OrmEnabled          bool
		LeaderElectionID    string
		Services            []svc
		Packages            []struct{}
		Workers             []wkr
		Operators           []op
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
		AllServiceNames     []string
	}{
		Module:           "example.com/myproject",
		LeaderElectionID: "myproject-leader",
		Services: []svc{
			{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "api", VarName: "api"},
			{Name: "billing", Package: "billing", ImportPath: "billing", FieldName: "Billing", Alias: "billing", VarName: "billing"},
		},
		Workers: []wkr{
			{Name: "indexer", Package: "indexer", ImportPath: "indexer", FieldName: "Indexer", Alias: "indexer", VarName: "indexer"},
		},
		Operators: []op{
			{Name: "scaler", Package: "scaler", ImportPath: "scaler", FieldName: "Scaler", Alias: "scaler", VarName: "scaler"},
		},
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap.go.tmpl: %v", err)
	}
	rendered := string(content)

	// Every component kind must contribute Name rows to the def table —
	// appkit.Run computes the banner's known set from these rows, so a
	// missing kind cannot be reported as excluded. Service rows come
	// from the user-owned RegisteredServices (registration-in-code);
	// worker/operator rows stay inline.
	for _, name := range []string{`Services: RegisteredServices(app, cfg, logger, devMode, opts...)`, `{Name: "indexer", Construct: func() error {`, `{Name: "scaler", Construct: func() error {`} {
		if !strings.Contains(rendered, name) {
			t.Errorf("bootstrap def table missing row %s — appkit's filter banner cannot report this name as excluded", name)
		}
	}

	// The names filter must reach appkit.Run, which owns the unknown-name
	// warning and the registered/excluded banner.
	if !strings.Contains(rendered, "appkit.Run(def, mux, logger, appkit.Options{Only: names})") {
		t.Error("bootstrap must hand the names filter to appkit.Run via Options.Only — that is where the loud filter banner lives now")
	}

	// And the banner behavior itself must NOT leak back inline (the
	// table-not-program rule).
	if strings.Contains(rendered, "server filter active") {
		t.Error("filter banner emission belongs to appkit.Run, not the generated table")
	}

	fset := token.NewFileSet()
	if _, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors); parseErr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}

// TestBootstrapTemplate_DevModeAuthzBanner pins the loud-by-default
// dev-mode banner. When cfg.Environment == "development" the per-service
// Authorizer is swapped to middleware.DevAuthorizer{} (allow-all), so a
// prod manifest that accidentally sets ENVIRONMENT=development would
// silently ship with authz disabled. The Warn line surfaces this at
// every container start so leakage between envs is caught immediately
// rather than by a security review months later.
//
// Zero-service projects don't get the swap (no Authorizer to flip) so
// the banner is gated on .Services like the devMode local is.
func TestBootstrapTemplate_DevModeAuthzBanner(t *testing.T) {
	type svc struct {
		Name, Package, ImportPath, FieldName, Alias, VarName string
		Fallible, HasWebhooks                                bool
	}
	mkData := func(services []svc) any {
		return struct {
			Module              string
			HasDatabase         bool
			DatabaseDriver      string
			OrmEnabled          bool
			Services            []svc
			Packages            []struct{}
			Workers             []struct{}
			Operators           []struct{}
			ConfigFields        map[string]bool
			RESTEnabled         bool
			ConnectImports      []string
			DiagnosticsEnabled  bool
			StrictWiringEnabled bool
			AllServiceNames     []string
			HasFallible         bool
		}{
			Module:       "example.com/myproject",
			Services:     services,
			ConfigFields: map[string]bool{"Environment": true},
		}
	}

	t.Run("with-services-emits-banner", func(t *testing.T) {
		data := mkData([]svc{
			{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "api", VarName: "api"},
		})
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		rendered := string(content)

		// Since the 2026-06 appkit table migration Bootstrap is a thin
		// delegate to BootstrapOnly, so a SINGLE banner site covers both
		// entry points (pre-migration each function computed devMode and
		// emitted its own). Per-service subcommands and `./<bin> server`
		// both flow through BootstrapOnly, so neither path can silently
		// swap to DevAuthorizer without the warn.
		if got := strings.Count(rendered, "DEV MODE — authorization checks disabled"); got != 1 {
			t.Errorf("expected exactly one dev-mode Warn site (BootstrapOnly, shared via the Bootstrap delegate), found %d occurrence(s)", got)
		}
		// Gate must match the devMode local exactly — emitting unconditionally
		// would print at every prod boot too, which neuters the signal.
		if !strings.Contains(rendered, "if devMode {") {
			t.Error("dev-mode banner must be gated on `if devMode {`")
		}
		if !strings.Contains(rendered, `"environment", cfg.Environment`) {
			t.Error("dev-mode banner should attach the actual environment value so operators can see what triggered the swap")
		}

		fset := token.NewFileSet()
		if _, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors); parseErr != nil {
			t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
		}
	})

	t.Run("zero-services-suppresses-banner", func(t *testing.T) {
		data := mkData(nil)
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		rendered := string(content)
		// No services → no DevAuthorizer swap → no banner. Emitting it
		// here would force an unused `logger` reference and surface a
		// misleading warning in CLI-only projects.
		if strings.Contains(rendered, "DEV MODE — authorization checks disabled") {
			t.Error("zero-service project must not emit the dev-mode banner (no Authorizer to swap)")
		}
	})
}

// TestBootstrapTemplate_DiagnosticsEmitWhenEnabled asserts that the
// `features.diagnostics: true` flag is the only thing that wires
// diagnostics into the rendered bootstrap. Since the 2026-06 appkit
// table migration the emit is a DATA FIELD on the def table
// (`Diagnostics: appkit.DiagnosticsLog/Strict`) — the Boot call and
// the pkg/diagnostics import live in appkit.Run. Off → no row.
//
// Existing projects without the flag keep their pre-diagnostics
// bootstrap shape (no regression), which is the central promise of
// the opt-in design.
func TestBootstrapTemplate_DiagnosticsEmitWhenEnabled(t *testing.T) {
	type svc struct {
		Name, Package, ImportPath, FieldName, Alias string
		Fallible, HasWebhooks                       bool
		ConnectPkg, ProtoServiceName                string
	}
	mkData := func(diagnostics, strict bool) any {
		return struct {
			Module              string
			HasDatabase         bool
			DatabaseDriver      string
			OrmEnabled          bool
			Services            []svc
			Packages            []struct{}
			Workers             []struct{}
			Operators           []struct{}
			ConfigFields        map[string]bool
			RESTEnabled         bool
			ConnectImports      []string
			DiagnosticsEnabled  bool
			StrictWiringEnabled bool
			AllServiceNames     []string
		}{
			Module: "example.com/myproject",
			Services: []svc{
				{Name: "api", Package: "api", ImportPath: "api", FieldName: "API", Alias: "api"},
			},
			ConfigFields:        map[string]bool{},
			DiagnosticsEnabled:  diagnostics,
			StrictWiringEnabled: strict,
		}
	}

	render := func(t *testing.T, data any) string {
		t.Helper()
		content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
		if err != nil {
			t.Fatalf("Render bootstrap.go.tmpl: %v", err)
		}
		return string(content)
	}

	t.Run("off-by-default", func(t *testing.T) {
		rendered := render(t, mkData(false, false))
		// Diagnostics-related symbols must NOT appear when the feature is
		// off — guards the additive default-off contract.
		if strings.Contains(rendered, "Diagnostics: appkit.Diagnostics") {
			t.Error("Diagnostics def row should not be emitted when DiagnosticsEnabled=false")
		}
		if strings.Contains(rendered, "pkg/diagnostics") {
			t.Error("pkg/diagnostics import should not be emitted when DiagnosticsEnabled=false")
		}
	})

	t.Run("on", func(t *testing.T) {
		rendered := render(t, mkData(true, false))
		if !strings.Contains(rendered, "Diagnostics: appkit.DiagnosticsLog,") {
			t.Errorf("expected Diagnostics: appkit.DiagnosticsLog row when DiagnosticsEnabled=true\n--- rendered ---\n%s", rendered)
		}
		// Plain log mode (no strict escalation) when strict is off.
		if strings.Contains(rendered, "DiagnosticsStrict") {
			t.Error("DiagnosticsStrict should not appear when StrictWiringEnabled=false")
		}
	})

	t.Run("strict", func(t *testing.T) {
		rendered := render(t, mkData(true, true))
		if !strings.Contains(rendered, "Diagnostics: appkit.DiagnosticsStrict,") {
			t.Errorf("expected Diagnostics: appkit.DiagnosticsStrict row when StrictWiringEnabled=true\n--- rendered ---\n%s", rendered)
		}
	})
}

// TestBootstrapTestingTemplate_ZeroServices verifies that bootstrap_testing.go.tmpl
// renders valid Go when all component lists are empty.
func TestBootstrapTestingTemplate_ZeroServices(t *testing.T) {
	data := struct {
		Module   string
		HasDatabase         bool
		DatabaseDriver      string
		OrmEnabled          bool
		Services []struct {
			Name, Package, FieldName, ProtoServiceName string
			ProtoConnectImportPath, ProtoConnectPkg    string
			Fallible                                   bool
			HasDB                                      bool
		}
		ConnectImports []string
		Packages       []struct {
			Name, Package, FieldName string
			Fallible                 bool
		}
		MultiTenantEnabled bool
		AnyServiceHasDB    bool
		// ExtraImports lists cross-package auto-stub imports. The
		// template ranges over it unconditionally; we declare the
		// field as an empty slice so text/template's struct-field
		// lookup succeeds. The element type only needs the same
		// .Alias / .Path fields the production type carries.
		ExtraImports []struct {
			Alias, Path string
		}
	}{
		Module: "example.com/myproject",
	}

	content, err := ProjectTemplates().Render("bootstrap_testing.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap_testing.go.tmpl: %v", err)
	}

	rendered := string(content)

	// Must contain basic test helpers
	if !strings.Contains(rendered, "type TestOption func") {
		t.Fatal("missing TestOption type")
	}
	// With zero services and zero packages, defaultTestConfig is dead code
	// (it's only called from per-service / per-package NewTest helpers, which
	// aren't emitted). Emitting it would also force a `"testing"` import that
	// goes unused, breaking compilation. Confirm it is suppressed.
	if strings.Contains(rendered, "func defaultTestConfig(") {
		t.Fatal("defaultTestConfig should be suppressed when no services and no packages exist")
	}
	if strings.Contains(rendered, `"testing"`) {
		t.Fatal("\"testing\" import should be suppressed when no services and no packages exist")
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "testing.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered testing.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}

// TestEntityExampleProto_HyphenatedPackageIsSnakeCased is a regression test
// for the stripe-latent bug: project names with hyphens (e.g. "my-app")
// would render as `package my-app.db.v1;` which is invalid proto. The fix
// runs {{.Package}} through the snakeCase template func.
func TestEntityExampleProto_HyphenatedPackageIsSnakeCased(t *testing.T) {
	out, err := ProjectTemplates().Render("entity-example.proto.tmpl", map[string]any{
		"Package": "my-app",
	})
	if err != nil {
		t.Fatalf("render entity-example.proto.tmpl: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "package my_app.db.v1;") {
		t.Errorf("expected 'package my_app.db.v1;' in rendered proto, got:\n%s", rendered)
	}
	// Inspect non-comment lines only — the file's own TODO/comment text is
	// allowed to mention "package my-app.db.v1" inside a quoted explanation.
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "package ") && strings.Contains(trimmed, "my-app") {
			t.Errorf("hyphenated package decl leaked through; snakeCase filter not applied: %q", trimmed)
		}
	}
}

// TestDBReadme_HyphenatedPackageIsSnakeCased covers the same stripe-latent
// bug in the markdown README example, which is rendered into proto/db/README.md
// at scaffold time (so consumers may copy-paste it into a real proto file).
func TestDBReadme_HyphenatedPackageIsSnakeCased(t *testing.T) {
	out, err := ProjectTemplates().Render("db-README.md.tmpl", map[string]any{
		"Package": "my-app",
	})
	if err != nil {
		t.Fatalf("render db-README.md.tmpl: %v", err)
	}
	rendered := string(out)
	if !strings.Contains(rendered, "package my_app.db.v1;") {
		t.Errorf("expected 'package my_app.db.v1;' in rendered markdown, got:\n%s", rendered)
	}
	// Inspect non-comment lines only (HTML comments in markdown / .proto-style
	// // comments) — the file's own TODO/comment text may mention the bad form.
	for _, line := range strings.Split(rendered, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "<!--") {
			continue
		}
		if strings.HasPrefix(trimmed, "package ") && strings.Contains(trimmed, "my-app") {
			t.Errorf("hyphenated package decl leaked through; snakeCase filter not applied: %q", trimmed)
		}
	}
}

// TestDockerfile_LocalForgePkgVendoredCopyLine verifies that the Dockerfile
// template emits the `COPY .forge-pkg/ ./.forge-pkg/` line if and only if
// the LocalForgePkgVendored flag is true. This is the load-bearing toggle
// for the dev-mode local-replace workaround: it must be off in the
// canonical (published-forge/pkg) shape and on when forge generate has
// vendored a sibling forge checkout.
func TestDockerfile_LocalForgePkgVendoredCopyLine(t *testing.T) {
	type tc struct {
		name        string
		vendored    bool
		wantContain string
		wantAbsent  string
	}
	cases := []tc{
		{
			name:       "vendored=false omits COPY .forge-pkg",
			vendored:   false,
			wantAbsent: "COPY .forge-pkg/",
		},
		{
			name:        "vendored=true emits COPY .forge-pkg",
			vendored:    true,
			wantContain: "COPY .forge-pkg/ ./.forge-pkg/",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			data := struct {
				Name                   string
				ProtoName              string
				Module                 string
				HasDatabase         bool
				DatabaseDriver      string
				OrmEnabled          bool
				ServiceName            string
				ServicePort            int
				ProjectName            string
				FrontendName           string
				FrontendPort           int
				GoVersion              string
				GoVersionMinor         string
				DockerBuilderGoVersion string
				LocalForgePkgVendored  bool
			}{
				Name: "demo", ProtoName: "demo", Module: "github.com/example/demo",
				ServiceName: "api", ServicePort: 8080, ProjectName: "demo",
				GoVersion: "1.26", GoVersionMinor: "26", DockerBuilderGoVersion: "1.26",
				LocalForgePkgVendored: c.vendored,
			}
			out, err := ProjectTemplates().Render("Dockerfile.tmpl", data)
			if err != nil {
				t.Fatalf("render Dockerfile.tmpl: %v", err)
			}
			rendered := string(out)
			if c.wantContain != "" && !strings.Contains(rendered, c.wantContain) {
				t.Errorf("expected %q in rendered Dockerfile, got:\n%s", c.wantContain, rendered)
			}
			if c.wantAbsent != "" && strings.Contains(rendered, c.wantAbsent) {
				t.Errorf("did not expect %q in rendered Dockerfile, got:\n%s", c.wantAbsent, rendered)
			}
		})
	}
}

// TestCmdServerTemplate_WiresPostBootstrapHook verifies the generated
// cmd/server.go wires the user-owned PostBootstrap hook into
// serverkit.Hooks.PostBootstrap. This is the chokepoint the user code
// relies on; if the wiring disappears from the template, projects that
// register post-construct collaborators will silently no-op.
//
// The error-propagation contract ("post-bootstrap hook failed: ...")
// now lives in serverkit.Run (see pkg/serverkit/run.go); the shim only
// needs to forward the typed *app.App to the hook.
func TestCmdServerTemplate_WiresPostBootstrapHook(t *testing.T) {
	data := struct {
		Module       string
		HasDatabase         bool
		DatabaseDriver      string
		OrmEnabled          bool
		ConfigFields map[string]bool
	}{
		Module:       "example.com/myproject",
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("cmd-server.go.tmpl", data)
	if err != nil {
		t.Fatalf("render cmd-server.go.tmpl: %v", err)
	}
	rendered := string(content)

	if !strings.Contains(rendered, "PostBootstrap:") {
		t.Errorf("cmd-server.go.tmpl must set serverkit.Hooks.PostBootstrap; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "app.PostBootstrap(a.(*app.App))") {
		t.Errorf("cmd-server.go.tmpl must forward the Application to app.PostBootstrap via type assertion; rendered output:\n%s", rendered)
	}

	// Verify it still parses as valid Go.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "server.go", rendered, parser.AllErrors); perr != nil {
		t.Fatalf("rendered server.go does not parse:\n%v\n\nSource:\n%s", perr, rendered)
	}
}

// TestPostBootstrapTemplate_ScaffoldsNoOp verifies the user-owned
// post_bootstrap.go scaffold renders as valid Go with the expected
// signature `func PostBootstrap(app *App) error`. The default body is
// a no-op the user replaces; if the signature drifts, every project
// that owns the file will fail to compile when cmd/server.go's
// generated call site updates underneath it.
func TestPostBootstrapTemplate_ScaffoldsNoOp(t *testing.T) {
	content, err := ProjectTemplates().Render("post_bootstrap.go.tmpl", struct{}{})
	if err != nil {
		t.Fatalf("render post_bootstrap.go.tmpl: %v", err)
	}
	rendered := string(content)

	if !strings.Contains(rendered, "func PostBootstrap(app *App) error") {
		t.Errorf("post_bootstrap.go.tmpl must declare `func PostBootstrap(app *App) error`; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "return nil") {
		t.Errorf("post_bootstrap.go.tmpl default body must `return nil` (no-op); got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "//forge:allow") {
		t.Errorf("post_bootstrap.go.tmpl must carry //forge:allow so the audit walker treats it as user-owned")
	}

	// Verify it parses as valid Go.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "post_bootstrap.go", rendered, parser.AllErrors); perr != nil {
		t.Fatalf("rendered post_bootstrap.go does not parse:\n%v\n\nSource:\n%s", perr, rendered)
	}
}
