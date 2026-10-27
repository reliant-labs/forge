package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/pkg/credentials"
)

func TestOrganizationsLoginSelectionRotationLogout(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	t.Setenv("FORGE_CREDENTIAL_HELPER", "")
	t.Setenv(cloud.DefaultTokenEnv, "")
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "forge.yaml"), []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	withFakeBrowser(t)
	var chosen string
	generation := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize":
			chosen = r.URL.Query().Get("org")
			if chosen == "" {
				chosen = "B"
			} // authoritative server default, not the oldest/first credential
			back, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
			back.RawQuery = url.Values{"code": {"ok"}, "state": {r.URL.Query().Get("state")}}.Encode()
			http.Redirect(w, r, back.String(), http.StatusFound)
		case "/oauth/token":
			generation++
			token := chosen
			if generation > 2 {
				token += "-rotated"
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": token, "org_id": chosen, "token_type": "Bearer"})
		case "/" + cloud.ProcGetOrganization:
			org := r.Header.Get("Authorization")[7:8]
			json.NewEncoder(w).Encode(cloud.OrganizationReply(org))
		default:
			json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	defer srv.Close()
	path, _ := cloud.CredentialsPath()
	for _, org := range []string{"A", ""} {
		args := []string{"login", "--endpoint", srv.URL}
		if org != "" {
			args = append(args, "--org", org)
		}
		if out, err := runLoginCmd(t, args...); err != nil {
			t.Fatalf("login: %v %s", err, out)
		}
	}
	ep, err := projectCloudEndpoint("prod", &cloud.Declaration{Endpoint: srv.URL})
	if err != nil || ep.OrgID != "" {
		t.Fatalf("login retargeted project: %v", err)
	}
	if _, err := cloud.ResolveCredential("", ep); !errors.Is(err, credentials.ErrOrgRequired) {
		t.Fatalf("guessed org: %v", err)
	}
	for _, org := range []string{"A", "B"} {
		cmd := newOrgCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"use", org, "--endpoint", srv.URL})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		ep, err = projectCloudEndpoint("prod", &cloud.Declaration{Endpoint: srv.URL})
		if err != nil || ep.OrgID != org {
			t.Fatalf("choice lost: %v", err)
		}
		cred, err := cloud.ResolveCredential("", ep)
		if err != nil || cred.Token != org {
			t.Fatalf("wrong org credential: %v", err)
		}
	}
	if _, err := runLoginCmd(t, "login", "--org", "A", "--endpoint", srv.URL); err != nil {
		t.Fatal(err)
	}
	ep, _ = projectCloudEndpoint("prod", &cloud.Declaration{Endpoint: srv.URL})
	if ep.OrgID != "B" {
		t.Fatal("relogin retargeted project")
	}
	if c, err := credentials.Lookup(path, srv.URL, cloud.ClientID, "A"); err != nil || c.Token != "A-rotated" {
		t.Fatalf("rotation: %v", err)
	}
	if _, err := runLoginCmd(t, "logout", "--org", "A", "--endpoint", srv.URL); err != nil {
		t.Fatal(err)
	}
	if c, err := credentials.Lookup(path, srv.URL, cloud.ClientID, "B"); err != nil || c.Token != "B" {
		t.Fatalf("logout lost B: %v", err)
	}
	if _, err := credentials.Lookup(path, srv.URL, cloud.ClientID, "A"); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatalf("logout kept A: %v", err)
	}
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "forge.yaml"), []byte("name: other\n"), 0o600)
	t.Chdir(other)
	ep, err = projectCloudEndpoint("prod", &cloud.Declaration{Endpoint: srv.URL})
	if err != nil || ep.OrgID != "" {
		t.Fatal("project choice leaked to another project")
	}
}

func TestOrganizationsRegistryLoginMemoIncludesOrgAndRotation(t *testing.T) {
	path := isolatedCredentials(t)
	t.Setenv("FORGE_CREDENTIAL_HELPER", "")
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "forge.yaml"), []byte("name: test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)
	calls := stubDockerLoginArgs(t)
	mismatch := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		org := r.Header.Get("Authorization")[7:8]
		if mismatch {
			org = "wrong"
		}
		json.NewEncoder(w).Encode(cloud.OrganizationReply(org))
	}))
	defer srv.Close()
	for _, org := range []string{"A", "B"} {
		if err := credentials.Store(path, srv.URL, cloud.ClientID, credentials.Credential{OrgID: org, Token: org}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []string{"A", "A", "B", "A", "B"} {
		cmd := newOrgCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"use", org, "--endpoint", srv.URL})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if err := loginToPlatformRegistry(context.Background(), "prod", platformHost, "", &cloud.Declaration{Endpoint: srv.URL}); err != nil {
			t.Fatal(err)
		}
	}
	if len(*calls) != 4 {
		t.Fatalf("registry memo crossed orgs: %d", len(*calls))
	}
	if err := credentials.Store(path, srv.URL, cloud.ClientID, credentials.Credential{OrgID: "B", Token: "B-rotated"}); err != nil {
		t.Fatal(err)
	}
	if err := loginToPlatformRegistry(context.Background(), "prod", platformHost, "", &cloud.Declaration{Endpoint: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 5 {
		t.Fatal("registry memo retained old token after rotation")
	}
	mismatch = true
	if err := loginToPlatformRegistry(context.Background(), "prod", platformHost, "A-foreign", &cloud.Declaration{Endpoint: srv.URL}); err == nil {
		t.Fatal("registry accepted wrong org token")
	}
	if len(*calls) != 5 {
		t.Fatal("registry logged in before rejecting wrong org")
	}
}

func TestOrganizationsLoginRejectsDifferentApprovedOrg(t *testing.T) {
	t.Setenv("FORGE_HOME", t.TempDir())
	srv := loginTestCP(t, "secret", "") // no org_id is not evidence of requested org
	withFakeBrowser(t)
	if _, err := runLoginCmd(t, "login", "--org", "A", "--endpoint", srv.URL); err == nil {
		t.Fatal("accepted missing organization")
	}
	if _, err := lookupStored(t, srv.URL); !errors.Is(err, credentials.ErrNotFound) {
		t.Fatal("mismatched credential was stored")
	}
}
