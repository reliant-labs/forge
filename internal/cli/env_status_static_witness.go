package cli

// `forge env status` for a HOSTED STATIC SITE (forge#2231).
//
// The control plane's origin serves a hosted site, and forge cannot read the
// bucket behind it, so without a witness of its own the frontend slot was
// always UNREACHABLE and status exited 2 even right after a good deploy. The
// witness is the site itself: a hosted site release carries a stamp naming
// its tree (deploytarget/staticsite_stamp.go), status fetches it over HTTP at
// the site's platform URL, and compares it with the tree the bound release's
// manifest declares. That is what visitors get through the CDN, not what a
// ledger claims.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// staticSiteWitness reports what a hosted static frontend serves against the
// release artifact (repository@digest) its binding pins.
type staticSiteWitness interface {
	Witness(ctx context.Context, frontend, repository, digest string) imageVerification
}

// staticStampSource names where a hosted site verdict came from.
const staticStampSource = "the live site's release stamp"

// httpStaticSiteWitness is the real witness: the site's URL from the env's
// hosted status, its stamp over HTTP, the release's tree from the registry.
// Each read is a seam so the comparison is tested without a network.
type httpStaticSiteWitness struct {
	env string
	// status reads the env's hosted status once; its URLs are shared by
	// every frontend.
	status func(ctx context.Context) (deploytarget.HostedEnvStatus, error)
	// fetch GETs one URL and returns its status code and body.
	fetch func(ctx context.Context, url string) (int, []byte, error)
	// tree reads the tree digest a release's manifest declares.
	tree func(ctx context.Context, repository, digest string) (string, error)

	once   sync.Once
	urls   map[string]string
	urlErr error
}

func newHTTPStaticSiteWitness(env string, timeout time.Duration) *httpStaticSiteWitness {
	client := &http.Client{Timeout: timeout}
	return &httpStaticSiteWitness{
		env: env,
		status: func(ctx context.Context) (deploytarget.HostedEnvStatus, error) {
			target, err := resolveDeclaredWaitTarget(ctx, env)
			if err != nil {
				return deploytarget.HostedEnvStatus{}, err
			}
			return deploytarget.ReadHostedStatus(ctx, target.Client, hostedProjectName(), env)
		},
		fetch: func(ctx context.Context, url string) (int, []byte, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return 0, nil, err
			}
			req.Header.Set("Cache-Control", "no-cache")
			resp, err := client.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			return resp.StatusCode, body, err
		},
		tree: deploytarget.ReadStaticSiteTree,
	}
}

// siteURL is the platform URL the env's hosted status reports for frontend.
func (w *httpStaticSiteWitness) siteURL(ctx context.Context, frontend string) (string, error) {
	w.once.Do(func() {
		st, err := w.status(ctx)
		if err != nil {
			w.urlErr = err
			return
		}
		w.urls = map[string]string{}
		for _, ws := range st.Workloads {
			if u := strings.TrimSpace(ws.URL); u != "" && (ws.Tier == "static" || w.urls[ws.Name] == "") {
				w.urls[ws.Name] = u
			}
		}
	})
	if w.urlErr != nil {
		return "", fmt.Errorf("could not read the env's hosted status for the site's URL: %v", cloud.FirstErrorLine(w.urlErr))
	}
	u := w.urls[frontend]
	if u == "" {
		return "", fmt.Errorf("the control plane reports no URL for site %s yet", frontend)
	}
	return u, nil
}

func (w *httpStaticSiteWitness) Witness(ctx context.Context, frontend, repository, digest string) imageVerification {
	v := imageVerification{Image: frontend + "/frontend", Declared: digest, State: imageUnreachable}
	base, err := w.siteURL(ctx, frontend)
	if err != nil {
		v.Detail = err.Error()
		return v
	}
	v.Workloads = []string{base}
	want, err := w.tree(ctx, repository, digest)
	if err != nil {
		v.Detail = fmt.Sprintf("could not read the release's tree from the registry: %v", err)
		return v
	}
	if want == "" {
		v.Detail = "this release was built before forge stamped sites, so what the site serves cannot be checked against it; a release built now carries the stamp"
		return v
	}
	// A query the origin ignores keeps a CDN from answering with a stamp it
	// cached before the deploy.
	stampURL := strings.TrimRight(base, "/") + "/" + deploytarget.StaticSiteStampPath + "?forge-status=" + strconv.FormatInt(time.Now().UnixNano(), 36)
	code, body, err := w.fetch(ctx, stampURL)
	switch {
	case err != nil:
		v.Detail = fmt.Sprintf("could not fetch the live site's stamp: %v", err)
		return v
	case code == http.StatusNotFound:
		v.State = imageDrift
		v.Detail = fmt.Sprintf("the live site carries no release stamp (%s answered 404), so it is not serving this release, which has one", deploytarget.StaticSiteStampPath)
		return v
	case code != http.StatusOK:
		v.Detail = fmt.Sprintf("the live site answered %d for its stamp", code)
		return v
	}
	stamp, err := deploytarget.ParseStaticSiteStamp(body)
	if err != nil {
		v.Detail = err.Error()
		return v
	}
	v.Running = stamp.Tree
	if stamp.Tree == want {
		v.State = imageMatch
		v.Detail = "the live site serves this release's tree (" + staticStampSource + ")"
		return v
	}
	v.State = imageDrift
	v.Detail = fmt.Sprintf("the live site serves tree %s, but release %s is tree %s (a CDN may take a minute after a deploy)", stamp.Tree, digest, want)
	return v
}

// witnessHostedSites reports each hosted static frontend binding of s
// through w; any other hosted binding stays unobservable.
func witnessHostedSites(ctx context.Context, s release.Snapshot, w staticSiteWitness) []imageVerification {
	var sites []release.ConsumerBinding
	for _, b := range s.Bindings {
		if b.Consumer.Cluster == release.BundleHostedCluster && b.Slot == "frontend" && w != nil {
			sites = append(sites, b)
		}
	}
	out := verifySnapshotBindings(withoutBindings(s, sites), release.BundleHostedCluster, nil,
		fmt.Errorf("snapshot observation requires an explicit per-consumer witness for destination %s", release.BundleHostedCluster))
	for _, b := range sites {
		a := s.Artifacts[b.Artifact]
		out = append(out, w.Witness(ctx, b.Consumer.Workload, a.Location, a.Content))
	}
	return out
}

// withoutBindings is s less the given bindings.
func withoutBindings(s release.Snapshot, drop []release.ConsumerBinding) release.Snapshot {
	if len(drop) == 0 {
		return s
	}
	skip := map[release.ConsumerBinding]bool{}
	for _, b := range drop {
		skip[b] = true
	}
	out := s
	out.Bindings = nil
	for _, b := range s.Bindings {
		if !skip[b] {
			out.Bindings = append(out.Bindings, b)
		}
	}
	return out
}
