package devidentity

// The native application against the real host Zitadel: the registered client
// is a public PKCE client that takes a private-use callback, signs in at the
// IdP's OWN pages (not the web sign-in), and a second pass writes nothing.
//
// Skipped under -short: it boots host postgres and the pinned Zitadel.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/hostinfra"
	"github.com/reliant-labs/forge/internal/templates"
	"github.com/reliant-labs/forge/pkg/identity"
	"github.com/reliant-labs/forge/pkg/release"
)

func TestConverge_NativeApplicationAgainstTheRealHostZitadel(t *testing.T) {
	if testing.Short() {
		t.Skip("boots host postgres and the pinned Zitadel; runs in task test")
	}
	if runtime.GOOS == "windows" {
		t.Skip("host Zitadel is exercised on unix")
	}

	dir := t.TempDir()
	steps, err := templates.ProjectTemplates().Render("idp-steps.yaml.tmpl", struct{ ProjectName string }{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "idp-steps.yaml"), steps, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	pgSpec := hostinfra.Spec{Name: "postgres", Engine: hostinfra.EnginePostgres, Port: freeLoopbackPort(t), Database: "acme", User: "postgres", Password: "postgres"}
	idpSpec := hostinfra.Spec{
		Name: "idp", Engine: hostinfra.EngineZitadel, Port: freeLoopbackPort(t),
		IDPDatabase: "zitadel", IDPDatabasePort: pgSpec.Port, IDPMasterKey: "MasterkeyNeedsToHave32Characters",
		IDPStepsFile: "idp-steps.yaml", IDPPATPath: ".forge/idp/pat.txt",
		Database: "postgres", User: "postgres", Password: "postgres",
	}
	t.Cleanup(func() {
		_ = hostinfra.Stop(dir, idpSpec)
		_ = hostinfra.Stop(dir, pgSpec)
	})
	if err := hostinfra.Start(ctx, dir, pgSpec); err != nil {
		t.Skipf("host postgres is unavailable here: %v", err)
	}
	if err := hostinfra.Start(ctx, dir, idpSpec); err != nil {
		t.Skipf("host Zitadel is unavailable here: %v", err)
	}

	const frontend = "http://localhost:3000"
	sh := &release.ShapeIdentity{
		Name: "identity", Runtime: release.IdentityRuntimeHost,
		Login: release.ShapeIdentityLogin{Mode: release.IdentityLoginNative},
		Roles: []release.ShapeIdentityRole{{Key: "admin", MembersFrom: release.IdentityMembersConsole}},
		Applications: []release.ShapeIdentityApplication{
			{Name: "dev:api", Workload: "api", Kind: release.IdentityAppNativeBroker},
			// Declared with a private-use and an https callback side by side:
			// the https one must converge with no code change.
			{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile",
				RedirectURIs: []string{"com.acme.mobile:/oauthredirect", "https://acme.example/oauthredirect"}},
			// Derived from the app id: no callback declared.
			{Name: "dev:other", Kind: release.IdentityAppNative, Frontend: "other"},
		},
	}
	in := Input{
		Project: "acme", Identity: sh,
		BaseURL: "http://localhost:" + itoa(idpSpec.Port), PATPath: idpSpec.PATPath(dir),
		FrontendOrigin:   frontend,
		NativeAppSchemes: map[string]string{"other": "com.acme.other"},
		StorePath:        filepath.Join(dir, "secrets", "dev.yaml"),
		OutputsPath:      filepath.Join(dir, ".forge", "identity", "identity.json"),
		KeysDir:          filepath.Join(dir, ".forge", "identity", "identity.keys"),
	}
	first, err := Converge(ctx, in)
	if err != nil {
		t.Fatalf("first converge: %v", err)
	}
	if !first.Report.Changed() {
		t.Fatal("a fresh instance converged without a single change")
	}

	var out identity.Output
	raw, err := os.ReadFile(in.OutputsPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	mobile, other := out.Apps["dev:mobile"], out.Apps["dev:other"]
	if mobile.ClientID == "" || other.ClientID == "" || mobile.ClientID == other.ClientID {
		t.Fatalf("client ids: mobile %q other %q", mobile.ClientID, other.ClientID)
	}
	if mobile.LoginURI != "" || other.LoginURI != "" {
		t.Errorf("a native app has no web sign-in route: %q %q", mobile.LoginURI, other.LoginURI)
	}
	if strings.Join(mobile.RedirectURIs, " ") != "com.acme.mobile:/oauthredirect https://acme.example/oauthredirect" {
		t.Errorf("mobile redirects = %v", mobile.RedirectURIs)
	}
	if strings.Join(other.RedirectURIs, " ") != "com.acme.other:/oauthredirect" || strings.Join(other.PostLogoutURIs, " ") != "com.acme.other:/signedout" {
		t.Errorf("derived callbacks = %v / %v", other.RedirectURIs, other.PostLogoutURIs)
	}
	if !contains(out.Scopes, identity.AudienceScope(out.ProjectID)) {
		t.Errorf("the published scopes carry no audience scope: %v", out.Scopes)
	}

	// The client the app would run with: authorization code + PKCE S256, a
	// private-use redirect. /authorize must land on the IdP's own login, not
	// bounce into the web app's /auth/sign-in (which cannot finish a flow it
	// did not start).
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	for _, redirect := range mobile.RedirectURIs {
		q := url.Values{
			"client_id": {mobile.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect},
			"scope":          {"openid profile email offline_access " + identity.AudienceScope(out.ProjectID)},
			"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {"s"},
		}
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Get(in.BaseURL + "/oauth/v2/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode/100 != 3 || !strings.HasPrefix(loc, "/ui/login/") || strings.Contains(loc, "error") {
			t.Errorf("authorize with %s: %d -> %q, want a redirect to the IdP's own login pages", redirect, resp.StatusCode, loc)
		}
	}
	// A redirect nobody registered is refused rather than honoured.
	q := url.Values{"client_id": {mobile.ClientID}, "response_type": {"code"}, "redirect_uri": {"com.evil.app:/oauthredirect"}, "scope": {"openid"}, "code_challenge": {"x"}, "code_challenge_method": {"S256"}}
	bad := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if resp, err := bad.Get(in.BaseURL + "/oauth/v2/authorize?" + q.Encode()); err == nil {
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if strings.HasPrefix(loc, "com.evil.app:") {
			t.Errorf("an unregistered redirect was honoured: %q", loc)
		}
	}

	second, err := Converge(ctx, in)
	if err != nil {
		t.Fatalf("second converge: %v", err)
	}
	if second.Report.Changed() {
		t.Errorf("a converged instance was written to again: %v", second.Report.Changes)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
