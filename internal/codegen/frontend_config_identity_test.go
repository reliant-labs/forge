package codegen

import (
	"path/filepath"
	"strings"
	"testing"
)

// oidcFrontendConfig is a frontend whose config message declares the three
// OIDC fields, i.e. one scaffolded by a forge that ships the browser
// sign-in surface.
func oidcFrontendConfig() FrontendConfig {
	fc := webFrontendConfig()
	fc.Fields = append(fc.Fields,
		ConfigField{Name: "oidc_issuer", ProtoType: "string", EnvVar: "OIDC_ISSUER"},
		ConfigField{Name: "oidc_client_id", ProtoType: "string", EnvVar: "OIDC_CLIENT_ID"},
		ConfigField{Name: "oidc_redirect_uri", ProtoType: "string", EnvVar: "OIDC_REDIRECT_URI"},
	)
	return fc
}

// backendJWTFields is the backend AppConfig's identity half — the fields a
// scaffolded config.proto declares for token validation. The dev-identity
// binding resolves its field NAMES from these by env var, so a test that
// omits them is testing a project that cannot validate a token at all.
func backendJWTFields() []ConfigField {
	return []ConfigField{
		{Name: "jwt_issuer", ProtoType: "string", EnvVar: "JWT_ISSUER"},
		{Name: "jwt_audience", ProtoType: "string", EnvVar: "JWT_AUDIENCE"},
		{Name: "jwt_jwks_url", ProtoType: "string", EnvVar: "JWT_JWKS_URL"},
	}
}

func scaffoldInstance(t *testing.T, fc FrontendConfig, env string, devIdentity bool) string {
	t.Helper()
	kclDir := filepath.Join(t.TempDir(), "deploy", "kcl")
	path := writeConfigK(t, kclDir, env, backendOnlyConfigK)
	if _, err := EnsureFrontendConfigInstances([]FrontendConfig{fc}, kclDir, env, "acme", devIdentity, backendJWTFields()); err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	return readFile(t, path)
}

