package packs

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

func TestJWTAuthPackManifest(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}

	if p.Name != "jwt-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "jwt-auth")
	}
	if !strings.Contains(p.Description, "JWKS") {
		t.Errorf("Description should mention JWKS, got: %s", p.Description)
	}
	// Honesty: the description must state the dev/JWT split (dev mode
	// needs no pack) and that the code ships with no call sites.
	if !strings.Contains(p.Description, "needs NO pack") {
		t.Errorf("Description should state local dev needs no pack, got: %s", p.Description)
	}
	if !strings.Contains(p.Description, "NO call sites") {
		t.Errorf("Description should state the code has no call sites until wired, got: %s", p.Description)
	}
	// The wiring the user must do by hand is printed at install time.
	for _, want := range []string{"jwtauth.Init", "jwtauth.Interceptor", "jwtauth.Close"} {
		if !strings.Contains(p.PostInstall, want) {
			t.Errorf("PostInstall should show the %s wiring, got: %s", want, p.PostInstall)
		}
	}

	// Check dependencies. Match by module path prefix so version-pinned
	// entries (e.g. "module@v1.2.3") still satisfy the check.
	wantDeps := map[string]bool{
		"github.com/golang-jwt/jwt/v5":     false,
		"github.com/MicahParks/keyfunc/v3": false,
	}
	for _, dep := range p.Dependencies {
		modPath := dep
		if i := strings.Index(dep, "@"); i >= 0 {
			modPath = dep[:i]
		}
		if _, ok := wantDeps[modPath]; ok {
			wantDeps[modPath] = true
		}
	}
	for dep, found := range wantDeps {
		if !found {
			t.Errorf("missing dependency: %s", dep)
		}
	}

	// Check files reference the correct templates
	templateNames := make(map[string]bool)
	for _, f := range p.Files {
		templateNames[f.Template] = true
	}
	if !templateNames["jwt_validator.go.tmpl"] {
		t.Error("files should include jwt_validator.go.tmpl")
	}
	if !templateNames["dev_auth.go.tmpl"] {
		t.Error("files should include dev_auth.go.tmpl")
	}
	if !templateNames["dev_login_handler.go.tmpl"] {
		t.Error("files should include dev_login_handler.go.tmpl")
	}

	// Check that all middleware-package files land under
	// pkg/middleware/auth/jwtauth/ — the per-pack nested subpackage that
	// prevents collisions with other auth packs (e.g. clerk).
	for _, f := range p.Files {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/jwtauth/") {
			t.Errorf("file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/jwtauth/<file>", f.Output)
		}
	}
	for _, f := range p.Generate {
		if strings.HasPrefix(f.Output, "pkg/middleware/") &&
			!strings.HasPrefix(f.Output, "pkg/middleware/auth/jwtauth/") {
			t.Errorf("generate file %s emits into pkg/middleware/ root; expected pkg/middleware/auth/jwtauth/<file>", f.Output)
		}
	}

	// Subpath hint must match the actual installation tree so users see the
	// right thing in `forge pack info`.
	if p.Subpath != "middleware/auth/jwtauth" {
		t.Errorf("Subpath = %q, want %q", p.Subpath, "middleware/auth/jwtauth")
	}

	// Check generate hook
	if len(p.Generate) != 1 {
		t.Fatalf("len(Generate) = %d, want 1", len(p.Generate))
	}
	if p.Generate[0].Template != "auth_gen_override.go.tmpl" {
		t.Errorf("Generate[0].Template = %q, want %q", p.Generate[0].Template, "auth_gen_override.go.tmpl")
	}
}

