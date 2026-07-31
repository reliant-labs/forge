package templates

import (
	"go/parser"
	"go/token"
	"regexp"
	"strings"
	"testing"
)

// bundleTokenRead matches a credential pulled out of a frontend bundle's
// PUBLIC env (NEXT_PUBLIC_* / VITE_* / EXPO_PUBLIC_* are inlined at build
// time and readable by anyone who loads the page).
var bundleTokenRead = regexp.MustCompile(`(?:process\.env|import\.meta\.env)\.[A-Z0-9_]*(?:TOKEN|SECRET|KEY)`)

// TestRenderAppAuthTemplate verifies the OWNED internal/app/auth.go
// scaffold (app-auth.go.tmpl): it parses as Go, is owned (no DO-NOT-EDIT
// banner), exposes SetupAuth with the validator-returning signature the
// cmd serve wiring threads in, reads the per-environment JWT values off the
// TYPED CONFIG, and always returns a real JWT validator so a fresh project
// does real auth out of the box (no dev-auth bypass).
func TestRenderAppAuthTemplate(t *testing.T) {
	data := struct{ Module string }{Module: "example.com/myproject"}

	content, err := ProjectTemplates().Render("app-auth.go.tmpl", data)
	if err != nil {
		t.Fatalf("render app-auth.go.tmpl: %v", err)
	}
	s := string(content)

	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "auth.go", s, parser.AllErrors); perr != nil {
		t.Fatalf("rendered auth.go does not parse: %v\n%s", perr, s)
	}

	checks := []struct {
		name     string
		contains string
		want     bool
	}{
		// Owned, scaffold-once — NOT a forge-owned generated file.
		{"no generated banner", "DO NOT EDIT", false},
		{"package app", "package app", true},
		// The signature the generated cmd serve threads into AuthDeps.Validate.
		{"SetupAuth signature", "func SetupAuth(cfg *config.Config) (func(token string) (*auth.Claims, error), error) {", true},
		{"module config import", "example.com/myproject/pkg/config", true},
		{"forge auth import", "github.com/reliant-labs/forge/pkg/auth", true},
		{"default JWT validator", `Provider: "jwt"`, true},
		// All five per-deployment JWT values arrive on the typed config, the
		// same channel every other per-environment value in a forge app uses.
		// SigningMethod is the one a local run needs most: leave it unwired
		// and the scaffold takes JWTConfig's RS256 default and cannot
		// validate an HS256 token signed with the secret the same file reads.
		{"signing method from config", "cfg.JwtSigningMethod", true},
		{"issuer from config", "cfg.JwtIssuer", true},
		{"audience from config", "cfg.JwtAudience", true},
		{"jwks from config", "cfg.JwtJwksUrl", true},
		{"secret from config", "cfg.JwtSecret", true},
		// No auth bypass: SetupAuth always builds a real validator and never
		// returns a nil (auth-off) validator to the interceptor.
		{"no auth-off nil validator", "return nil, nil", false},
	}
	for _, tc := range checks {
		if found := strings.Contains(s, tc.contains); found != tc.want {
			if tc.want {
				t.Errorf("%s: expected %q to be present in output", tc.name, tc.contains)
			} else {
				t.Errorf("%s: expected %q to NOT be present in output", tc.name, tc.contains)
			}
		}
	}

	// The scaffold is the only file in a forge app that ever carried a
	// forbidigo suppression, and it carried FIVE. Configuration reaches this
	// app one way; a file that opts out of the guardrail is the app teaching
	// its next reader that opting out is normal. Assert on the MECHANISM
	// (os.* environment reads and the suppression that hid them), not on the
	// five variable names — a sixth knob added the old way must fail here too.
	for _, banned := range []string{"os.Getenv", "os.LookupEnv", "os.Environ", "nolint:forbidigo", `"os"`} {
		if strings.Contains(s, banned) {
			t.Errorf("internal/app/auth.go must read configuration off the typed cfg, never %s:\n%s", banned, s)
		}
	}
}

// TestConfigProtoTemplate_DeclaresJWTFields is the other half of Fix 2: the
// typed config has to actually CARRY the values SetupAuth reads, or the
// scaffold does not compile. It also pins the one property that is a
// security decision rather than a naming one — jwt_secret is `sensitive`, so
// the deploy projection binds it to a secretKeyRef instead of writing an
// HS256 signing key into a manifest as a literal.
func TestConfigProtoTemplate_DeclaresJWTFields(t *testing.T) {
	content, err := ProjectTemplates().Render("config.proto.tmpl", struct{ Module string }{Module: "example.com/myproject"})
	if err != nil {
		t.Fatalf("render config.proto.tmpl: %v", err)
	}
	s := string(content)

	for _, decl := range []string{
		"string jwt_signing_method",
		"string jwt_issuer",
		"string jwt_audience",
		"string jwt_jwks_url",
		"string jwt_secret",
	} {
		if !strings.Contains(s, decl) {
			t.Errorf("config.proto declares no %q field — SetupAuth reads it off the typed config and would not compile", decl)
		}
	}

	secretField := protoFieldBlock(t, s, "string jwt_secret")
	if !strings.Contains(secretField, "sensitive: true") {
		t.Errorf("jwt_secret must be `sensitive: true` — an HS* value both verifies AND mints, so it can never be an inline manifest value:\n%s", secretField)
	}
	// The other four are ordinary config: routing a JWKS URL or an issuer
	// through the secret store would demand a Secret entry for a value that
	// is public, and a dev loop that cannot boot without one.
	for _, name := range []string{"jwt_signing_method", "jwt_issuer", "jwt_audience", "jwt_jwks_url"} {
		if block := protoFieldBlock(t, s, "string "+name); strings.Contains(block, "sensitive: true") {
			t.Errorf("%s is public configuration and must not be marked sensitive:\n%s", name, block)
		}
	}
}

