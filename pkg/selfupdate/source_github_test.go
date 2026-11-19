package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubSourceLatest(t *testing.T) {
	t.Parallel()
	var auth string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/repos/reliant-labs/forge/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v1.4.0",
			"assets": []map[string]any{
				{"name": "forge_linux_amd64.tar.gz", "browser_download_url": "https://x/a", "size": 10},
				{"name": "checksums.txt", "browser_download_url": "https://x/c", "size": 1},
				{"name": "forge-release.sigstore.json", "browser_download_url": "https://x/s", "size": 2},
			},
		})
	}))
	defer srv.Close()

	src := GitHubSource{Repo: "reliant-labs/forge", Bundle: "forge-release.sigstore.json", APIBaseURL: srv.URL, Token: "tok", Client: srv.Client()}
	rel, err := src.Latest(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "1.4.0" || rel.Tag != "v1.4.0" || len(rel.Assets) != 3 {
		t.Fatalf("release = %+v", rel)
	}
	if rel.Bundle.URL != "https://x/s" {
		t.Fatalf("Bundle = %+v, want the named signature asset", rel.Bundle)
	}
	if a, ok := rel.Asset("checksums.txt"); !ok || a.URL != "https://x/c" {
		t.Fatalf("Asset(checksums.txt) = %+v, %v", a, ok)
	}
	if auth != "Bearer tok" {
		t.Fatalf("Authorization = %q", auth)
	}
}

func TestGitHubSourcePrereleaseChannel(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.4.0"},
			{"tag_name": "v1.5.0-rc.1", "prerelease": true},
			{"tag_name": "v1.6.0", "draft": true},
			{"tag_name": "v1.3.9"},
		})
	}))
	defer srv.Close()
	rel, err := GitHubSource{Repo: "o/r", APIBaseURL: srv.URL, Client: srv.Client()}.Latest(context.Background(), "prerelease")
	if err != nil {
		t.Fatal(err)
	}
	if rel.Tag != "v1.5.0-rc.1" {
		t.Fatalf("prerelease channel picked %s, want the newest non-draft v1.5.0-rc.1", rel.Tag)
	}
}

func TestGitHubSourceRefusals(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()
	ctx := context.Background()
	cases := map[string]struct {
		src     GitHubSource
		channel string
		want    string
	}{
		"bad repo":        {GitHubSource{Repo: "forge"}, "", "owner/name"},
		"unknown channel": {GitHubSource{Repo: "o/r", APIBaseURL: srv.URL, Client: srv.Client()}, "nightly", "no channel"},
		"http API":        {GitHubSource{Repo: "o/r", APIBaseURL: "http://example.com"}, "", "non-https"},
		"API error":       {GitHubSource{Repo: "o/r", APIBaseURL: srv.URL, Client: srv.Client()}, "latest", "403"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := tc.src.Latest(ctx, tc.channel)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
