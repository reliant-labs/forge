package cloud

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOrganizationsHelperScopeMemoRotationAndMismatch(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	useFakeHelper(t, "org", "-one")
	a := Endpoint{URL: "https://cp.example", OrgID: "A"}
	b := Endpoint{URL: a.URL, OrgID: "B"}
	for _, ep := range []Endpoint{a, b, a} {
		c, err := ResolveCredential("", ep)
		if err != nil || c.Token != ep.OrgID+"-one" || c.OrgID != ep.OrgID {
			t.Fatalf("wrong scoped helper: %v", err)
		}
	}
	// Change the helper while retaining the client; refresh must request A, not
	// the most recent B entry, and must never adopt a different organization.
	client := NewClient(a, Credential{Token: "A-one", OrgID: "A", Source: SourceHelper})
	useFakeHelper(t, "org", "-two")
	if c, ok := client.refreshedCredential(); !ok || c.Token != "A-two" || c.OrgID != "A" {
		t.Fatal("rotation crossed orgs or failed")
	}
	useFakeHelper(t, "org", "-three", "B")
	if _, ok := client.refreshedCredential(); ok {
		t.Fatal("rotation adopted mismatched org")
	}
	for _, org := range []string{"B", ""} {
		useFakeHelper(t, "raw-org", org) // bypass Serve's own guard to exercise Exec
		if _, err := ResolveCredential("", a); err == nil {
			t.Fatal("accepted helper missing/mismatched org")
		}
	}
}

func TestOrganizationsClientRejectsWrongBearerAndRequestBeforeEffect(t *testing.T) {
	effects := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/"+ProcGetOrganization {
			json.NewEncoder(w).Encode(OrganizationReply("B"))
			return
		}
		effects++
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	for _, cred := range []Credential{{Token: "opaque"}, {Token: "opaque", OrgID: "B"}} {
		c := NewClient(Endpoint{URL: srv.URL, OrgID: "A"}, cred)
		if err := c.Call(context.Background(), "Deploy/Apply", map[string]any{}, nil); err == nil {
			t.Fatal("wrong bearer acted in selected org")
		}
		if err := c.Call(context.Background(), "Deploy/Apply", map[string]any{"org_id": "B"}, nil); err == nil {
			t.Fatal("request overrode selected org")
		}
		if err := c.Call(context.Background(), ProcGetOrganization, map[string]any{}, nil); err == nil {
			t.Fatal("organization response mismatch accepted")
		}
	}
	if effects != 0 {
		t.Fatal("effect before validating org")
	}
}

func TestOrganizationsRequestCarriesChoiceAndAnonymousRemainsUnscoped(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if calls < 2 {
			if req["orgId"] != "A" || r.Header.Get("Authorization") != "Bearer scoped" {
				t.Error("scoped request lost organization or bearer")
			}
		} else if req["orgId"] != nil || r.Header.Get("Authorization") != "" {
			t.Error("anonymous request inherited organization or bearer")
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/"+ProcGetOrganization {
			json.NewEncoder(w).Encode(OrganizationReply("A"))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c := NewClient(Endpoint{URL: srv.URL, OrgID: "A"}, Credential{Token: "scoped", OrgID: "A"})
	if err := c.Call(context.Background(), "controlplane.v1.DeployService/ListReleases", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.CallAnonymous(context.Background(), "controlplane.v1.DeployService/ListReleases", map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("calls: %d", calls)
	}
}

func TestOrganizationsAnonymousNeverFallsIntoSelectedOrg(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv(DefaultTokenEnv, "")
	ep := Endpoint{URL: "https://cp.example"}
	restore := UseClaimableCredential(ep, "anonymous", nil)
	defer restore()
	if c, err := ResolveCredential("", ep); err != nil || c.Token != "anonymous" {
		t.Fatal("anonymous flow broken")
	}
	ep.OrgID = "B"
	if _, err := ResolveCredential("", ep); err == nil {
		t.Fatal("anonymous token used for selected ordinary org")
	}
}
