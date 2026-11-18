package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

const (
	witnessDigest = "sha256:" + "aa00000000000000000000000000000000000000000000000000000000000000"
	witnessTree   = "sha256:7e1"
)

func fakeStaticWitness(code int, body string, tree string, statusErr, treeErr error) (*httpStaticSiteWitness, *string) {
	var fetched string
	return &httpStaticSiteWitness{
		env: "prod",
		status: func(context.Context) (deploytarget.HostedEnvStatus, error) {
			return deploytarget.HostedEnvStatus{Workloads: []deploytarget.HostedWorkloadStatus{
				{Name: "api", URL: "https://api.example"},
				{Name: "landing", Tier: "static", URL: "https://landing.example/"},
			}}, statusErr
		},
		fetch: func(_ context.Context, url string) (int, []byte, error) {
			fetched = url
			return code, []byte(body), nil
		},
		tree: func(_ context.Context, repo, digest string) (string, error) {
			if repo != "reg.example/org/landing/static.v1" || digest != witnessDigest {
				return "", errors.New("asked for the wrong release")
			}
			return tree, treeErr
		},
	}, &fetched
}

func stampBody(tree string) string {
	return `{"schema":"forge.static-site-stamp/v1","tree":"` + tree + `"}`
}

// forge#2231: a hosted static site is witnessed by the stamp it serves,
// compared with the tree its bound release declares — MATCH right after a
// good deploy instead of an UNREACHABLE that exits 2 forever.
func TestHTTPStaticSiteWitness(t *testing.T) {
	for _, tc := range []struct {
		name               string
		code               int
		body, tree         string
		statusErr, treeErr error
		want               imageState
		detail             string
	}{
		{name: "serving the release", code: 200, body: stampBody(witnessTree), tree: witnessTree, want: imageMatch, detail: "serves this release's tree"},
		{name: "serving another tree", code: 200, body: stampBody("sha256:old"), tree: witnessTree, want: imageDrift, detail: "serves tree sha256:old"},
		{name: "serving an unstamped site", code: 404, tree: witnessTree, want: imageDrift, detail: "carries no release stamp"},
		{name: "an unstamped release cannot be judged", code: 200, body: stampBody(witnessTree), tree: "", want: imageUnreachable, detail: "built before forge stamped sites"},
		{name: "a page that is not a stamp", code: 200, body: "<html>", tree: witnessTree, want: imageUnreachable, detail: "not a forge stamp"},
		{name: "the origin errs", code: 503, tree: witnessTree, want: imageUnreachable, detail: "answered 503"},
		{name: "no hosted status", statusErr: errors.New("unauthenticated"), tree: witnessTree, want: imageUnreachable, detail: "could not read the env's hosted status"},
		{name: "no registry", code: 200, body: stampBody(witnessTree), treeErr: errors.New("denied"), want: imageUnreachable, detail: "could not read the release's tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, fetched := fakeStaticWitness(tc.code, tc.body, tc.tree, tc.statusErr, tc.treeErr)
			v := w.Witness(context.Background(), "landing", "reg.example/org/landing/static.v1", witnessDigest)
			if v.State != tc.want || !strings.Contains(v.Detail, tc.detail) {
				t.Fatalf("verification = %+v, want %s with %q", v, tc.want, tc.detail)
			}
			if v.Declared != witnessDigest || v.Image != "landing/frontend" {
				t.Errorf("row = %+v", v)
			}
			if *fetched != "" && !strings.HasPrefix(*fetched, "https://landing.example/_forge/site.json?forge-status=") {
				t.Errorf("fetched %s, want the static site's stamp with a cache-busting query", *fetched)
			}
		})
	}
}

// Only the hosted frontend slot has a witness; any other platform-run
// binding stays unobservable, as before.
func TestWitnessHostedSites_OnlyTheFrontendSlot(t *testing.T) {
	s := release.Snapshot{
		Artifacts: map[release.ArtifactID]release.SnapshotArtifact{
			"site": {Kind: release.KindOCI, Location: "reg.example/org/landing/static.v1", Content: witnessDigest},
			"img":  {Kind: release.KindOCI, Location: "reg.example/org/api", Content: witnessDigest},
		},
		Bindings: []release.ConsumerBinding{
			{Consumer: release.ConsumerKey{Cluster: release.BundleHostedCluster, Workload: "landing"}, Slot: "frontend", Artifact: "site"},
			{Consumer: release.ConsumerKey{Cluster: release.BundleHostedCluster, Workload: "api"}, Slot: "container:api", Artifact: "img"},
		},
	}
	w, _ := fakeStaticWitness(200, stampBody(witnessTree), witnessTree, nil, nil)
	got := map[string]imageState{}
	for _, v := range witnessHostedSites(context.Background(), s, w) {
		got[v.Image] = v.State
	}
	if got["landing/frontend"] != imageMatch || got["api/container:api"] != imageUnreachable || len(got) != 2 {
		t.Fatalf("rows = %v", got)
	}
}
