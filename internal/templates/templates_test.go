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
		HasDatabase  bool
		OrmEnabled   bool
		HasFallible  bool
		BinaryShared bool
		ConfigFields map[string]bool
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

	// Verify it parses as valid Go
	fset := token.NewFileSet()
	_, parseErr := parser.ParseFile(fset, "bootstrap.go", rendered, parser.AllErrors)
	if parseErr != nil {
		t.Fatalf("rendered bootstrap.go does not parse as valid Go:\n%v\n\nSource:\n%s", parseErr, rendered)
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
