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