func TestJWTAuthTemplatesRender(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}

	data := map[string]any{
		"ModulePath":  "github.com/example/myapp",
		"ProjectName": "myapp",
		"PackConfig":  p.Config.Defaults,
	}

	// Test all file templates render without error
	allFiles := append(p.Files, p.Generate...)
	for _, f := range allFiles {
		t.Run(f.Template, func(t *testing.T) {
			tmplPath := "jwt-auth/templates/" + f.Template
			tmplContent, err := packsFS.ReadFile(tmplPath)
			if err != nil {
				t.Fatalf("read template %s: %v", tmplPath, err)
			}

			tmpl, err := template.New(f.Template).Funcs(templates.FuncMap()).Parse(string(tmplContent))
			if err != nil {
				t.Fatalf("parse template %s: %v", f.Template, err)
			}

			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, data); err != nil {
				t.Fatalf("execute template %s: %v", f.Template, err)
			}

			output := buf.String()
			if len(output) == 0 {
				t.Errorf("template %s produced empty output", f.Template)
			}

			// Verify output contains expected package declaration. After
			// the per-pack-subpackage refactor, all jwt-auth code lives in
			// package jwtauth (under pkg/middleware/auth/jwtauth/).
			if !strings.Contains(output, "package jwtauth") {
				t.Errorf("template %s output missing 'package jwtauth'", f.Template)
			}
		})
	}
}

func TestJWTValidatorTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/jwt_validator.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	// Should contain key types and functions. Names are unprefixed because
	// the file lives in its own subpackage (jwtauth) — no collision with
	// other auth packs.
	checks := []string{
		"package jwtauth",
		"type Validator struct",
		"type ValidatorConfig struct",
		"func NewValidator(",
		"ValidateToken",
		"keyfunc.NewDefault",
		"func (v *Validator) Close()",
		// Cross-package references for shared Claims type.
		"middleware.Claims",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("jwt_validator.go.tmpl should contain %q", check)
		}
	}
}

func TestDevAuthTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/dev_auth.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package jwtauth",
		// Unprefixed names — the package itself namespaces them.
		"func DevAuthEnabled(",
		"func DevClaims(",
		"dev-user-001",
		"ENVIRONMENT",
		// Dev-bypass sentinel — must stay in sync with DEV_BYPASS_TOKEN
		// in the frontend stub auth provider. Keep both as compile-time
		// constants so accidental changes show up in code review.
		"DevBypassToken",
		`"dev-bypass-do-not-use-in-prod"`,
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("dev_auth.go.tmpl should contain %q", check)
		}
	}

	// Negative checks: the old prefixed names belong to the pre-subpackage
	// era. They must not reappear, otherwise we lose the structural
	// collision-free property that the subpackage layout buys us.
	for _, banned := range []string{"JWTDevAuthEnabled", "JWTDevClaims"} {
		if strings.Contains(content, banned) {
			t.Errorf("dev_auth.go.tmpl must not declare prefixed %q (now lives in package jwtauth)", banned)
		}
	}
}

func TestDevLoginHandlerTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/dev_login_handler.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package jwtauth",
		"func DevLoginHandler(",
		// Frontend contract — must match auth-ui's LoginForm expectation.
		"type DevLoginResponse struct",
		"Token",
		"ExpiresAt",
		"User",
		// Dev-gate — endpoint must 404 when not in dev mode.
		"DevAuthEnabled()",
		"http.NotFound",
		// Sentinel — must reuse the constant from dev_auth.go.
		"DevBypassToken",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("dev_login_handler.go.tmpl should contain %q", check)
		}
	}
}

func TestAuthGenOverrideTemplateContent(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/auth_gen_override.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}

	content := string(tmplContent)

	checks := []string{
		"package jwtauth",
		"func Init(",
		"func Close(",
		"func Interceptor(",
		"DevAuthEnabled",
		"validator",
		"JWT_JWKS_URL",
		"JWT_SIGNING_METHOD",
		"middleware.ContextWithClaims",
		// Sentinel bypass — per-request, gated on DevAuthEnabled. Without
		// this branch, dev-mode is either all-bypass (old behavior, can't
		// test real login) or all-validate (no fast path for agents).
		"DevBypassToken",
	}
	for _, check := range checks {
		if !strings.Contains(content, check) {
			t.Errorf("auth_gen_override.go.tmpl should contain %q", check)
		}
	}
}