// The dev identity block imports the committed identity module and READS
// it — client_id and issuer both come from the published file, never from
// a render-time plugin call. The redirect URI stays scaffolded EMPTY
// (the frontend fills it in at runtime).
func TestFrontendConfigInstance_DevIdentityBlock(t *testing.T) {
	body := scaffoldInstance(t, oidcFrontendConfig(), "dev", true)

	for _, want := range []string{
		"import ." + IDPIdentityModule + " as idp",
		`oidc_issuer = idp.idp_identity["issuer"]`,
		`oidc_redirect_uri = ""`,
		`oidc_client_id = idp.idp_identity["client_id"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.k missing %s:\n%s", want, body)
		}
	}
}

// THE UN-PINNING. A scaffolded dev environment must not name a frontend port
// anywhere in its identity block: the whole point of the origin glob the
// idp-provision job registers is that the frontend keeps the kernel-
// assigned port `forge run` gives it, so a literal port here would
// silently re-pin it and stop two dev stacks from signing in at once.
func TestFrontendConfigInstance_DevIdentityPinsNoFrontendPort(t *testing.T) {
	body := scaffoldInstance(t, oidcFrontendConfig(), "dev", true)

	if strings.Contains(body, "localhost:3000") || strings.Contains(body, "localhost:8080") {
		t.Errorf("the dev identity block pins a frontend port:\n%s", body)
	}
}

// A DEPLOYED environment gets no identity block and no read of the
// published identity file. Its issuer is a real one whose applications are
// registered out of band, so forge must never emit a line that would try
// to import a dev-only convergence artifact.
func TestFrontendConfigInstance_NoDevIdentityForDeployedEnv(t *testing.T) {
	body := scaffoldInstance(t, oidcFrontendConfig(), "prod", false)

	for _, unwanted := range []string{"idp_identity", "import ." + IDPIdentityModule} {
		if strings.Contains(body, unwanted) {
			t.Errorf("a deployed env's config.k must not read the dev identity file, found %q:\n%s", unwanted, body)
		}
	}
}

// A hand-written config message with no OIDC fields gets no identity block:
// emitting one would reference fields the schema does not declare, which is
// a KCL error rather than a missing convenience.
func TestFrontendConfigInstance_NoIdentityFieldsNoBlock(t *testing.T) {
	body := scaffoldInstance(t, webFrontendConfig(), "dev", true)

	if strings.Contains(body, "idp_identity") {
		t.Errorf("emitted an identity read for a config with no OIDC fields:\n%s", body)
	}
}

// All three fields or none. A client id without the issuer that minted it,
// or without the redirect URI it is registered against, declares a sign-in
// that cannot complete — so a partial field set is left entirely alone.
func TestFrontendConfigInstance_PartialIdentityFieldsNoBlock(t *testing.T) {
	fc := webFrontendConfig()
	fc.Fields = append(fc.Fields,
		ConfigField{Name: "oidc_issuer", ProtoType: "string", EnvVar: "OIDC_ISSUER"},
		ConfigField{Name: "oidc_client_id", ProtoType: "string", EnvVar: "OIDC_CLIENT_ID"},
		// No oidc_redirect_uri.
	)
	body := scaffoldInstance(t, fc, "dev", true)

	if strings.Contains(body, "idp_identity") {
		t.Errorf("emitted a partial identity block:\n%s", body)
	}
}

// THE BOTH-HALVES RULE. A dev environment that tells the browser where to
// sign in must also tell the server what to accept, from the SAME published
// identity.
//
// This is a regression test for the state every scaffolded project shipped
// in: the frontend block was wired, the backend's was not, so the dev IdP
// minted a real token and every authenticated RPC answered 401 with "no JWT
// signing material configured". It presents as a broken token — the search
// starts at the validator, which is being shown a token it was never told
// how to check — and `forge run` reported a healthy stack throughout.
func TestFrontendConfigInstance_DevIdentityWiresBackendToo(t *testing.T) {
	body := scaffoldInstance(t, oidcFrontendConfig(), "dev", true)

	for _, want := range []string{
		backendJWTOverrideMarker,
		`jwt_issuer = idp.idp_identity["issuer"]`,
		`jwt_jwks_url = idp.idp_identity["jwks_url"]`,
		`jwt_audience = idp.idp_identity["audience"]`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("config.k does not wire the backend half (%s):\n%s", want, body)
		}
	}
}

// The backend binding is DEV-ONLY, for the same reason the frontend block is:
// a deployed environment validates against a real issuer configured out of
// band, and a reference to the dev convergence artifact would not even render
// there.
func TestFrontendConfigInstance_NoBackendIdentityForDeployedEnv(t *testing.T) {
	body := scaffoldInstance(t, oidcFrontendConfig(), "prod", false)

	if strings.Contains(body, backendJWTOverrideMarker) || strings.Contains(body, "jwt_issuer") {
		t.Errorf("a deployed env must not bind the dev identity:\n%s", body)
	}
}

// Appending must be idempotent. `forge generate` runs constantly, and a
// binding that stacked up a new union block each time would turn a working
// config.k into a growing pile of identical overrides.
func TestFrontendConfigInstance_BackendIdentityAppendedOnce(t *testing.T) {
	kclDir := filepath.Join(t.TempDir(), "deploy", "kcl")
	path := writeConfigK(t, kclDir, "dev", backendOnlyConfigK)
	fc := oidcFrontendConfig()

	for range 3 {
		if _, err := EnsureFrontendConfigInstances(
			[]FrontendConfig{fc}, kclDir, "dev", "acme", true, backendJWTFields()); err != nil {
			t.Fatalf("scaffold: %v", err)
		}
	}

	if got := strings.Count(readFile(t, path), backendJWTOverrideMarker); got != 1 {
		t.Errorf("backend identity block appears %d times, want exactly 1", got)
	}
}

// An author who has taken these fields over by hand owns them. Forge layering
// its own issuer on top would silently override a deliberate choice — a
// split-issuer dev setup (a token-exchange gateway, an IdP migration) is
// legitimate, and doctor already reports the parity it cannot judge.
func TestFrontendConfigInstance_BackendIdentityRespectsHandWiring(t *testing.T) {
	kclDir := filepath.Join(t.TempDir(), "deploy", "kcl")
	handWired := `import config_gen

app_config: config_gen.AppConfig = {
    jwt_issuer = "https://id.example.com"
    jwt_jwks_url = "https://id.example.com/.well-known/jwks.json"
}
`
	path := writeConfigK(t, kclDir, "dev", handWired)
	if _, err := EnsureFrontendConfigInstances(
		[]FrontendConfig{oidcFrontendConfig()}, kclDir, "dev", "acme", true, backendJWTFields()); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	body := readFile(t, path)
	if strings.Contains(body, backendJWTOverrideMarker) {
		t.Errorf("forge overrode a hand-wired issuer:\n%s", body)
	}
	if !strings.Contains(body, "https://id.example.com") {
		t.Errorf("the hand-wired issuer was lost:\n%s", body)
	}
}

// A project whose config declares no JWT fields at all gets no binding —
// a reference to a field that does not exist would not render.
func TestFrontendConfigInstance_BackendIdentitySkippedWithoutJWTFields(t *testing.T) {
	kclDir := filepath.Join(t.TempDir(), "deploy", "kcl")
	path := writeConfigK(t, kclDir, "dev", backendOnlyConfigK)
	if _, err := EnsureFrontendConfigInstances(
		[]FrontendConfig{oidcFrontendConfig()}, kclDir, "dev", "acme", true, nil); err != nil {
		t.Fatalf("scaffold: %v", err)
	}

	if strings.Contains(readFile(t, path), backendJWTOverrideMarker) {
		t.Errorf("bound JWT fields the project does not declare:\n%s", readFile(t, path))
	}
}

// THE BACKFILL. The projects that most need the backend half are the ones
// that already exist: their config.k was written before it was wired, so
// there is no frontend instance left to append and the append pass has
// nothing else to do. If the binding rode along with an append, every
// existing project would stay broken through any number of `forge generate`
// runs — which is the state this whole fix exists to end.
func TestFrontendConfigInstance_BackendIdentityBackfillsExistingProject(t *testing.T) {
	kclDir := filepath.Join(t.TempDir(), "deploy", "kcl")

	// config.k as an OLDER forge left it: the frontend instance is already
	// declared and reads the published identity, but nothing binds the
	// backend.
	alreadyScaffolded := `import config_gen
import frontend_config_gen

app_config: config_gen.AppConfig = {
    environment = "development"
}

import .` + IDPIdentityModule + ` as idp

web_config: frontend_config_gen.WebConfig = {
    oidc_issuer = idp.idp_identity["issuer"]
    oidc_redirect_uri = ""
    oidc_client_id = idp.idp_identity["client_id"]
}
`
	path := writeConfigK(t, kclDir, "dev", alreadyScaffolded)

	added, err := EnsureFrontendConfigInstances(
		[]FrontendConfig{oidcFrontendConfig()}, kclDir, "dev", "acme", true, backendJWTFields())
	if err != nil {
		t.Fatalf("scaffold: %v", err)
	}
	if len(added) != 0 {
		t.Errorf("added = %v, want none (the instance was already declared)", added)
	}

	body := readFile(t, path)
	if !strings.Contains(body, `jwt_jwks_url = idp.idp_identity["jwks_url"]`) {
		t.Errorf("an existing project was not backfilled:\n%s", body)
	}
	// The author's own values must survive the insertion untouched.
	if !strings.Contains(body, `environment = "development"`) {
		t.Errorf("backfill dropped an existing value:\n%s", body)
	}
}
