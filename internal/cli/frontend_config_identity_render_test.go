package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// identityFrontendConfig is a frontend config message that declares the
// three OIDC fields, i.e. the surface the scaffolded frontend_config.proto
// ships.
func identityFrontendConfig() codegen.FrontendConfig {
	return codegen.FrontendConfig{
		Frontend:    "web",
		MessageName: "WebConfig",
		Fields: []codegen.ConfigField{
			{Name: "api_url", ProtoType: "string", EnvVar: "API_URL", DefaultValue: "http://localhost:8080"},
			{Name: "oidc_issuer", ProtoType: "string", EnvVar: "OIDC_ISSUER"},
			{Name: "oidc_client_id", ProtoType: "string", EnvVar: "OIDC_CLIENT_ID"},
			{Name: "oidc_redirect_uri", ProtoType: "string", EnvVar: "OIDC_REDIRECT_URI"},
		},
	}
}

// identityProject scaffolds the on-disk shape a dev env has once
// `forge generate` has run: the forge-owned frontend config module, a
// dev config.k whose frontend instance forge SCAFFOLDED (reading the
// published identity file), and the identity stub itself.
func identityProject(t *testing.T) string {
	t.Helper()
	projectDir := t.TempDir()
	kclDir := filepath.Join(projectDir, "deploy", "kcl")
	if err := os.MkdirAll(filepath.Join(kclDir, "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFrontendCfgFile(t, filepath.Join(kclDir, "kcl.mod"),
		"[package]\nname = \"deploy\"\nversion = \"0.0.1\"\n")

	fc := identityFrontendConfig()
	module, err := codegen.GenerateFrontendConfigKCL([]codegen.FrontendConfig{fc}, "acme")
	if err != nil {
		t.Fatalf("GenerateFrontendConfigKCL: %v", err)
	}
	writeFrontendCfgFile(t, filepath.Join(kclDir, codegen.FrontendConfigModule+".k"), module)

	// The env's config.k starts with only the backend half, exactly as the
	// write-if-absent scaffolder leaves it, and forge appends the frontend
	// instance — the code path under test.
	writeFrontendCfgFile(t, filepath.Join(kclDir, "dev", "config.k"),
		"app_config = {\n    log_level = \"debug\"\n}\n")
	if err := codegen.EnsureIDPIdentityStub(kclDir, "dev"); err != nil {
		t.Fatalf("EnsureIDPIdentityStub: %v", err)
	}
	if _, err := codegen.EnsureFrontendConfigInstances(
		[]codegen.FrontendConfig{fc}, kclDir, "dev", "acme", true, identityBackendFields()); err != nil {
		t.Fatalf("EnsureFrontendConfigInstances: %v", err)
	}
	return projectDir
}

// writeConvergedIdentity overwrites the committed identity file with what a
// successful idp-provision run would have left behind — the same file the
// pkg/devidp.KCLFilePublisher writes.
func writeConvergedIdentity(t *testing.T, projectDir, clientID string) {
	t.Helper()
	path := filepath.Join(projectDir, "deploy", "kcl", "dev", codegen.IDPIdentityModule+".k")
	body := "idp_identity = {\n" +
		"    \"client_id\" = \"" + clientID + "\"\n" +
		"    \"audience\" = \"generated-project-id\"\n" +
		"    \"issuer\" = \"http://localhost:8080\"\n" +
		"    \"jwks_url\" = \"http://localhost:8080/oauth/v2/keys\"\n" +
		"}\n"
	writeFrontendCfgFile(t, path, body)
}

// THE END-TO-END PROPERTY. What forge scaffolds into config.k must be valid
// KCL that renders through the real projection, reading whatever the
// idp-provision job most recently published — the file is forge-owned at
// the point it is written, so a shape that does not evaluate would break
// `forge generate` for every project that declares a frontend, not just
// fail a unit assertion.
func TestScaffoldedIdentityConfigKRenders(t *testing.T) {
	projectDir := identityProject(t)
	writeConvergedIdentity(t, projectDir, "resolved-client-id")

	values, err := loadFrontendRuntimeConfig(projectDir, "dev",
		[]codegen.FrontendConfig{identityFrontendConfig()})
	if err != nil {
		t.Fatalf("render the scaffolded config.k: %v", err)
	}

	web := values["web"]
	if web == nil {
		t.Fatalf("no runtime config projected for the frontend: %v", values)
	}
	if got := web["OIDC_CLIENT_ID"]; got != "resolved-client-id" {
		t.Errorf("OIDC_CLIENT_ID = %v, want the published id", got)
	}
	// The issuer rides the same file, so a render that read the id but
	// dropped the issuer would still leave sign-in broken.
	if got := web["OIDC_ISSUER"]; got != "http://localhost:8080" {
		t.Errorf("OIDC_ISSUER = %v", got)
	}
	// The redirect URI projects through EMPTY, and that is the un-pinning:
	// oidc-provider.ts then falls back to `<window.location.origin>/auth/
	// callback`, naming whatever dev port this run was assigned. A literal
	// here would re-pin the frontend's port.
	if got := web["OIDC_REDIRECT_URI"]; got != "" {
		t.Errorf("OIDC_REDIRECT_URI = %v, want empty so the browser names its own origin", got)
	}
}

// THE OFFLINE RENDER, end to end. Before the idp-provision job has ever
// run, the committed stub EnsureIDPIdentityStub seeds carries empty
// values, and the scaffolded config.k still renders — so `forge generate`
// on a fresh clone, or on a plane, produces a document rather than an
// error.
func TestScaffoldedIdentityConfigKRendersWithNoIdP(t *testing.T) {
	projectDir := identityProject(t)

	values, err := loadFrontendRuntimeConfig(projectDir, "dev",
		[]codegen.FrontendConfig{identityFrontendConfig()})
	if err != nil {
		t.Fatalf("an unconverged identity stub must not fail the render: %v", err)
	}
	if got := values["web"]["OIDC_CLIENT_ID"]; got != "" {
		t.Errorf("OIDC_CLIENT_ID = %v, want empty before idp-provision has ever run", got)
	}
}

// The generated config.k must IMPORT the identity module it reads. They
// are emitted by two different branches, and a file that reads
// idp.idp_identity without importing it is a KCL error at render time
// rather than a missing convenience.
func TestScaffoldedIdentityConfigKImportsTheIdentityModule(t *testing.T) {
	projectDir := identityProject(t)
	body, err := os.ReadFile(filepath.Join(projectDir, "deploy", "kcl", "dev", "config.k"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	if strings.Contains(src, "idp_identity[") && !strings.Contains(src, "import ."+codegen.IDPIdentityModule) {
		t.Errorf("config.k reads the identity module without importing it:\n%s", src)
	}
}

// identityBackendFields is the backend AppConfig's token-validation half, as
// a scaffolded config.proto declares it. The dev-identity binding resolves
// these field names by env var.
func identityBackendFields() []codegen.ConfigField {
	return []codegen.ConfigField{
		{Name: "jwt_issuer", ProtoType: "string", EnvVar: "JWT_ISSUER"},
		{Name: "jwt_audience", ProtoType: "string", EnvVar: "JWT_AUDIENCE"},
		{Name: "jwt_jwks_url", ProtoType: "string", EnvVar: "JWT_JWKS_URL"},
	}
}

// THE BACKEND HALF MUST RENDER TOO. The frontend block tells the browser
// where to sign in; this one tells the server what to accept. Both are
// written into a file forge owns at the moment it writes it, so a union that
// does not evaluate would break `forge generate` for every project with a
// frontend — which is exactly why this asserts through the real KCL renderer
// rather than on the emitted text.
func TestScaffoldedBackendIdentityRenders(t *testing.T) {
	projectDir := identityProject(t)
	writeConvergedIdentity(t, projectDir, "resolved-client-id")

	appConfig := renderAppConfig(t, projectDir, "dev")

	// The published issuer must reach the backend, or sign-in succeeds and
	// every RPC 401s on a token the server cannot verify.
	for field, want := range map[string]string{
		"jwt_issuer":   "http://localhost:8080",
		"jwt_jwks_url": "http://localhost:8080/oauth/v2/keys",
		"jwt_audience": "generated-project-id",
	} {
		if got, _ := appConfig[field].(string); got != want {
			t.Errorf("app_config.%s = %q, want %q", field, got, want)
		}
	}
}

// renderAppConfig evaluates the env's config.k and returns its app_config
// instance, using the same throwaway-probe technique as
// loadFrontendRuntimeConfig: a single-file module in the env dir, so only
// config.k and what it imports is pulled into the graph.
func renderAppConfig(t *testing.T, projectDir, env string) map[string]any {
	t.Helper()
	envDir := filepath.Join(projectDir, "deploy", "kcl", env)
	probePath := filepath.Join(envDir, "zz_forge_app_config_probe.k")
	writeFrontendCfgFile(t, probePath,
		"import .config as forge_envcfg\n\nforge_app_config = forge_envcfg.app_config\n")
	defer func() { _ = os.Remove(probePath) }()

	out, err := kclrender.Run(projectDir, probePath, []string{"env=" + env})
	if err != nil {
		t.Fatalf("render %s app_config: %v", env, err)
	}
	var doc struct {
		AppConfig map[string]any `json:"forge_app_config"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("parse app_config projection: %v", err)
	}
	if doc.AppConfig == nil {
		t.Fatalf("probe produced no app_config:\n%s", out)
	}
	return doc.AppConfig
}
