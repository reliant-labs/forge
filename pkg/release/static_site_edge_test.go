package release

import (
	"slices"
	"strings"
	"testing"
)

// A static site that asks for Markdown negotiation, or opts out of edge
// logs, is refused to a control plane that does not honour it, never sent:
// it would ignore the declaration.
func TestBundleReader_CheckStaticSiteEdgeFields(t *testing.T) {
	sites := []string{"web", "docs"}
	cases := []struct {
		capability string
		check      func(*BundleReader, []string) error
		want       string
	}{
		{CapabilityServesMarkdownNegotiation, (*BundleReader).CheckMarkdownNegotiation,
			`static site web, docs declares markdown_negotiation, and this control plane does not negotiate Markdown twins (it does not advertise "serves-markdown-negotiation")`},
		{CapabilityHonorsEdgeLogOptOut, (*BundleReader).CheckEdgeLogOptOut,
			`static site web, docs opts out of edge logs (ObservabilitySignals.edge = False, or observability disabled), and this control plane does not honour that opt-out (it does not advertise "honors-edge-log-opt-out")`},
	}
	for _, c := range cases {
		for name, r := range map[string]*BundleReader{
			"machine ledger (nil reader)": nil,
			"honours it":                  {Features: []string{c.capability}},
		} {
			if err := c.check(r, sites); err != nil {
				t.Errorf("%s / %s: %v", c.capability, name, err)
			}
		}
		for name, r := range map[string]*BundleReader{
			"predates it":                      {},
			"a pin bump: every decode feature": {Features: BundleDocFeatures()},
		} {
			err := c.check(r, sites)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s / %s: got %v, want it to contain %q", c.capability, name, err, c.want)
			}
		}
		if err := c.check(&BundleReader{}, nil); err != nil {
			t.Errorf("%s: no site declares it, yet refused: %v", c.capability, err)
		}
		// Honouring is added by hand, never claimed by a pin bump.
		if slices.Contains(BundleDocFeatures(), c.capability) {
			t.Errorf("BundleDocFeatures claims %s", c.capability)
		}
	}
	// The wire strings are frozen.
	if CapabilityServesMarkdownNegotiation != "serves-markdown-negotiation" || CapabilityHonorsEdgeLogOptOut != "honors-edge-log-opt-out" {
		t.Error("a static-site edge capability's wire string changed")
	}
}
