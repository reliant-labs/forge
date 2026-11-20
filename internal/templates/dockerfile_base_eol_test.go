package templates

import (
	"regexp"
	"testing"
	"time"
)

// alpineReleases is when each Alpine branch was released. Alpine supports a
// stable branch's main repository for two years from its release
// (https://alpinelinux.org/releases/), after which it gets no security
// updates and scanners report the image as end-of-life.
var alpineReleases = map[string]time.Time{
	"3.21": time.Date(2024, time.December, 5, 0, 0, 0, 0, time.UTC),
	"3.22": time.Date(2025, time.May, 30, 0, 0, 0, 0, time.UTC),
	"3.23": time.Date(2025, time.December, 3, 0, 0, 0, 0, time.UTC),
	"3.24": time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC),
}

// alpineEOLMargin is how long before a branch's end of life the scaffold
// must have moved off it, so no newly scaffolded project starts on a base a
// scanner is about to call end-of-life.
const alpineEOLMargin = 120 * 24 * time.Hour

// TestScaffoldedRuntimeBaseIsSupported pins NS FG-28: the Dockerfile forge
// scaffolds shipped alpine:3.21 after Trivy reported it end-of-life ("no
// longer supported by the distribution … security updates are not
// provided"). Every runtime stage must name an Alpine branch with at least
// alpineEOLMargin of support left. It reads only the template (embedded),
// and its verdict changes with the calendar on purpose: it is the reminder to
// bump the base, which is one line in Dockerfile.tmpl plus a row above.
func TestScaffoldedRuntimeBaseIsSupported(t *testing.T) {
	src, err := ProjectTemplates().Get("Dockerfile.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	froms := regexp.MustCompile(`(?m)^FROM alpine:(\d+\.\d+)\b`).FindAllStringSubmatch(string(src), -1)
	if len(froms) == 0 {
		t.Fatal("Dockerfile.tmpl names no alpine runtime base; update this test with the base it uses now")
	}
	for _, m := range froms {
		released, ok := alpineReleases[m[1]]
		if !ok {
			t.Errorf("alpine:%s has no release date in alpineReleases: add it", m[1])
			continue
		}
		eol := released.AddDate(2, 0, 0)
		if time.Until(eol) < alpineEOLMargin {
			t.Errorf("Dockerfile.tmpl's runtime base alpine:%s reaches end of life on %s: move it to the newest Alpine branch (and add that branch to alpineReleases)",
				m[1], eol.Format("2006-01-02"))
		}
	}
}
