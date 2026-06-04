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
		HasDatabase    bool
		OrmEnabled     bool
		HasFallible    bool
		BinaryShared   bool
		ConfigFields   map[string]bool
		RESTEnabled    bool
		ConnectImports []string
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
		Module         string
		Services       []svc
		Packages       []struct{}
		Workers        []struct{}
		Operators      []struct{}
		ConfigFields   map[string]bool
		RESTEnabled    bool
		ConnectImports []string
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

// TestCmdServerTemplate_CallsPostBootstrap verifies the generated
// cmd/server.go invokes the user-owned PostBootstrap hook after
// Bootstrap returns and propagates any error as a fatal boot failure.
// This is the chokepoint the post-bootstrap user code relies on; if
// the call disappears from the template, projects that wire
// post-construct collaborators here will silently no-op.
func TestCmdServerTemplate_CallsPostBootstrap(t *testing.T) {
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

	if !strings.Contains(rendered, "app.PostBootstrap(application)") {
		t.Errorf("cmd-server.go.tmpl must call app.PostBootstrap(application); rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "post-bootstrap hook failed") {
		t.Errorf("cmd-server.go.tmpl must propagate post-bootstrap errors with a 'post-bootstrap hook failed' message")
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
