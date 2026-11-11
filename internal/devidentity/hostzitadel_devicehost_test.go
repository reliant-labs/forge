package devidentity

// --device-host against the real host Zitadel: the IdP serves this machine's
// LAN address as a second domain of ONE instance, the converge publishes that
// origin as the run's issuer, and a token minted at it carries an `iss` the
// API's validator (pkg/auth, pinned to the published issuer, keys found
// through discovery exactly as the scaffolded SetupAuth does) accepts. The
// API's userinfo lookup, with the dev-only AllowLocalPlainHTTP opt-in, fills
// the user's verified address from that LAN-address issuer, and without the
// opt-in it does not. Then the way back, twice: a run without a device host
// against the still-running process (the real CLI path), and a stop/start.
//
// Skipped under -short: it boots host postgres and the pinned Zitadel, and
// needs an address of this machine other than loopback.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/hostinfra"
	"github.com/reliant-labs/forge/internal/secrets"
	"github.com/reliant-labs/forge/internal/templates"
	"github.com/reliant-labs/forge/pkg/auth"
	"github.com/reliant-labs/forge/pkg/devidp"
	"github.com/reliant-labs/forge/pkg/identity"
	"github.com/reliant-labs/forge/pkg/oauth2"
	"github.com/reliant-labs/forge/pkg/release"
)

// lanIPv4 is a non-loopback IPv4 address of this machine, "" when it has none.
func lanIPv4() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLoopback() && !n.IP.IsLinkLocalUnicast() {
			return n.IP.String()
		}
	}
	return ""
}

