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

	// Must contain key functions. BootstrapOnly (the string name filter)
	// is retired (FORGE_SHAPE_REDESIGN §2): selection moved to the cmd
	// layer over internal/app.Inventory.
	if !strings.Contains(rendered, "func Bootstrap(") {
		t.Fatal("missing Bootstrap function")
	}
	if strings.Contains(rendered, "func BootstrapOnly(") {
		t.Fatal("BootstrapOnly should be retired — string-keyed selection moved to the cmd layer")
	}
	if !strings.Contains(rendered, "func (a *App) Shutdown(") {
		t.Fatal("missing Shutdown method")
	}

	// Must NOT contain service-specific imports
	if strings.Contains(rendered, "pkg/middleware") {
		t.Fatal("zero-service bootstrap should not import middleware")
	}

	// Regression for forge-new-empty-services-unused-locals (originally
	// the runAll declaration, then devMode after the 2026-06 appkit
	// table migration, retired entirely with the M6 cmd-as-code rework):
	// dev mode is read from cfg.Mode().IsDev() at each consumer, so the
	// bootstrap must not declare ANY local that only service closures
	// would consume. go/parser doesn't flag unused locals so the
	// ParseFile check below cannot catch this on its own; guard the
	// literal declaration string instead.
	if strings.Contains(rendered, "devMode := ") {
		t.Fatal("bootstrap must not declare devMode — the devMode parameter threading was removed; consumers read cfg.Mode().IsDev() directly")
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

	// devMode threading is gone (M6 cmd-as-code): wireXxxDeps reads
	// cfg.Mode().IsDev() itself, so bootstrap must not declare a local.
	if strings.Contains(rendered, "devMode := ") {
		t.Fatal("bootstrap must not declare devMode — wireXxxDeps reads cfg.Mode().IsDev() directly")
	}
	// Since the registration-in-code rework, the per-service Construct
	// closures live in services_gen.go; bootstrap consumes the row list
	// via the user-owned RegisteredServices.
	if !strings.Contains(rendered, "RegisteredServices(app, cfg, logger, opts...)") {
		t.Fatal("bootstrap with services must consume the user-owned RegisteredServices row list")
	}
	// String-keyed selection retired (§2): appkit.Run is filter-free.
	if !strings.Contains(rendered, "appkit.Run(def, mux, logger)") {
		t.Fatal("bootstrap must call the filter-free appkit.Run(def, mux, logger)")
	}
	if strings.Contains(rendered, "appkit.Options") {
		t.Fatal("appkit.Options (the string filter) should be retired")
	}
}

