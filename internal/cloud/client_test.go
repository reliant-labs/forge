package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClient_Call_AuthenticatesAndReturnsData drives the real transport
// against a test server that speaks Connect's HTTP+JSON binding: the
// procedure path, the Authorization header, the JSON request and the
// JSON reply.
func TestClient_Call_AuthenticatesAndReturnsData(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"releases":[{"id":"rel_1","version":"v1.4.0","gitCommit":"abc123def456"}]}`))
	}))
	defer srv.Close()

	ep, err := ResolveEndpoint("prod", &Declaration{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(ep, Credential{Token: "tok-123", Source: SourceEnv, From: "ACME_TOKEN"})

	var out struct {
		Releases []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"releases"`
	}
	if err := client.Call(context.Background(),
		"controlplane.v1.DeployService/ListReleases", map[string]any{"limit": 5}, &out); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if gotPath != "/controlplane.v1.DeployService/ListReleases" {
		t.Errorf("procedure path: got %q", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("Authorization header: got %q, want %q", gotAuth, "Bearer tok-123")
	}
	if !strings.Contains(gotBody, `"limit":5`) {
		t.Errorf("request body should carry the limit; got %q", gotBody)
	}
	if len(out.Releases) != 1 || out.Releases[0].Version != "v1.4.0" {
		t.Fatalf("decoded response: %+v", out)
	}
}

// TestClient_Call_AuthFailureIsActionable: a bare 401 leaves a user
// unable to tell a missing credential from a wrong one, so the message
// must name the endpoint, where the credential came from, and both ways
// to supply a better one.
func TestClient_Call_AuthFailureIsActionable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "unauthenticated", "message": "token expired",
		})
	}))
	defer srv.Close()

	ep, err := ResolveEndpoint("prod", &Declaration{Endpoint: srv.URL, TokenEnv: "ACME_PROD_TOKEN"})
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(ep, Credential{Token: "stale", Source: SourceLogin, From: "/home/u/.forge/login.json"})

	err = client.Call(context.Background(), "controlplane.v1.DeployService/ListReleases", map[string]any{}, &struct{}{})
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	msg := err.Error()
	for _, want := range []string{"forge login", "ACME_PROD_TOKEN", srv.URL, "/home/u/.forge/login.json", "token expired"} {
		if !strings.Contains(msg, want) {
			t.Errorf("auth error should contain %q; got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "stale") {
		t.Errorf("the error must never echo the credential itself; got:\n%s", msg)
	}
}

// TestClient_Call_UnimplementedIsReportedAsSuch — the hosted API has
// real scaffold stubs, so hitting one must read as "the server does not
// implement this", not as a forge bug.
func TestClient_Call_UnimplementedIsReportedAsSuch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code": "unimplemented", "message": "handler for ListDeployments not yet implemented",
		})
	}))
	defer srv.Close()

	ep, _ := ResolveEndpoint("prod", &Declaration{Endpoint: srv.URL})
	client := NewClient(ep, Credential{Token: "t"})
	err := client.Call(context.Background(), "controlplane.v1.DeployService/ListDeployments", map[string]any{}, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("want an unimplemented-shaped error; got %v", err)
	}
}

// TestClient_Call_ForwardsDeclaredOrganization — the org hint is a
// non-sensitive per-env declaration, so it rides as a header.
func TestClient_Call_ForwardsDeclaredOrganization(t *testing.T) {
	var gotOrg string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOrg = r.Header.Get("X-Forge-Organization")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ep, _ := ResolveEndpoint("prod", &Declaration{Endpoint: srv.URL, Organization: "acme"})
	client := NewClient(ep, Credential{Token: "t"})
	if err := client.Call(context.Background(), "controlplane.v1.DeployService/ListReleases", map[string]any{}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	if gotOrg != "acme" {
		t.Errorf("declared organization should be forwarded; got %q", gotOrg)
	}
}
