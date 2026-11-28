package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestCovDHTTPClientFetcher_SendsHeadersAndReturnsStatusBodyAndHeaders(t *testing.T) {
	var gotAccept, gotAuth, gotAgent, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccept, gotAuth, gotAgent, gotMethod = r.Header.Get("Accept"), r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Method
		w.Header().Set("Docker-Content-Digest", "sha256:abc")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	f := newHTTPFetcher(5 * time.Second)
	resp, err := f.Fetch(context.Background(), fetchRequest{URL: srv.URL + "/v2/x", Accept: "application/json", Bearer: "tok"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound || string(resp.Body) != "nope" || resp.Header.Get("Docker-Content-Digest") != "sha256:abc" {
		t.Errorf("resp = %+v", resp)
	}
	if gotMethod != http.MethodGet || gotAccept != "application/json" || gotAuth != "Bearer tok" || gotAgent != "forge-release-verify" {
		t.Errorf("request: %s accept=%q auth=%q agent=%q", gotMethod, gotAccept, gotAuth, gotAgent)
	}

	// Optional headers are omitted when not asked for.
	if _, err := f.Fetch(context.Background(), fetchRequest{URL: srv.URL}); err != nil {
		t.Fatal(err)
	}
	if gotAccept != "" || gotAuth != "" {
		t.Errorf("an unauthenticated plain request must send no Accept or Authorization: accept=%q auth=%q", gotAccept, gotAuth)
	}
}

func TestCovDHTTPClientFetcher_BodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		chunk := []byte(strings.Repeat("x", 1<<20))
		for i := 0; i < 20; i++ {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	resp, err := newHTTPFetcher(10*time.Second).Fetch(context.Background(), fetchRequest{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Body) != maxVerifyBodyBytes {
		t.Errorf("body length = %d, want the %d-byte cap", len(resp.Body), maxVerifyBodyBytes)
	}
}

func TestCovDHTTPClientFetcher_TransportErrors(t *testing.T) {
	f := newHTTPFetcher(5 * time.Second)
	if _, err := f.Fetch(context.Background(), fetchRequest{URL: "http://[::1"}); err == nil {
		t.Error("a malformed URL must fail before any request")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	if _, err := f.Fetch(context.Background(), fetchRequest{URL: url}); err == nil {
		t.Error("a refused connection is a transport error, not a verdict")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Fetch(ctx, fetchRequest{URL: url}); err == nil {
		t.Error("a cancelled context must fail")
	}

	// A body that dies mid-stream is a transport failure.
	trunc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer trunc.Close()
	if _, err := f.Fetch(context.Background(), fetchRequest{URL: trunc.URL}); err == nil {
		t.Error("a truncated body must be reported as an error")
	}
}

func TestCovDVerifyReleaseArtifacts_CatalogReleasesVerifyByArtifactID(t *testing.T) {
	rel := release.Release{Catalog: map[release.ArtifactID]release.SnapshotArtifact{
		"id-file": {Kind: release.KindFile, Location: "", Content: "sha256:aa"},
		"id-git":  {Kind: release.KindGit, Location: "github.com/acme/x", Content: strings.Repeat("a", 40)},
		"id-new":  {Kind: release.Kind("future"), Location: "somewhere", Content: "z"},
	}}
	got := verifyReleaseArtifacts(context.Background(), nil, rel, 0)
	if len(got) != 3 || got[0].Name != "id-file" || got[1].Name != "id-git" || got[2].Name != "id-new" {
		t.Fatalf("results must be keyed by artifact id and sorted: %+v", got)
	}
	for _, r := range got {
		if r.Status != verifyUnverifiable {
			t.Errorf("%s: status = %s, want unverifiable (no network was reachable for these kinds)", r.Name, r.Status)
		}
	}
	if tally := tallyVerifications(got); tally.Unverifiable != 3 || tally.Failed != 0 {
		t.Errorf("tally = %+v", tally)
	}
}

func TestCovDVerifyFileArtifact_ExplainsWhatIsMissing(t *testing.T) {
	cases := []struct {
		art  release.Artifact
		want string
	}{
		{release.Artifact{}, "neither a publish destination nor a hash"},
		{release.Artifact{Integrity: "sha256-xx"}, "sha256-xx recorded, but no publish destination"},
		{release.Artifact{URI: "https://x/y", Integrity: "sha256-xx"}, "downloading file artifacts is not implemented"},
	}
	for _, c := range cases {
		r := verifyFileArtifact("f", c.art)
		if r.Status != verifyUnverifiable || r.Kind != "file" || !strings.Contains(r.Detail, c.want) {
			t.Errorf("%+v -> %+v, want unverifiable containing %q", c.art, r, c.want)
		}
	}
}