// TestBootstrapTemplate_ConstructsEveryComponentRow pins that the def
// table carries a row for every service, worker, and operator. String-keyed
// selection is retired (FORGE_SHAPE_REDESIGN §2): the old loud-filter banner
// is gone (appkit.Run constructs+mounts everything), but the table must
// still contribute a row per component kind so the binary serves them all.
func TestBootstrapTemplate_ConstructsEveryComponentRow(t *testing.T) {
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

	// Every component kind must contribute a row to the def table. Service
	// rows come from the user-owned RegisteredServices (registration-in-
	// code); worker/operator rows stay inline.
	for _, name := range []string{`Services: RegisteredServices(app, cfg, logger, opts...)`, `{Name: "indexer", Construct: func() error {`, `{Name: "scaler", Construct: func() error {`} {
		if !strings.Contains(rendered, name) {
			t.Errorf("bootstrap def table missing row %s", name)
		}
	}

	// appkit.Run is filter-free (string-keyed selection retired).
	if !strings.Contains(rendered, "appkit.Run(def, mux, logger)") {
		t.Error("bootstrap must call the filter-free appkit.Run(def, mux, logger)")
	}
	if strings.Contains(rendered, "server filter active") || strings.Contains(rendered, "appkit.Options") {
		t.Error("the string filter / loud banner is retired and must not appear")
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
// the banner is gated on .Services.
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
		if got := strings.Count(rendered, "AUTH BYPASS — authorization checks disabled"); got != 1 {
			t.Errorf("expected exactly one auth-bypass Warn site (BootstrapOnly, shared via the Bootstrap delegate), found %d occurrence(s)", got)
		}
		// Gate must match the wireXxxDeps swap condition exactly
		// (cfg.DevAuthBypass() — dev mode AND AUTH_DEV_MODE, NOT IsDev); the
		// banner must never fire when authz is actually still enforced.
		if !strings.Contains(rendered, "if cfg.DevAuthBypass() {") {
			t.Error("auth-bypass banner must be gated on `if cfg.DevAuthBypass() {`")
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
		if strings.Contains(rendered, "AUTH BYPASS — authorization checks disabled") {
			t.Error("zero-service project must not emit the auth-bypass banner (no Authorizer to swap)")
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
		Module         string
		HasDatabase    bool
		DatabaseDriver string
		OrmEnabled     bool
		Services       []struct {
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

	// The body uses *slog.Logger unconditionally (testConfig.logger,
	// WithLogger), so the import must be present even in the
	// zero-component state. Journey fr-994db53964: zero-service
	// pkg/app/testing.go failed to compile with `undefined: slog`
	// because the import was gated on `or .Services .Packages` while
	// the symbols were not.
	if strings.Contains(rendered, "slog.") && !strings.Contains(rendered, `"log/slog"`) {
		t.Fatalf("rendered testing.go references slog without importing log/slog:\n%s", rendered)
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "testing.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered testing.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}

	// Belt-and-braces for the same class of bug on every qualifier the
	// template can emit: any package qualifier used in the body must
	// have a matching import. Parse-level only (no type checking), but
	// it catches gated-import-vs-unconditional-symbol drift for all
	// branches of the template, not just slog.
	assertQualifiersImported(t, rendered)
}

// assertQualifiersImported parses a rendered Go file and asserts that
// every known package qualifier referenced in the body has a matching
// import path. The qualifier→path table covers the packages
// bootstrap_testing.go.tmpl can emit; extend it when the template grows
// a new import.
func assertQualifiersImported(t *testing.T, src string) {
	t.Helper()
	qualifierImports := map[string]string{
		"slog":     "log/slog",
		"testing":  "testing",
		"context":  "context",
		"http":     "net/http",
		"httptest": "net/http/httptest",
		"connect":  "connectrpc.com/connect",
		"orm":      "github.com/reliant-labs/forge/pkg/orm",
		"testkit":  "github.com/reliant-labs/forge/pkg/testkit",
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "testing.go", src, parser.AllErrors)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	imported := map[string]bool{}
	for _, imp := range f.Imports {
		imported[strings.Trim(imp.Path.Value, `"`)] = true
	}
	for qual, path := range qualifierImports {
		if strings.Contains(src, qual+".") && !imported[path] {
			t.Errorf("rendered file references %s.* without importing %q", qual, path)
		}
	}
}

// TestDBReadme_HyphenatedPackageIsSnakeCased covers the same stripe-latent
// bug in the markdown README example, which is rendered into proto/db/README.md
// at scaffold time (so consumers may copy-paste it into a real proto file).
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
				HasDatabase            bool
				DatabaseDriver         string
				OrmEnabled             bool
				ServiceName            string
				ServicePort            int
				ProjectName            string
				FrontendName           string
				FrontendPort           int
				GoVersion              string
				GoVersionMinor         string
				DockerBuilderGoVersion string
				LocalForgePkgVendored  bool
				VersionVar             string
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

// TestDockerfile_VersionVarLdflags verifies the Dockerfile's build-stage
// ldflags emit the extra `-X <path>=${FORGE_VERSION}` when build.version_var
// is set, and omit it (leaving the canonical main.version/commit/date
// stamping) when it is empty. Also pins that the in-container `git describe`
// is gone — replaced by the FORGE_VERSION build-arg.
func TestDockerfile_VersionVarLdflags(t *testing.T) {
	type tc struct {
		name        string
		versionVar  string
		wantContain string
		wantAbsent  string
	}
	cases := []tc{
		{
			name:       "unset omits extra -X target",
			versionVar: "",
			wantAbsent: "-X github.com/acme/app/internal/buildinfo.Version=${FORGE_VERSION}",
		},
		{
			name:        "set emits extra -X target with FORGE_VERSION value",
			versionVar:  "github.com/acme/app/internal/buildinfo.Version",
			wantContain: "-X github.com/acme/app/internal/buildinfo.Version=${FORGE_VERSION}",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			data := struct {
				Name                   string
				ProtoName              string
				Module                 string
				ServiceName            string
				ServicePort            int
				ProjectName            string
				FrontendName           string
				FrontendPort           int
				GoVersion              string
				GoVersionMinor         string
				DockerBuilderGoVersion string
				LocalForgePkgVendored  bool
				VersionVar             string
			}{
				Name: "demo", ProtoName: "demo", Module: "github.com/example/demo",
				ServiceName: "api", ServicePort: 8080, ProjectName: "demo",
				GoVersion: "1.26", GoVersionMinor: "26", DockerBuilderGoVersion: "1.26",
				VersionVar: c.versionVar,
			}
			out, err := ProjectTemplates().Render("Dockerfile.tmpl", data)
			if err != nil {
				t.Fatalf("render Dockerfile.tmpl: %v", err)
			}
			rendered := string(out)
			// The canonical main.version/commit/date stamping is always present.
			for _, want := range []string{
				"-X main.version=${FORGE_VERSION}",
				"-X main.commit=${FORGE_COMMIT}",
				"-X main.date=${FORGE_DATE}",
				"ARG FORGE_VERSION=dev",
			} {
				if !strings.Contains(rendered, want) {
					t.Errorf("expected %q in rendered Dockerfile, got:\n%s", want, rendered)
				}
			}
			// The old in-container git-describe invocation in -ldflags must
			// be gone (the prose comment may still mention it).
			if strings.Contains(rendered, "$(git describe") {
				t.Errorf("expected in-container `$(git describe ...)` ldflags to be removed, got:\n%s", rendered)
			}
			if c.wantContain != "" && !strings.Contains(rendered, c.wantContain) {
				t.Errorf("expected %q in rendered Dockerfile, got:\n%s", c.wantContain, rendered)
			}
			if c.wantAbsent != "" && strings.Contains(rendered, c.wantAbsent) {
				t.Errorf("did not expect %q in rendered Dockerfile, got:\n%s", c.wantAbsent, rendered)
			}
		})
	}
}

// TestCmdServerTemplate_ComposesServer verifies the generated
// cmd/server.go is the §2 hybrid-DI COMPOSITION SITE: open the owned infra
// (app.OpenInfra), run the generated injector (app.Build), run the owned
// two-phase wiring (app.PostBuild), mount the selected services over the
// data-only app.Inventory, select workers/operators from the constructed
// *Services, and pack the finished pieces into serverkit.Server before
// serverkit.Run. If any of these wirings disappear, generated projects
// regress.
func TestCmdServerTemplate_ComposesServer(t *testing.T) {
	data := struct {
		Module               string
		HasDatabase          bool
		DatabaseDriver       string
		OrmEnabled           bool
		ConfigFields         map[string]bool
		AuthProvider         string
		AuthProviderExternal bool
		RESTEnabled          bool
	}{
		Module:       "example.com/myproject",
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("cmd-server.go.tmpl", data)
	if err != nil {
		t.Fatalf("render cmd-server.go.tmpl: %v", err)
	}
	rendered := string(content)

	// Hybrid DI: OpenInfra → Build → PostBuild over internal/app.
	for _, want := range []string{
		"app.OpenInfra(ctx, cfg, logger)",
		"app.Build(infra)",
		"app.PostBuild(services)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("cmd-server.go.tmpl must call %q (§2 hybrid DI); rendered output:\n%s", want, rendered)
		}
	}
	// The old appkit Bootstrap path is gone.
	for _, gone := range []string{"app.BootstrapOnly(", "app.Bootstrap(", "app.PostBootstrap(", "theApp.RESTHandler()", "serverkit.Hooks"} {
		if strings.Contains(rendered, gone) {
			t.Errorf("cmd-server.go.tmpl must NOT reference the retired %q; rendered output:\n%s", gone, rendered)
		}
	}
	// Mount selection over the data-only inventory + worker/operator
	// selection over the constructed *Services.
	if !strings.Contains(rendered, "mountServices(services, mux, cfg, logger, args, opts...)") {
		t.Errorf("cmd-server.go.tmpl must mount selected services over app.Inventory; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "selectWorkers(app.WorkerList(services), args)") {
		t.Errorf("cmd-server.go.tmpl must select workers from the constructed *Services; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "selectOperators(app.OperatorList(services), args)") {
		t.Errorf("cmd-server.go.tmpl must select operators from the constructed *Services; rendered output:\n%s", rendered)
	}
	// Composed Server + RunOperators (over services) + OnShutdown.
	if !strings.Contains(rendered, "serverkit.Run(ctx, skCfg, serverkit.Server{") {
		t.Errorf("cmd-server.go.tmpl must call serverkit.Run with a composed serverkit.Server; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "app.RunOperators(services, ctx, logger, healthProbeAddr)") {
		t.Errorf("cmd-server.go.tmpl must pass app.RunOperators(services, ...) into Server; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "OnShutdown:") {
		t.Errorf("cmd-server.go.tmpl must compose Server.OnShutdown (otel flush); rendered output:\n%s", rendered)
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
