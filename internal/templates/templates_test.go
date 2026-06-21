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
			HasAuthorizer                              bool
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
				Binaries               []struct {
					Dir     string
					Primary bool
				}
			}{
				Name: "demo", ProtoName: "demo", Module: "github.com/example/demo",
				ServiceName: "api", ServicePort: 8080, ProjectName: "demo",
				GoVersion: "1.26", GoVersionMinor: "26", DockerBuilderGoVersion: "1.26",
				LocalForgePkgVendored: c.vendored,
				Binaries: []struct {
					Dir     string
					Primary bool
				}{{Dir: "demo", Primary: true}},
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
				Binaries               []struct {
					Dir     string
					Primary bool
				}
			}{
				Name: "demo", ProtoName: "demo", Module: "github.com/example/demo",
				ServiceName: "api", ServicePort: 8080, ProjectName: "demo",
				GoVersion: "1.26", GoVersionMinor: "26", DockerBuilderGoVersion: "1.26",
				VersionVar: c.versionVar,
				Binaries: []struct {
					Dir     string
					Primary bool
				}{{Dir: "demo", Primary: true}},
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
// internal/cli/serve.go is the §2 hybrid-DI SHARED SERVE PIPELINE: open the
// owned infra (app.OpenInfra), run the generated injector (app.Build), run
// the owned two-phase wiring (app.PostBuild), apply a TYPED mount FUNCTION
// (no string selection, no inventory lookup on the run path), and pack the
// finished pieces into serverkit.Server before serverkit.Run. serverkit now
// OWNS OTel, so serve must project OTLPEndpoint + ServiceName onto skCfg and
// must NOT build an otel-shutdown closure of its own.
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
		ConfigFields: map[string]bool{"OtlpEndpoint": true},
	}

	content, err := ProjectTemplates().Render("cmd-tree-serve.go.tmpl", data)
	if err != nil {
		t.Fatalf("render cmd-tree-serve.go.tmpl: %v", err)
	}
	rendered := string(content)

	// Hybrid DI: OpenInfra → Build → PostBuild over internal/app.
	for _, want := range []string{
		"app.OpenInfra(ctx, cfg, logger)",
		"app.Build(infra)",
		"app.PostBuild(services)",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("cmd-tree-serve.go.tmpl must call %q (§2 hybrid DI); rendered output:\n%s", want, rendered)
		}
	}
	// The old appkit Bootstrap path AND the string-selection mount path are
	// gone. serverkit owns OTel, so the cmd no longer composes an otel flush.
	for _, gone := range []string{
		"app.BootstrapOnly(", "app.Bootstrap(", "serverkit.Hooks",
		"mountServices(", "app.Inventory", "setupOTel(", "otelShutdown",
		`runServer(cmd, []string{`,
	} {
		if strings.Contains(rendered, gone) {
			t.Errorf("cmd-tree-serve.go.tmpl must NOT reference the retired %q; rendered output:\n%s", gone, rendered)
		}
	}
	// TYPED mount: serve takes a typed mount FUNCTION (method expression),
	// not a string. The function value is applied to the constructed graph.
	if !strings.Contains(rendered, "mount(services, mux, cfg, logger, opts...)") {
		t.Errorf("cmd-tree-serve.go.tmpl must apply the typed mount func to the constructed *Services; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "mount MountFunc") {
		t.Errorf("cmd-tree-serve.go.tmpl serve() must take a typed mountFunc; rendered output:\n%s", rendered)
	}
	// serverkit owns OTel — serve projects OTLPEndpoint + ServiceName.
	if !strings.Contains(rendered, "ServiceName: ServiceName") {
		t.Errorf("cmd-tree-serve.go.tmpl must project the app-identity ServiceName onto skCfg; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "OTLPEndpoint: cfg.OtlpEndpoint") {
		t.Errorf("cmd-tree-serve.go.tmpl must project cfg.OtlpEndpoint onto skCfg; rendered output:\n%s", rendered)
	}
	// Composed Server + RunOperators (over services).
	if !strings.Contains(rendered, "serverkit.Run(ctx, skCfg, serverkit.Server{") {
		t.Errorf("cmd-tree-serve.go.tmpl must call serverkit.Run with a composed serverkit.Server; rendered output:\n%s", rendered)
	}
	if !strings.Contains(rendered, "app.RunOperators(services, ctx, logger, healthProbeAddr)") {
		t.Errorf("cmd-tree-serve.go.tmpl must pass app.RunOperators(services, ...) into Server; rendered output:\n%s", rendered)
	}

	// Verify it still parses as valid Go.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "serve.go", rendered, parser.AllErrors); perr != nil {
		t.Fatalf("rendered serve.go does not parse:\n%v\n\nSource:\n%s", perr, rendered)
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
