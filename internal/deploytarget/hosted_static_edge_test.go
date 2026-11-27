package deploytarget

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

// A static site asking for Markdown negotiation, or opting out of edge logs,
// is refused before the environment is ensured or the bundle recorded when
// the control plane does not honour it; one that does takes the deploy.
//
// MUTATIONS VERIFIED RED: dropping the checkStaticSiteServed call from
// Deploy → the deploy ensures the environment and succeeds.
func TestHostedDeployGatesStaticSiteEdgeFields(t *testing.T) {
	off := false
	cases := []struct {
		name       string
		mutate     func(g *ServiceGroup)
		capability string
		want       string
	}{
		{"markdown", func(g *ServiceGroup) { g.Services[0].Hosted.Static.MarkdownNegotiation = true },
			release.CapabilityServesMarkdownNegotiation, `declares markdown_negotiation, and this control plane does not negotiate Markdown twins (it does not advertise "serves-markdown-negotiation")`},
		{"edge logs off", func(g *ServiceGroup) { g.Services[0].Hosted.Static.EdgeLogs = &off },
			release.CapabilityHonorsEdgeLogOptOut, `opts out of edge logs (ObservabilitySignals.edge = False, or observability disabled), and this control plane does not honour that opt-out (it does not advertise "honors-edge-log-opt-out")`},
	}
	for _, c := range cases {
		t.Run(c.name+"/refused", func(t *testing.T) {
			g := staticGroup("v1", map[string]string{staticSiteArtifact: digestA})
			c.mutate(&g)
			cp := &autoscaleCP{fakeCP: &fakeCP{status: staticReadyStatus(digestA)}, features: release.BundleDocFeatures()}
			recorded := false
			p := HostedProvider{Client: cp, PollInterval: time.Millisecond,
				RecordBundle: func(context.Context, string) (string, error) { recorded = true; return "", nil }}
			err := p.Deploy(context.Background(), g)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Deploy = %v, want it to contain %q", err, c.want)
			}
			if got := cp.procs(); !slices.Equal(got, []string{"GetCapabilities"}) {
				t.Errorf("refused, but the calls were %v; want only GetCapabilities", got)
			}
			if recorded {
				t.Error("refused, but the bundle was recorded")
			}
		})
		t.Run(c.name+"/honoured", func(t *testing.T) {
			g := staticGroup("v1", map[string]string{staticSiteArtifact: digestA})
			c.mutate(&g)
			cp := &autoscaleCP{fakeCP: &fakeCP{status: staticReadyStatus(digestA)}, features: []string{c.capability}}
			p := HostedProvider{Client: cp, PollInterval: time.Millisecond,
				RecordBundle: func(context.Context, string) (string, error) { return "", nil }}
			if err := p.Deploy(context.Background(), g); err != nil {
				t.Fatalf("Deploy: %v", err)
			}
		})
	}
}
