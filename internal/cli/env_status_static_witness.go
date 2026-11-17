package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

// The witness for a hosted static site in `forge env status` (forge#2231).
//
// forge cannot read the control plane's static origin, so for a hosted
// frontend the snapshot path used to report UNREACHABLE (exit 2) forever. The
// site itself is the witness: `env build` stamps the release it builds into
// the site (deploytarget.StaticSiteReleaseMarkerPath), and status fetches that
// marker from the env's live URL, through the CDN users reach, and compares
// it with the release that built the artifact the snapshot binds:
//
//	marker release == binding's release   MATCH
//	marker release != binding's release   DRIFT (exit 1)
//	no URL, no answer, no marker          UNREACHABLE (exit 2), saying which
//
// A site built before forge stamped markers serves none, so it stays
// UNREACHABLE until its next release, with a detail that says so.

// errNoReleaseMarker is a live site that answers 404 for the marker.
var errNoReleaseMarker = errors.New("serves no forge release marker")

// readDeclaredStaticSiteURLs is the live URL of each hosted static site of
// env, by frontend name, as the env's control plane reports it.
func readDeclaredStaticSiteURLs(ctx context.Context, env string) (map[string]string, error) {
	target, err := resolveDeclaredWaitTarget(ctx, env)
	if err != nil {
		return nil, err
	}
	st, err := deploytarget.ReadHostedStatus(ctx, target.Client, hostedProjectName(), env)
	if err != nil {
		return nil, err
	}
	urls := map[string]string{}
	for _, w := range st.Workloads {
		if w.Tier == string(deploytarget.HostedTierStatic) && w.URL != "" {
			urls[w.Name] = w.URL
		}
	}
	return urls, nil
}

// releaseMarkerURL is where siteURL serves its release marker.
func releaseMarkerURL(siteURL string) string {
	return strings.TrimRight(siteURL, "/") + "/" + deploytarget.StaticSiteReleaseMarkerPath
}

// fetchStaticReleaseMarker reads the release marker siteURL serves. The
// marker is served no-cache; the request says no-cache too, so a cache in
// between revalidates rather than answering for the previous release.
func fetchStaticReleaseMarker(ctx context.Context, siteURL string) (deploytarget.StaticSiteReleaseMarker, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseMarkerURL(siteURL), http.NoBody)
	if err != nil {
		return deploytarget.StaticSiteReleaseMarker{}, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return deploytarget.StaticSiteReleaseMarker{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return deploytarget.StaticSiteReleaseMarker{}, errNoReleaseMarker
	}
	if resp.StatusCode != http.StatusOK {
		return deploytarget.StaticSiteReleaseMarker{}, fmt.Errorf("answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return deploytarget.StaticSiteReleaseMarker{}, err
	}
	return deploytarget.ParseStaticSiteReleaseMarker(body)
}

// verifyHostedSnapshotBindings is the verdict for every binding of the
// snapshot's hosted tree: a static site's frontend slot through its release
// marker, and everything else UNREACHABLE, as forge has no witness for it.
func verifyHostedSnapshotBindings(ctx context.Context, s release.Snapshot, env string, opts envStatusOptions) []imageVerification {
	readURLs := opts.StaticSiteURLs
	if readURLs == nil {
		readURLs = readDeclaredStaticSiteURLs
	}
	fetch := opts.FetchReleaseMarker
	if fetch == nil {
		fetch = fetchStaticReleaseMarker
	}
	var (
		urls    map[string]string
		urlErr  error
		urlRead bool
	)
	var out []imageVerification
	for _, b := range s.Bindings {
		if b.Consumer.Cluster != release.BundleHostedCluster {
			continue
		}
		a := s.Artifacts[b.Artifact]
		result := imageVerification{Image: b.Consumer.Workload + "/" + b.Slot, Declared: a.Content, State: imageUnreachable,
			Detail: "snapshot observation requires an explicit per-consumer witness for destination " + release.BundleHostedCluster}
		if b.Slot != "frontend" {
			out = append(out, result)
			continue
		}
		if !urlRead {
			urlRead = true
			urls, urlErr = readURLs(ctx, env)
		}
		out = append(out, verifyStaticSite(ctx, result, b.Consumer.Workload, a, urls, urlErr, fetch))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Image < out[j].Image })
	return out
}

// verifyStaticSite judges one hosted static site by the marker its live URL
// serves. result arrives UNREACHABLE and leaves that way unless the marker
// says otherwise.
func verifyStaticSite(ctx context.Context, result imageVerification, name string, a release.SnapshotArtifact, urls map[string]string, urlErr error,
	fetch func(context.Context, string) (deploytarget.StaticSiteReleaseMarker, error)) imageVerification {
	want := a.Origin.Release
	switch {
	case want == "":
		result.Detail = "the snapshot records no release for this site, so there is nothing to compare its marker with"
		return result
	case urlErr != nil:
		result.Detail = "read the site's live URL from the control plane: " + urlErr.Error()
		return result
	case urls[name] == "":
		result.Detail = "the control plane reports no live URL for static site " + name + " yet"
		return result
	}
	site := urls[name]
	result.Workloads = []string{site}
	m, err := fetch(ctx, site)
	switch {
	case errors.Is(err, errNoReleaseMarker):
		result.Detail = fmt.Sprintf("%s %s; a release built before forge stamped one has none, so this site is verifiable from its next release",
			site, errNoReleaseMarker)
		return result
	case err != nil:
		result.Detail = fmt.Sprintf("read %s: %v", releaseMarkerURL(site), err)
		return result
	}
	result.Running = "release " + m.Release
	if m.Frontend != "" && m.Frontend != name {
		result.State = imageDrift
		result.Detail = fmt.Sprintf("%s serves frontend %s's release marker, not %s's", site, m.Frontend, name)
		return result
	}
	if m.Release != want {
		result.State = imageDrift
		result.Detail = fmt.Sprintf("%s serves release %s; the snapshot binds release %s", site, m.Release, want)
		return result
	}
	result.State = imageMatch
	result.Detail = fmt.Sprintf("%s serves release %s (its release marker)", site, m.Release)
	return result
}
