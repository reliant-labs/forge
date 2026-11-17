package deploytarget

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
)

// fakeRealm is a registry and its token realm on one loopback server (plain
// HTTP, as plainHTTPRegistry answers for 127.x): /v2/ challenges, and
// /auth/token issues a JWT whose access claim grants grants[repo], narrowing
// like the platform's realm does rather than refusing.
type fakeRealm struct {
	srv        *httptest.Server
	host       string
	password   string              // the one valid basic-auth password
	grants     map[string][]string // repository path → actions granted
	opaque     bool                // issue a non-JWT token
	challenge  string              // overrides the WWW-Authenticate header
	gotScopes  []string
	gotService string
}

func newFakeRealm(t *testing.T) *fakeRealm {
	t.Helper()
	f := &fakeRealm{password: "good", grants: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	f.host = strings.TrimPrefix(f.srv.URL, "http://")
	return f
}

func (f *fakeRealm) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v2/":
		c := f.challenge
		if c == "" {
			c = fmt.Sprintf(`Bearer realm="%s/auth/token",service="%s"`, f.srv.URL, f.host)
		}
		w.Header().Set("WWW-Authenticate", c)
		w.WriteHeader(http.StatusUnauthorized)
	case "/auth/token":
		f.gotScopes = r.URL.Query()["scope"]
		f.gotService = r.URL.Query().Get("service")
		if _, pass, ok := r.BasicAuth(); !ok || pass != f.password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"errors":[{"code":"UNAUTHORIZED","message":"the supplied credential is not valid"}]}`))
			return
		}
		token := "opaque-token"
		if !f.opaque {
			var access []tokenGrant
			for _, s := range f.gotScopes {
				path := strings.TrimSuffix(strings.TrimPrefix(s, "repository:"), ":pull,push")
				if actions := f.grants[path]; len(actions) > 0 {
					access = append(access, tokenGrant{Type: "repository", Name: path, Actions: actions})
				}
			}
			claims, _ := json.Marshal(map[string]any{"access": access})
			token = "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": token, "access_token": token})
	default:
		http.NotFound(w, r)
	}
}

func staticCred(c auth.Credential) auth.CredentialFunc {
	return func(context.Context, string) (auth.Credential, error) { return c, nil }
}

var goodCred = auth.Credential{Username: "forge", Password: "good"}

func (f *fakeRealm) repos() (site, maps string) {
	return f.host + "/org/proj/web/static.v1", f.host + "/org/proj/web-sourcemaps"
}

func TestProbePushAccess_GrantedWhenEveryRepositoryGrantsPush(t *testing.T) {
	t.Parallel()
	f := newFakeRealm(t)
	f.grants["org/proj/web/static.v1"] = []string{"pull", "push"}
	f.grants["org/proj/web-sourcemaps"] = []string{"pull", "push"}
	site, maps := f.repos()
	got := probePushAccess(context.Background(), f.srv.Client(), staticCred(goodCred), f.host,
		[]string{site, maps, "ghcr.io/acme/elsewhere"})
	if got.Verdict != PushAccessGranted {
		t.Fatalf("verdict = %+v, want granted", got)
	}
	want := []string{"repository:org/proj/web/static.v1:pull,push", "repository:org/proj/web-sourcemaps:pull,push"}
	if !slices.Equal(f.gotScopes, want) || f.gotService != f.host {
		t.Errorf("realm asked for scopes %v service %q; want %v service %q (another host's repository is not asked about)",
			f.gotScopes, f.gotService, want, f.host)
	}
}

// A realm narrows rather than refuses: the token is issued, without push on
// the source-map repository. That is the authoritative "no".
func TestProbePushAccess_DeniedNamesTheRepositoryWithoutPush(t *testing.T) {
	t.Parallel()
	f := newFakeRealm(t)
	f.grants["org/proj/web/static.v1"] = []string{"pull", "push"}
	f.grants["org/proj/web-sourcemaps"] = []string{"pull"}
	site, maps := f.repos()
	got := probePushAccess(context.Background(), f.srv.Client(), staticCred(goodCred), f.host, []string{site, maps})
	if got.Verdict != PushAccessDenied || !slices.Equal(got.Missing, []string{maps}) || !strings.Contains(got.Reason, maps) {
		t.Fatalf("got %+v, want denied naming %s", got, maps)
	}
}

func TestProbePushAccess_DeniedWhenTheRealmRefusesTheCredential(t *testing.T) {
	t.Parallel()
	f := newFakeRealm(t)
	site, _ := f.repos()
	got := probePushAccess(context.Background(), f.srv.Client(), staticCred(auth.Credential{Username: "forge", Password: "revoked"}), f.host, []string{site})
	if got.Verdict != PushAccessDenied || !strings.Contains(got.Reason, "the supplied credential is not valid") {
		t.Fatalf("got %+v, want denied carrying the realm's message", got)
	}
	got = probePushAccess(context.Background(), f.srv.Client(), staticCred(auth.EmptyCredential), f.host, []string{site})
	if got.Verdict != PushAccessDenied || !strings.Contains(got.Reason, "no credential") {
		t.Fatalf("no credential: got %+v, want denied", got)
	}
}

// Everything that is not an answer is Unknown, never a refusal.
func TestProbePushAccess_UnknownWhenThereIsNoAuthoritativeAnswer(t *testing.T) {
	t.Parallel()
	t.Run("opaque token", func(t *testing.T) {
		t.Parallel()
		f := newFakeRealm(t)
		f.opaque = true
		site, _ := f.repos()
		if got := probePushAccess(context.Background(), f.srv.Client(), staticCred(goodCred), f.host, []string{site}); got.Verdict != PushAccessUnknown {
			t.Errorf("got %+v, want unknown", got)
		}
	})
	t.Run("not a bearer challenge", func(t *testing.T) {
		t.Parallel()
		f := newFakeRealm(t)
		f.challenge = `Basic realm="registry"`
		site, _ := f.repos()
		if got := probePushAccess(context.Background(), f.srv.Client(), staticCred(goodCred), f.host, []string{site}); got.Verdict != PushAccessUnknown {
			t.Errorf("got %+v, want unknown", got)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		t.Parallel()
		f := newFakeRealm(t)
		site, _ := f.repos()
		f.srv.Close()
		got := probePushAccess(context.Background(), &http.Client{}, staticCred(goodCred), f.host, []string{site})
		if got.Verdict != PushAccessUnknown || !strings.Contains(got.Reason, "could not reach") {
			t.Errorf("got %+v, want unknown: could not reach", got)
		}
	})
}

// The probe presents the credential the pushes present: docker's reading of
// the store, where an EMPTY credHelpers entry pins the host to config.json
// (dockercred). The credsStore here does not exist, so reading it — oras's
// own reading — would fail; docker's reading finds the good login.
func TestProbePushAccess_ReadsTheCredentialTheWayThePushesDo(t *testing.T) {
	f := newFakeRealm(t)
	f.grants["org/proj/web/static.v1"] = []string{"pull", "push"}
	dir := t.TempDir()
	cfg := fmt.Sprintf(`{"auths":{%q:{"auth":%q}},"credsStore":"forge-test-no-such-helper","credHelpers":{%q:""}}`,
		f.host, base64.StdEncoding.EncodeToString([]byte("forge:good")), f.host)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
	site, _ := f.repos()
	if got := ProbePushAccess(context.Background(), f.host, []string{site}); got.Verdict != PushAccessGranted {
		t.Fatalf("got %+v, want granted with the config.json login", got)
	}
}

func TestParseChallenge(t *testing.T) {
	t.Parallel()
	scheme, p := parseChallenge(`Bearer realm="https://r.example/auth/token",service="r.example",scope="repository:a/b:pull,push"`)
	if scheme != "Bearer" || p["realm"] != "https://r.example/auth/token" || p["service"] != "r.example" || p["scope"] != "repository:a/b:pull,push" {
		t.Errorf("scheme %q params %v", scheme, p)
	}
	if _, p := parseChallenge(`Bearer realm=https://r.example/t, service=r.example`); p["realm"] != "https://r.example/t" || p["service"] != "r.example" {
		t.Errorf("unquoted params %v", p)
	}
}