func TestConverge_DeviceHostAgainstTheRealHostZitadel(t *testing.T) {
	if testing.Short() {
		t.Skip("boots host postgres and the pinned Zitadel; runs in task test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("host Zitadel is exercised on unix")
	}
	lan := lanIPv4()
	if lan == "" {
		t.Skip("this machine has no non-loopback IPv4 address to act as a device host")
	}

	dir := t.TempDir()
	steps, err := templates.ProjectTemplates().Render("idp-steps.yaml.tmpl", struct{ ProjectName string }{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "idp-steps.yaml"), steps, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pgSpec := hostinfra.Spec{Name: "postgres", Engine: hostinfra.EnginePostgres, Port: freeLoopbackPort(t), Database: "acme", User: "postgres", Password: "postgres"}
	idpSpec := hostinfra.Spec{
		Name: "idp", Engine: hostinfra.EngineZitadel, Port: freeLoopbackPort(t),
		IDPDatabase: "zitadel", IDPDatabasePort: pgSpec.Port, IDPMasterKey: "MasterkeyNeedsToHave32Characters",
		// An ABSOLUTE, non-default PAT path: the default one is also published
		// to ~/.forge/idp/localhost_<port>_zitadel, which a test must not litter.
		IDPStepsFile: "idp-steps.yaml", IDPPATPath: filepath.Join(dir, "pat", "pat.txt"),
		Database: "postgres", User: "postgres", Password: "postgres",
	}
	t.Cleanup(func() {
		_ = hostinfra.Stop(dir, idpSpec)
		_ = hostinfra.Stop(dir, pgSpec)
	})
	if err := hostinfra.Start(ctx, dir, pgSpec); err != nil {
		t.Skipf("host postgres is unavailable here: %v", err)
	}
	// A localhost-only IdP first. Only THIS may skip the test (no binary, no
	// network): from here on a failure is a failure of the device-host code.
	if err := hostinfra.Start(ctx, dir, idpSpec); err != nil {
		t.Skipf("host Zitadel is unavailable here: %v", err)
	}
	withDevice := idpSpec
	withDevice.DeviceHost = lan
	// The running instance gains the device host: ONE restart that adds the
	// domain, no data reset.
	if err := hostinfra.Start(ctx, dir, withDevice); err != nil {
		t.Fatalf("adding the device host to a running IdP: %v", err)
	}

	port := itoa(idpSpec.Port)
	localOrigin, deviceOrigin := "http://localhost:"+port, "http://"+lan+":"+port
	for _, origin := range []string{localOrigin, deviceOrigin} {
		if iss := discoveryIssuer(t, origin); iss != origin {
			t.Fatalf("discovery at %s names issuer %q: iss must follow the origin the request came in at", origin, iss)
		}
	}

	const frontend = "http://localhost:3000"
	sh := &release.ShapeIdentity{
		Name: "identity", Runtime: release.IdentityRuntimeHost,
		Login: release.ShapeIdentityLogin{Mode: release.IdentityLoginNative},
		Roles: []release.ShapeIdentityRole{{Key: "admin", MembersFrom: release.IdentityMembersConsole}},
		Applications: []release.ShapeIdentityApplication{
			{Name: "dev:api", Workload: "api", Kind: release.IdentityAppNativeBroker},
			{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile", RedirectURIs: []string{"com.acme.mobile:/oauthredirect"}},
		},
		SeedUsers: []release.ShapeIdentitySeedUser{{Email: "dev@acme.test", Roles: []string{"admin"}, PasswordSecret: "DEV_PASSWORD"}},
	}
	store := filepath.Join(dir, "secrets", "dev.yaml")
	in := Input{
		Project: "acme", Identity: sh,
		BaseURL: localOrigin, DeviceHost: lan, PATPath: idpSpec.PATPath(dir),
		FrontendOrigin: frontend,
		Secret: func(n string) (string, bool) {
			if n == "DEV_PASSWORD" {
				return "Password1!", true
			}
			return "", false
		},
		Refs: []Ref{
			{EnvName: "JWT_ISSUER", SecretName: "forge-identity-identity", SecretKey: "ISSUER"},
			{EnvName: "JWT_AUDIENCE", SecretName: "forge-identity-identity", SecretKey: "AUDIENCE"},
			{EnvName: "OIDC_CLIENT_ID", SecretName: "forge-identity-identity", SecretKey: "DEV_API_CLIENT_ID"},
			{EnvName: "OIDC_REDIRECT_URI", SecretName: "forge-identity-identity", SecretKey: "DEV_API_REDIRECT_URI"},
			{EnvName: "IDP_BASE", SecretName: "forge-identity-identity", SecretKey: "ISSUER"},
			{EnvName: "IDP_BROKER_TOKEN", SecretName: "forge-identity-identity", SecretKey: "DEV_API_BROKER_TOKEN"},
		},
		StorePath:   store,
		OutputsPath: filepath.Join(dir, ".forge", "identity", "identity.json"),
		KeysDir:     filepath.Join(dir, ".forge", "identity", "identity.keys"),
	}

	readOutput := func() identity.Output {
		var out identity.Output
		raw, err := os.ReadFile(in.OutputsPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// signIn is the broker's real path: it dials IDP_BASE (the published
	// issuer) with the broker token and signs the seed user in.
	signIn := func(t *testing.T, issuer string) (string, map[string]string) {
		t.Helper()
		out := readOutput()
		if out.Issuer != issuer {
			t.Fatalf("published issuer = %q, want %q", out.Issuer, issuer)
		}
		vals := readStore(t, store)
		if vals["JWT_ISSUER"] != issuer || vals["IDP_BASE"] != issuer {
			t.Fatalf("store JWT_ISSUER %q / IDP_BASE %q, want %q for both", vals["JWT_ISSUER"], vals["IDP_BASE"], issuer)
		}
		client := &devidp.Client{Base: vals["IDP_BASE"], Token: vals["IDP_BROKER_TOKEN"]}
		toks, err := client.SignIn(ctx, devidp.FlowConfig{
			ClientID: vals["OIDC_CLIENT_ID"], RedirectURI: vals["OIDC_REDIRECT_URI"], Scopes: out.Scopes,
		}, devidp.Credentials{LoginName: "dev@acme.test", Password: "Password1!"})
		if err != nil {
			t.Fatalf("sign in through %s: %v", issuer, err)
		}
		return toks.AccessToken, vals
	}
	// apiAuthenticator is the scaffolded SetupAuth: the key source is found
	// through the issuer's OIDC discovery (jwt_jwks_url empty), and the userinfo
	// lookup is installed over the same issuer, with the dev opt-in when asked.
	apiAuthenticator := func(t *testing.T, issuer, audience string, allowLocalHTTP bool) *auth.Authenticator {
		t.Helper()
		var client *http.Client
		var doer oauth2.HTTPDoer
		if allowLocalHTTP {
			client = auth.LocalIssuerHTTPClient(issuer)
			doer = client
		}
		meta, err := oauth2.Discover(ctx, doer, issuer)
		if err != nil {
			t.Fatalf("discover %s: %v", issuer, err)
		}
		v, err := auth.NewValidator(auth.Config{Provider: "jwt", JWT: auth.JWTConfig{
			Issuer: issuer, Audience: audience, JWKSURL: meta.JWKSURI, HTTPClient: client,
		}})
		if err != nil {
			t.Fatalf("API validator: %v", err)
		}
		t.Cleanup(func() { _ = v.Close() })
		a := auth.NewAuthenticator(v.Validate)
		userInfo := auth.NewUserInfo(issuer, func(context.Context) (string, error) { return meta.UserinfoEndpoint, nil }, nil)
		if allowLocalHTTP {
			userInfo.AllowLocalPlainHTTP()
		}
		a.UseUserInfo(userInfo)
		return a
	}
	signInAndValidate := func(t *testing.T, issuer string) (*auth.Claims, error) {
		t.Helper()
		token, vals := signIn(t, issuer)
		return apiAuthenticator(t, vals["JWT_ISSUER"], vals["JWT_AUDIENCE"], false).Authenticate(ctx, token)
	}

	first, err := Converge(ctx, in)
	if err != nil {
		t.Fatalf("first converge: %v", err)
	}
	if !first.Report.Changed() {
		t.Fatal("a fresh instance converged without a single change")
	}
	t.Run("device run: one issuer, the device origin, accepted by the API", func(t *testing.T) {
		claims, err := signInAndValidate(t, deviceOrigin)
		if err != nil {
			t.Fatalf("a token minted at the device origin was rejected by the API's validator: %v", err)
		}
		if claims.UserID == "" {
			t.Error("accepted token carries no subject")
		}
		out := readOutput()
		if nat := out.Apps["dev:mobile"]; nat.ClientID == "" || strings.Join(nat.RedirectURIs, " ") != "com.acme.mobile:/oauthredirect" {
			t.Errorf("native app = %+v", nat)
		}
		if n := nativeOIDCIssuer(out); n != deviceOrigin {
			t.Errorf("the issuer a device is handed = %q, want %q", n, deviceOrigin)
		}
	})
	t.Run("userinfo at the LAN-address issuer: filled with the opt-in, refused by default", func(t *testing.T) {
		token, vals := signIn(t, deviceOrigin)
		withOptIn, err := apiAuthenticator(t, vals["JWT_ISSUER"], vals["JWT_AUDIENCE"], true).Authenticate(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if withOptIn.Email != "dev@acme.test" || !withOptIn.EmailVerified {
			t.Errorf("with AllowLocalPlainHTTP: Email %q verified %v, want the seed user's verified address", withOptIn.Email, withOptIn.EmailVerified)
		}
		byDefault, err := apiAuthenticator(t, vals["JWT_ISSUER"], vals["JWT_AUDIENCE"], false).Authenticate(ctx, token)
		if err != nil {
			t.Fatal(err)
		}
		if byDefault.Email != "" {
			t.Errorf("by default Email = %q: a plain-HTTP LAN issuer must not be sent a token unless opted in", byDefault.Email)
		}
	})
	t.Run("the localhost origin still serves the instance, under its own issuer", func(t *testing.T) {
		// One issuer per run: the API pinned to the device origin must NOT
		// accept a token the same instance minted for localhost.
		vals := readStore(t, store)
		client := &devidp.Client{Base: localOrigin, Token: vals["IDP_BROKER_TOKEN"]}
		toks, err := client.SignIn(ctx, devidp.FlowConfig{
			ClientID: vals["OIDC_CLIENT_ID"], RedirectURI: vals["OIDC_REDIRECT_URI"], Scopes: readOutput().Scopes,
		}, devidp.Credentials{LoginName: "dev@acme.test", Password: "Password1!"})
		if err != nil {
			t.Fatalf("sign in at localhost with a device-host instance: %v", err)
		}
		v, err := auth.NewValidator(auth.Config{Provider: "jwt", JWT: auth.JWTConfig{
			Issuer: deviceOrigin, JWKSURL: deviceOrigin + "/oauth/v2/keys",
		}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = v.Close() })
		if _, err := v.Validate(toks.AccessToken); err == nil {
			t.Error("a localhost-issued token validated against the device issuer: the API would accept two issuers")
		}
	})
	t.Run("a second pass writes nothing and a second start is a no-op", func(t *testing.T) {
		second, err := Converge(ctx, in)
		if err != nil {
			t.Fatalf("second converge: %v", err)
		}
		if second.Report.Changed() || len(second.StoreKeys) != 0 {
			t.Errorf("converged instance written to again: %v %v", second.Report.Changes, second.StoreKeys)
		}
		if err := hostinfra.Start(ctx, dir, withDevice); err != nil {
			t.Fatal(err)
		}
	})

	// The way back, on the same database and without wiping anything. First
	// the real CLI path: a run without --device-host finds the IdP still
	// running (it was started with the device host and keeps serving both
	// domains), starts nothing, and converges with no device host. The issuer
	// is localhost again, every output follows it, and a localhost token
	// validates.
	if err := hostinfra.Start(ctx, dir, idpSpec); err != nil {
		t.Fatalf("a run without a device host against the running IdP: %v", err)
	}
	in.DeviceHost = ""
	if _, err := Converge(ctx, in); err != nil {
		t.Fatalf("converge without a device host against the running IdP: %v", err)
	}
	t.Run("back without a device host, IdP still running: localhost issuer, accepted by the API", func(t *testing.T) {
		if _, err := signInAndValidate(t, localOrigin); err != nil {
			t.Fatalf("a localhost token was rejected after moving back: %v", err)
		}
	})
	// And after a stop and a fresh start.
	in.DeviceHost = lan
	if _, err := Converge(ctx, in); err != nil {
		t.Fatalf("converge with the device host again: %v", err)
	}
	if err := hostinfra.Stop(dir, idpSpec); err != nil {
		t.Fatal(err)
	}
	if err := hostinfra.Start(ctx, dir, idpSpec); err != nil {
		t.Fatalf("restart without a device host: %v", err)
	}
	in.DeviceHost = ""
	if _, err := Converge(ctx, in); err != nil {
		t.Fatalf("converge without a device host: %v", err)
	}
	t.Run("back after a stop and start: localhost issuer, accepted by the API", func(t *testing.T) {
		if _, err := signInAndValidate(t, localOrigin); err != nil {
			t.Fatalf("a localhost token was rejected after moving back: %v", err)
		}
	})
	// Zitadel cannot remove an instance domain, so the device origin keeps
	// answering; it is simply nobody's issuer. Pinned here so a future Zitadel
	// that does remove it is noticed rather than assumed.
	if iss := discoveryIssuer(t, deviceOrigin); iss != "" && iss != deviceOrigin {
		t.Errorf("a retained device domain must mint its own origin as issuer, got %q", iss)
	}
}

// nativeOIDCIssuer is the issuer the launch hands a native app, from the same
// published output the CLI reads (internal/cli.nativeOIDCFromOutput uses
// out.Issuer unchanged).
func nativeOIDCIssuer(out identity.Output) string { return out.Issuer }

func readStore(t *testing.T, path string) map[string]string {
	t.Helper()
	vals, err := secrets.ReadSecretFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return vals
}

// discoveryIssuer is the issuer the instance's discovery document names for a
// request whose Host is origin, "" when the instance does not answer for it.
func discoveryIssuer(t *testing.T, origin string) string {
	t.Helper()
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://localhost:"+u.Port()+"/.well-known/openid-configuration", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = u.Host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var doc struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc.Issuer
}
