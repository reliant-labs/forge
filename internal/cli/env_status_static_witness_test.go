package cli

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// hostedSiteSnapshot is a snapshot whose hosted tree holds one static site,
// web, built by release builtBy, and one hosted service, api, which forge has
// no witness for.
func hostedSiteSnapshot(t *testing.T, builtBy string) release.Snapshot {
	t.Helper()
	s := release.Snapshot{Artifacts: map[release.ArtifactID]release.SnapshotArtifact{}}
	add := func(workload, slot string, a release.SnapshotArtifact) {
		id, err := release.ArtifactIdentity(a)
		if err != nil {
			t.Fatal(err)
		}
		s.Artifacts[id] = a
		k := release.ConsumerKey{Cluster: release.BundleHostedCluster, Workload: workload}
		s.Consumers = append(s.Consumers, release.Consumer{Key: k, Slots: []string{slot}, DependenciesDeclared: true})
		s.Bindings = append(s.Bindings, release.ConsumerBinding{Consumer: k, Slot: slot, Artifact: id})
	}
	add("web", "frontend", release.SnapshotArtifact{Kind: release.KindOCI, Location: "registry.example/acme/static.v1/web",
		Content: "sha256:" + strings.Repeat("a", 64), Origin: release.ArtifactOrigin{Release: builtBy}})
	add("api", "container:api", release.SnapshotArtifact{Kind: release.KindOCI, Location: "registry.example/acme/api",
		Content: "sha256:" + strings.Repeat("b", 64), Origin: release.ArtifactOrigin{Release: builtBy}})
	return s
}

func siteVerdict(t *testing.T, results []imageVerification) imageVerification {
	t.Helper()
	for _, r := range results {
		if r.Image == "web/frontend" {
			return r
		}
	}
	t.Fatalf("no verdict for web/frontend in %+v", results)
	return imageVerification{}
}

// forge#2231: a hosted static site is verified against the release marker its
// live URL serves, instead of being UNREACHABLE forever.
func TestEnvStatusWitnessesAHostedStaticSiteByItsReleaseMarker(t *testing.T) {
	marker := func(rel string) func(context.Context, string) (deploytarget.StaticSiteReleaseMarker, error) {
		return func(_ context.Context, site string) (deploytarget.StaticSiteReleaseMarker, error) {
			if site != "https://web.acme.example" {
				t.Errorf("fetched %s, want the site's live URL", site)
			}
			return deploytarget.StaticSiteReleaseMarker{Schema: deploytarget.StaticSiteReleaseMarkerSchema, Frontend: "web", Release: rel}, nil
		}
	}
	liveURL := func(context.Context, string) (map[string]string, error) {
		return map[string]string{"web": "https://web.acme.example"}, nil
	}
	for name, tc := range map[string]struct {
		urls     func(context.Context, string) (map[string]string, error)
		fetch    func(context.Context, string) (deploytarget.StaticSiteReleaseMarker, error)
		want     imageState
		detail   string
		exitCode int
	}{
		"the site serves the bound release": {urls: liveURL, fetch: marker("r2"), want: imageMatch, detail: "serves release r2"},
		"the site serves another release":   {urls: liveURL, fetch: marker("r1"), want: imageDrift, detail: "serves release r1; the snapshot binds release r2", exitCode: 1},
		"the site serves no marker": {urls: liveURL, want: imageUnreachable, detail: "serves no forge release marker", exitCode: 2,
			fetch: func(context.Context, string) (deploytarget.StaticSiteReleaseMarker, error) {
				return deploytarget.StaticSiteReleaseMarker{}, errNoReleaseMarker
			}},
		"the control plane reports no URL yet": {want: imageUnreachable, detail: "no live URL", exitCode: 2, fetch: marker("r2"),
			urls: func(context.Context, string) (map[string]string, error) { return map[string]string{}, nil }},
		"the control plane cannot be read": {want: imageUnreachable, detail: "connection refused", exitCode: 2, fetch: marker("r2"),
			urls: func(context.Context, string) (map[string]string, error) { return nil, errors.New("connection refused") }},
	} {
		t.Run(name, func(t *testing.T) {
			s := hostedSiteSnapshot(t, "r2")
			results := snapshotStatusImages(context.Background(), s, "prod", envStatusOptions{StaticSiteURLs: tc.urls, FetchReleaseMarker: tc.fetch})
			got := siteVerdict(t, results)
			if got.State != tc.want || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("web/frontend = %s (%s), want %s containing %q", got.State, got.Detail, tc.want, tc.detail)
			}
			// The hosted service still has no witness: unreachable, as before.
			for _, r := range results {
				if r.Image == "api/container:api" && r.State != imageUnreachable {
					t.Errorf("api/container:api = %s, want unreachable", r.State)
				}
			}
			only := []imageVerification{got}
			err := envStatusReleaseVerdict("prod", "r2", nil, tallyEnvStatusRelease(only))
			if code := exitCodeForError(err); (err == nil && tc.exitCode != 0) || (err != nil && code != tc.exitCode) {
				t.Errorf("exit = %d (%v), want %d", code, err, tc.exitCode)
			}
		})
	}
}

// A carried site keeps the release that built it: the marker is compared with
// the artifact's release, not the env's newest one.
func TestEnvStatusComparesAStaticSiteWithTheReleaseThatBuiltIt(t *testing.T) {
	s := hostedSiteSnapshot(t, "r1")
	got := siteVerdict(t, snapshotStatusImages(context.Background(), s, "prod", envStatusOptions{
		StaticSiteURLs: func(context.Context, string) (map[string]string, error) {
			return map[string]string{"web": "https://web.acme.example"}, nil
		},
		FetchReleaseMarker: func(context.Context, string) (deploytarget.StaticSiteReleaseMarker, error) {
			return deploytarget.StaticSiteReleaseMarker{Schema: deploytarget.StaticSiteReleaseMarkerSchema, Frontend: "web", Release: "r1"}, nil
		},
	}))
	if got.State != imageMatch {
		t.Fatalf("carried site = %s (%s), want match", got.State, got.Detail)
	}
}

// The HTTP half: the marker path, a 404 as "no marker", and an HTML page in
// its place as unreadable, never as a release.
func TestFetchStaticReleaseMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok/.well-known/forge-release.json":
			if r.Header.Get("Cache-Control") != "no-cache" {
				t.Errorf("request Cache-Control = %q, want no-cache", r.Header.Get("Cache-Control"))
			}
			_, _ = w.Write([]byte(`{"schema":"forge.static-site-release/v1","frontend":"web","release":"r2"}`))
		case "/html/.well-known/forge-release.json":
			_, _ = w.Write([]byte("<!doctype html><title>spa</title>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if m, err := fetchStaticReleaseMarker(ctx, srv.URL+"/ok/"); err != nil || m.Release != "r2" || m.Frontend != "web" {
		t.Errorf("ok: %+v, %v", m, err)
	}
	if _, err := fetchStaticReleaseMarker(ctx, srv.URL+"/missing"); !errors.Is(err, errNoReleaseMarker) {
		t.Errorf("missing: err = %v, want errNoReleaseMarker", err)
	}
	if _, err := fetchStaticReleaseMarker(ctx, srv.URL+"/html"); err == nil || errors.Is(err, errNoReleaseMarker) {
		t.Errorf("html: err = %v, want a parse error", err)
	}
}
