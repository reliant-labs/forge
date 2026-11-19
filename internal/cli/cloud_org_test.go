package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
)

func TestCloudOrgList(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, response string
		want           []string
	}{
		{"none", `{}`, []string{"No organizations found"}},
		{"permissions", `{"organizations":[{"id":"org-a","name":"Personal","role":"owner","isDefault":true,"canCreateHostedProject":false},{"id":"org-b","name":"Team","role":"admin","canCreateHostedProject":true}]}`, []string{"org-a", "Personal", "no", "org-b", "Team", "yes", "--org <id>"}},
		{"older server", `{"organizations":[{"id":"old","role":"owner"}]}`, []string{"unknown (older server)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := &recordingCaller{resp: tc.response}
			var out bytes.Buffer
			if err := runCloudOrgList(context.Background(), caller, false, &out); err != nil {
				t.Fatal(err)
			}
			if caller.procedure != procListMyOrganizations || len(caller.req) != 0 {
				t.Fatalf("request %s %+v", caller.procedure, caller.req)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q: %s", want, out.String())
				}
			}
		})
	}
}

func TestCloudOrgListJSON(t *testing.T) {
	t.Parallel()
	caller := &recordingCaller{resp: `{"organizations":[{"id":"a","canCreateHostedProject":false},{"id":"b"}],"defaultOrgId":"a"}`}
	var out bytes.Buffer
	if err := runCloudOrgList(context.Background(), caller, true, &out); err != nil {
		t.Fatal(err)
	}
	var got struct {
		OK            bool
		Organizations []wireMyOrganization
		DefaultOrgID  string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.DefaultOrgID != "a" || len(got.Organizations) != 2 || got.Organizations[0].CanCreateHostedProject == nil || *got.Organizations[0].CanCreateHostedProject || got.Organizations[1].CanCreateHostedProject != nil {
		t.Fatalf("invalid response: %s", out.String())
	}
}

func TestCloudOrgDiscoveryDoesNotResolveAProject(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/"+procListMyOrganizations {
			t.Errorf("unexpected project lookup: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer human-session" {
			t.Error("missing human credential")
		}
		if r.Header.Get("Forge-Project-ID") != "" {
			t.Error("discovery carried project binding")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"organizations":[]}`))
	}))
	defer srv.Close()
	client := organizationDiscoveryClient(cloud.Endpoint{URL: srv.URL, ProjectName: "new-project", ProjectID: "stale-project"}, cloud.Credential{Token: "human-session", Kind: "human"})
	if err := runCloudOrgList(context.Background(), client, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestCloudOrgListCommandIsDiscoverable(t *testing.T) {
	t.Parallel()
	root := newCloudCmd()
	cmd, args, err := root.Find([]string{"org", "list"})
	if err != nil || cmd.Name() != "list" || len(args) != 0 {
		t.Fatalf("command lookup: %v %v %v", cmd, args, err)
	}
	for _, flag := range []string{"env", "token", "json"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("missing --%s", flag)
		}
	}
}