// protoFieldBlock returns the option block of the proto field whose
// declaration starts with decl, failing loudly when the field is gone.
func protoFieldBlock(t *testing.T, proto, decl string) string {
	t.Helper()
	i := strings.Index(proto, decl)
	if i < 0 {
		t.Fatalf("config.proto declares no %q field", decl)
	}
	rest := proto[i:]
	j := strings.Index(rest, "}];")
	if j < 0 {
		t.Fatalf("config.proto field %q has no option block", decl)
	}
	return rest[:j]
}

// TestCmdServeTemplate_CallsSetupAuth pins the call-site half of the
// owned-auth contract: the generated runServer calls the OWNED
// app.SetupAuth (unconditionally — auth is code now, not a forge.yaml
// provider gate), threads its validator into AuthDeps.Validate, carries no
// dev-auth bypass, and never references the retired InstallGeneratedAuth /
// GeneratedAuthInterceptor surface.
func TestCmdServeTemplate_CallsSetupAuth(t *testing.T) {
	data := struct {
		Module       string
		ConfigFields map[string]bool
		RESTEnabled  bool
	}{
		Module:       "example.com/myproject",
		ConfigFields: map[string]bool{},
	}

	content, err := ProjectTemplates().Render("cmd-tree-serve.go.tmpl", data)
	if err != nil {
		t.Fatalf("render cmd-tree-serve.go.tmpl: %v", err)
	}
	s := string(content)

	if !strings.Contains(s, "app.SetupAuth(cfg)") {
		t.Error("runServer must call the owned app.SetupAuth(cfg) to build the validator")
	}
	if !strings.Contains(s, "authDeps.Validate = authValidate") {
		t.Error("the validator from SetupAuth must be threaded into AuthDeps.Validate")
	}
	// The auth policy is exactly {AnonymousOK: false} — fail-closed, and no
	// auth-bypass field. Pinning the literal locks out re-introducing a
	// bypass toggle AND the non-gating posture: with AnonymousOK: true a
	// credential-less request reaches every handler claim-less, and
	// `auth_required: true` in the proto gates nothing. One measured app
	// served 17 of its 20 CRUD RPCs to anonymous callers that way, with
	// every one of them declaring auth_required: true.
	if !strings.Contains(s, "middleware.AuthDeps{AnonymousOK: false}") {
		t.Error("serve template must build authDeps as middleware.AuthDeps{AnonymousOK: false} — " +
			"fail-closed, with the published RPCs coming from the protos' auth_required declarations")
	}
	if !strings.Contains(s, "middleware.NewAuthInterceptor(authDeps)") {
		t.Error("auth is constructed up front so a server with no validator refuses to start")
	}
	for _, retired := range []string{"InstallGeneratedAuth", "GeneratedAuthInterceptor", "AuthProvider"} {
		if strings.Contains(s, retired) {
			t.Errorf("serve template still references the retired auth surface %q", retired)
		}
	}
}

// TestSessionProviderReadsEachPlatformsEnv pins the one thing a template
// conditional can get wrong SILENTLY: the scaffolded auth provider is a
// single shared file, and each frontend kind exposes public env under a
// different accessor. A Vite tree rendered with `process.env.NEXT_PUBLIC_*`
// still typechecks, still lints, still builds — and reads undefined at
// runtime, so the app is permanently signed out with nothing to point at.
//
// It also asserts the negative: no tree may carry another tree's accessor,
// which is what a stray branch or a copy-paste produces.
//
// And it pins what this provider must NEVER contain: a token. Forge issues
// no credentials, so a scaffolded provider that held one would be a
// credential inlined into a bundle and a session model production does not
// have. Signed-out against a real backend is the honest state.
func TestSessionProviderReadsEachPlatformsEnv(t *testing.T) {
	accessors := map[string]string{
		"nextjs":       "process.env.NEXT_PUBLIC_MOCK_API",
		"vite-spa":     "import.meta.env.VITE_MOCK_API",
		"react-native": "process.env.EXPO_PUBLIC_MOCK_API",
	}
	for platform, want := range accessors {
		t.Run(platform, func(t *testing.T) {
			content, err := FrontendTemplates().Render(
				"shared/src/lib/auth/session-provider.ts.tmpl",
				FrontendTemplateData{Platform: platform},
			)
			if err != nil {
				t.Fatalf("render session-provider for %s: %v", platform, err)
			}
			src := string(content)
			if !strings.Contains(src, want) {
				t.Errorf("%s must read mock mode via %s:\n%s", platform, want, src)
			}
			for other, accessor := range accessors {
				if other == platform {
					continue
				}
				if strings.Contains(src, accessor) {
					t.Errorf("%s carries %s's accessor %s — it reads undefined there, so the app renders the wrong session state", platform, other, accessor)
				}
			}
			// The MECHANISM, not one variable name: ANY token read out of the
			// bundle's public env is a credential forge put in a browser.
			if m := bundleTokenRead.FindString(src); m != "" {
				t.Errorf("%s reads a credential (%s) out of the bundle env — forge issues no tokens, so the scaffolded provider holds none:\n%s", platform, m, src)
			}
		})
	}
}
