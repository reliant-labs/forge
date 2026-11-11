package kcl

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// TestReleaseProbeBoundsMatchGo holds forge.Probe's KCL bounds (kcl/workload.k)
// equal to pkg/release's, which the control plane validates a recorded shape
// with. A bound loosened on one side only is a declaration KCL renders and the
// control plane refuses (or the reverse), so it fails here instead of at a
// deploy. It also requires the Probe schema to check against those constants,
// so a literal written into a check cannot drift past this test.
func TestReleaseProbeBoundsMatchGo(t *testing.T) {
	src := readFile(t, "workload.k")
	for name, want := range map[string]int{
		"_RELEASE_PROBE_MAX_PATHS":           release.MaxReleaseProbePaths,
		"_RELEASE_PROBE_MAX_PATH_LEN":        release.MaxReleaseProbePathLen,
		"_RELEASE_PROBE_MIN_RATE":            release.MinReleaseProbeRate,
		"_RELEASE_PROBE_MAX_RATE":            release.MaxReleaseProbeRate,
		"_RELEASE_PROBE_MIN_TIMEOUT_SECONDS": int(release.MinReleaseProbeTimeout / time.Second),
		"_RELEASE_PROBE_MAX_TIMEOUT_SECONDS": int(release.MaxReleaseProbeTimeout / time.Second),
	} {
		m := regexp.MustCompile(`(?m)^` + name + ` = (\d+)$`).FindStringSubmatch(src)
		if m == nil {
			t.Errorf("kcl/workload.k declares no %s", name)
			continue
		}
		if got, _ := strconv.Atoi(m[1]); got != want {
			t.Errorf("KCL %s = %d, Go = %d", name, got, want)
		}
		// The path length is checked in _release_probe_path_ok, which the
		// schema calls; every other bound in the schema's own checks.
		checks := schemaBodies(src)["Probe"] + src[strings.Index(src, "_release_probe_path_ok = "):strings.Index(src, "schema Probe:")]
		if !strings.Contains(checks, name) {
			t.Errorf("schema Probe does not check against %s", name)
		}
	}
}

// ReleaseProbePathCases are paths lists forge.Probe and pkg/release must judge
// alike: every path rule (prefix, length, query, fragment, '..', '//', '.'
// segment, percent-encoding, the character set) and every list rule (1 to 5
// paths, no repeats). Exported for release_probe_eval_test.go, which renders
// each through KCL.
var ReleaseProbePathCases = [][]string{
	{"/"}, {"/flow-health"}, {"/a/b.c"}, {"/.well-known/health"}, {"/~u/x_y"}, {"/a:b@c;d=e,f+g"},
	{"/a!$&'()*"}, {"/" + strings.Repeat("a", release.MaxReleaseProbePathLen-1)},
	{"/" + strings.Repeat("a", release.MaxReleaseProbePathLen)},
	{""}, {"a"}, {"/a?b=1"}, {"/a#top"}, {"/a/../b"}, {"/.."}, {"/a..b"},
	{"//evil.example"}, {"/a//b"}, {"/./a"}, {"/a/."}, {"/a/./b"}, {"/a%20b"}, {"/%2e%2e"},
	{"/a b"}, {"/a\\b"}, {"/a\tb"}, {"/a\n"}, {"/é"}, {"/a\"b"}, {"/a<b>"}, {"/a|b"}, {"/a^b"}, {"/a`b"}, {"/a{b}"}, {"/a[b]"},
	{}, {"/a", "/b", "/c", "/d", "/e"}, {"/a", "/b", "/c", "/d", "/e", "/f"}, {"/a", "/a"}, {"/", "/a b"},
}

// ReleaseProbePathsOK is pkg/release's verdict on a probe declaring paths.
func ReleaseProbePathsOK(paths []string) bool {
	return release.Shape{Kind: release.EnvSelfManaged, ReleaseProbes: []release.ShapeReleaseProbe{{
		Target: "web", Kind: release.ReleaseProbeKindFrontend, Paths: paths, Method: "GET",
		RatePerMin: 4, TimeoutSeconds: 10, Source: release.ReleaseProbeSourceUser,
	}}}.Validate() == nil
}

// TestReleaseProbePathCharsMatchGo is the short-tier half of the path rule's
// agreement: forge.Probe's character-set regex (_RELEASE_PROBE_PATH_RE) must
// accept a single path exactly when pkg/release accepts it, apart from the
// rules KCL checks beside the regex (length and the segment rules), which
// TestReleaseProbePathsAgreeWithGo proves through a real KCL evaluation.
func TestReleaseProbePathCharsMatchGo(t *testing.T) {
	src := readFile(t, "workload.k")
	m := regexp.MustCompile(`(?m)^_RELEASE_PROBE_PATH_RE = r"(.*)"$`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("kcl/workload.k declares no _RELEASE_PROBE_PATH_RE")
	}
	re := regexp.MustCompile(m[1])
	segmentRule := func(p string) bool {
		return len(p) > release.MaxReleaseProbePathLen || strings.Contains(p, "..") || strings.Contains(p, "//") ||
			strings.Contains(p, "/./") || strings.HasSuffix(p, "/.")
	}
	for _, paths := range ReleaseProbePathCases {
		if len(paths) != 1 || segmentRule(paths[0]) {
			continue
		}
		if got, want := re.MatchString(paths[0]), ReleaseProbePathsOK(paths); got != want {
			t.Errorf("path %q: KCL regex accepts = %v, Go accepts = %v", paths[0], got, want)
		}
	}
}
