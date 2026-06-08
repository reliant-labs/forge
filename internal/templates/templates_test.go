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
		OrmEnabled          bool
		HasFallible         bool
		BinaryShared        bool
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
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

	// Regression for forge-new-empty-services-unused-runAll: BootstrapOnly
	// declares `runAll := len(names) == 0` purely to feed the per-service
	// `if runAll || nameSet["<svc>"]` mux-registration guard. When there are
	// no services, that guard is never emitted, and `runAll :=` becomes a
	// "declared and not used" compile error. go/parser doesn't flag unused
	// locals so the ParseFile check below cannot catch this on its own;
	// guard the literal declaration string instead.
	if strings.Contains(rendered, "runAll := ") {
		t.Fatal("zero-service bootstrap must not declare runAll (no consumer block emitted; would fail 'declared and not used')")
	}

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
	}
}

// TestBootstrapTemplate_WithServicesStillDeclaresRunAll guards the other side
// of the forge-new-empty-services-unused-runAll fix: when services ARE
// configured, the per-service mux-registration loop relies on `runAll` to
// implement the "empty names slice == mount everything" identity used by
// `./<bin> server`. The empty-case fix must not regress that path.
func TestBootstrapTemplate_WithServicesStillDeclaresRunAll(t *testing.T) {
	type svc struct {
		Name, Package, FieldName, Alias string
		Fallible, HasWebhooks           bool
		ConnectPkg, ProtoServiceName    string
	}
	data := struct {
		Module              string
		Services            []svc
		Packages            []struct{}
		Workers             []struct{}
		Operators           []struct{}
		ConfigFields        map[string]bool
		RESTEnabled         bool
		ConnectImports      []string
		DiagnosticsEnabled  bool
		StrictWiringEnabled bool
	}{
		Module: "example.com/myproject",
		Services: []svc{
			{Name: "api", Package: "api", FieldName: "API", Alias: "apihandler"},
		},
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("bootstrap.go.tmpl", data)
	if err != nil {
		t.Fatalf("Render bootstrap.go.tmpl: %v", err)
	}
	rendered := string(content)

	if !strings.Contains(rendered, "runAll := len(names) == 0") {
		t.Fatal("bootstrap with services must declare runAll for the per-service mux-registration guard")
	}
	if !strings.Contains(rendered, "if runAll || nameSet[\"api\"]") {
		t.Fatal("bootstrap with services must consume runAll in the per-service registration guard")
	}
}

// TestBootstrapTemplate_DiagnosticsEmitWhenEnabled asserts that the
// `features.diagnostics: true` flag is the only thing that wires
// diagnostics.Default.Boot into the rendered bootstrap. Off → no
// import, no call. On + strict_wiring → StrictEmitter wrap.
//
// Existing projects without the flag keep their pre-diagnostics
// bootstrap byte-for-byte (no regression), which is the central
// promise of the opt-in design.
func TestBootstrapTemplate_DiagnosticsEmitWhenEnabled(t *testing.T) {
	type svc struct {
		Name, Package, FieldName, Alias string
		Fallible, HasWebhooks           bool
		ConnectPkg, ProtoServiceName    string
	}
	mkData := func(diagnostics, strict bool) any {
		return struct {
			Module              string
			Services            []svc
			Packages            []struct{}
			Workers             []struct{}
			Operators           []struct{}
			ConfigFields        map[string]bool
			RESTEnabled         bool
			ConnectImports      []string
			DiagnosticsEnabled  bool
			StrictWiringEnabled bool
		}{
			Module: "example.com/myproject",
			Services: []svc{
				{Name: "api", Package: "api", FieldName: "API", Alias: "api"},
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
		if strings.Contains(rendered, "diagnostics.Default.Boot") {
			t.Error("diagnostics.Default.Boot should not be emitted when DiagnosticsEnabled=false")
		}
		if strings.Contains(rendered, "pkg/diagnostics") {
			t.Error("pkg/diagnostics import should not be emitted when DiagnosticsEnabled=false")
		}
	})

	t.Run("on", func(t *testing.T) {
		rendered := render(t, mkData(true, false))
		if !strings.Contains(rendered, "diagnostics.Default.Boot(diagnostics.NewLogEmitter(logger))") {
			t.Errorf("expected diagnostics.Default.Boot(NewLogEmitter) when DiagnosticsEnabled=true\n--- rendered ---\n%s", rendered)
		}
		if !strings.Contains(rendered, `"github.com/reliant-labs/forge/pkg/diagnostics"`) {
			t.Errorf("expected pkg/diagnostics import")
		}
		// Plain LogEmitter (no StrictEmitter wrap) when strict is off.
		if strings.Contains(rendered, "NewStrictEmitter") {
			t.Error("StrictEmitter should not appear when StrictWiringEnabled=false")
		}
	})

	t.Run("strict", func(t *testing.T) {
		rendered := render(t, mkData(true, true))
		if !strings.Contains(rendered, "diagnostics.Default.Boot(diagnostics.NewStrictEmitter(diagnostics.NewLogEmitter(logger)))") {
			t.Errorf("expected StrictEmitter wrap when StrictWiringEnabled=true\n--- rendered ---\n%s", rendered)
		}
	})
}

// TestBootstrapTestingTemplate_ZeroServices verifies that bootstrap_testing.go.tmpl
// renders valid Go when all component lists are empty.
func TestBootstrapTestingTemplate_ZeroServices(t *testing.T) {
	data := struct {
		Module   string
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