// TestDevAuthTemplate_NoEnvGate pins the dev-mode unification: the
// jwt-auth pack's dev gate consumes the typed config.Mode injected at
// Init — it must NOT read os.Getenv("ENVIRONMENT") itself. Three
// scattered env gates (bootstrap, middleware-auth, jwtauth) is how
// dev-mode skew between authn and authz happened.
func TestDevAuthTemplate_NoEnvGate(t *testing.T) {
	tmplContent, err := packsFS.ReadFile("jwt-auth/templates/dev_auth.go.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	content := string(tmplContent)

	if strings.Contains(content, "os.Getenv") {
		t.Errorf("dev_auth.go.tmpl must not read the environment directly — dev mode is injected as config.Mode:\n%s", content)
	}
	for _, want := range []string{"config.Mode", "func SetMode("} {
		if !strings.Contains(content, want) {
			t.Errorf("dev_auth.go.tmpl should contain %q (typed-mode injection)", want)
		}
	}

	// Init must accept the injected mode.
	override, err := packsFS.ReadFile("jwt-auth/templates/auth_gen_override.go.tmpl")
	if err != nil {
		t.Fatalf("read override template: %v", err)
	}
	if !strings.Contains(string(override), "func Init(logger *slog.Logger, mode config.Mode) error") {
		t.Errorf("auth_gen_override.go.tmpl Init should take the injected config.Mode")
	}
}

// TestApplyAuthConfigSection pins install-time config wiring: installing
// an auth pack sets forge.yaml's typed auth block the way the pack docs
// claim (the J1 finding: the packs skill SAID install sets auth.provider;
// nothing did, so the generate pipeline's auth-aware steps never ran).
func TestApplyAuthConfigSection(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}
	eff := mergePackConfig(p.Config.Defaults, nil)

	// Fresh project: provider + jwt defaults projected.
	cfg := &config.ProjectConfig{}
	p.applyAuthConfigSection(cfg, eff)
	if cfg.Auth.Provider != "jwt" {
		t.Errorf("Auth.Provider = %q, want %q", cfg.Auth.Provider, "jwt")
	}
	if cfg.Auth.JWT.SigningMethod != "RS256" {
		t.Errorf("Auth.JWT.SigningMethod = %q, want RS256", cfg.Auth.JWT.SigningMethod)
	}
	// Empty defaults (jwks_url etc.) must not be projected as "".
	if cfg.Auth.JWT.JWKSURL != "" || cfg.Auth.JWT.Issuer != "" || cfg.Auth.JWT.Audience != "" {
		t.Errorf("empty jwt defaults should stay empty, got %+v", cfg.Auth.JWT)
	}

	// User intent wins: an existing different provider is never stomped.
	cfg2 := &config.ProjectConfig{Auth: config.AuthConfig{Provider: "api_key"}}
	p.applyAuthConfigSection(cfg2, eff)
	if cfg2.Auth.Provider != "api_key" {
		t.Errorf("existing Auth.Provider was overwritten: %q", cfg2.Auth.Provider)
	}
	if cfg2.Auth.JWT.SigningMethod != "" {
		t.Errorf("jwt defaults should not apply when the provider is kept, got %+v", cfg2.Auth.JWT)
	}

	// User-set jwt fields survive a matching-provider install.
	cfg3 := &config.ProjectConfig{Auth: config.AuthConfig{
		Provider: "jwt",
		JWT:      config.JWTConfig{SigningMethod: "ES256", Issuer: "https://issuer.example"},
	}}
	p.applyAuthConfigSection(cfg3, eff)
	if cfg3.Auth.JWT.SigningMethod != "ES256" || cfg3.Auth.JWT.Issuer != "https://issuer.example" {
		t.Errorf("user-set jwt fields were overwritten: %+v", cfg3.Auth.JWT)
	}

	// Non-auth packs never touch the auth block.
	np := &Pack{Config: PackConfig{Section: "nats", Defaults: map[string]any{"provider": "x"}}}
	cfg4 := &config.ProjectConfig{}
	np.applyAuthConfigSection(cfg4, mergePackConfig(np.Config.Defaults, nil))
	if cfg4.Auth.Provider != "" {
		t.Errorf("non-auth pack set Auth.Provider = %q", cfg4.Auth.Provider)
	}
}
