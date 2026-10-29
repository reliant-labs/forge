package cli

import (
	"testing"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/servicecatalog"
)

// A control plane whose layer validator predates the catalog refuses the
// whole bundle, so a reader that does not name the feature never gets it.
func TestServiceCatalogIsWithheldFromAReaderThatDoesNotNameIt(t *testing.T) {
	files := []bundle.ObservabilityFile{
		{Path: "observability/metrics-schema.json", Data: []byte("{}")},
		{Path: servicecatalog.BundlePath, Data: []byte("{}")},
	}
	for name, tc := range map[string]struct {
		reader   *release.BundleReader
		withheld bool
	}{
		"this forge (nil reader)":    {nil, false},
		"control plane naming it":    {&release.BundleReader{Features: release.BundleDocFeatures()}, false},
		"control plane predating it": {&release.BundleReader{Features: []string{release.BundleFeatureUptimeChecks}}, true},
		"control plane with no RPC":  {&release.BundleReader{}, true},
	} {
		got, withheld := withholdServiceCatalog(files, tc.reader)
		if withheld != tc.withheld {
			t.Errorf("%s: withheld = %v, want %v", name, withheld, tc.withheld)
		}
		want := len(files)
		if tc.withheld {
			want--
		}
		if len(got) != want {
			t.Errorf("%s: %d files, want %d", name, len(got), want)
		}
		for _, f := range got {
			if tc.withheld && f.Path == servicecatalog.BundlePath {
				t.Errorf("%s: the catalog was sent", name)
			}
		}
	}
	if _, withheld := withholdServiceCatalog(files[:1], &release.BundleReader{}); withheld {
		t.Error("a bundle with no catalog reports nothing withheld")
	}
}
