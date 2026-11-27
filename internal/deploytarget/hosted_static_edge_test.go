package deploytarget

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
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

// THE v1 COMPATIBILITY PROPERTY. An env that disabled observability without
// naming edge logs (the implicit opt-out) deployed before the field existed,
// so it is never refused: against a control plane that does not advertise
// honors-edge-log-opt-out the deploy goes ahead with the spec it sent
// before, and against one that does the spec carries edgeLogs: false. The
// record carries the implicit marker (resolved and removed by bundle.Build)
// and never an edgeLogs key of its own.
func TestHostedDeployImplicitEdgeLogOptOutIsNeverRefused(t *testing.T) {
	implicit := func() ServiceGroup {
		g := staticGroup("v1", map[string]string{staticSiteArtifact: digestA})
		g.Services[0].Hosted.EdgeLogsOffIfHonoured = true
		return g
	}
	for name, features := range map[string][]string{
		"predates it":                      nil,
		"a pin bump: every decode feature": release.BundleDocFeatures(),
		"honours it":                       {release.CapabilityHonorsEdgeLogOptOut},
	} {
		t.Run(name, func(t *testing.T) {
			cp := &autoscaleCP{fakeCP: &fakeCP{status: staticReadyStatus(digestA)}, features: features}
			p := HostedProvider{Client: cp, PollInterval: time.Millisecond,
				RecordBundle: func(context.Context, string) (string, error) { return "", nil }}
			if err := p.Deploy(context.Background(), implicit()); err != nil {
				t.Fatalf("an implicit opt-out was refused: %v", err)
			}
		})
	}
	recs, err := HostedRecords(implicit())
	if err != nil {
		t.Fatal(err)
	}
	y := string(recs[0].YAML)
	if !strings.Contains(y, EdgeLogsOffIfHonouredAnnotation+`: "true"`) || strings.Contains(y, "edgeLogs") {
		t.Errorf("record = %s, want the implicit marker and no edgeLogs", y)
	}
	plain, _ := HostedRecords(staticGroup("v1", map[string]string{staticSiteArtifact: digestA}))
	if strings.Contains(string(plain[0].YAML), "annotations") {
		t.Errorf("a site without the opt-out records annotations: %s", plain[0].YAML)
	}
}

// The implicit opt-out becomes edgeLogs: false in the plan only for a
// control plane that honours it, and never overrides an explicit value.
func TestApplyImplicitEdgeLogOptOut(t *testing.T) {
	plan := func() []hostedPlanItem {
		return []hostedPlanItem{{Name: "web", Spec: v1alpha1.StaticSiteSpec{}, EdgeLogsOffIfHonoured: true}, {Name: "plain", Spec: v1alpha1.StaticSiteSpec{}}}
	}
	p := plan()
	applyImplicitEdgeLogOptOut(p, false)
	if s := p[0].Spec.(v1alpha1.StaticSiteSpec); s.EdgeLogs != nil {
		t.Errorf("not honoured, yet edgeLogs = %v", *s.EdgeLogs)
	}
	p = plan()
	applyImplicitEdgeLogOptOut(p, true)
	if s := p[0].Spec.(v1alpha1.StaticSiteSpec); s.EdgeLogs == nil || *s.EdgeLogs {
		t.Errorf("honoured, want edgeLogs false, got %v", s.EdgeLogs)
	}
	if s := p[1].Spec.(v1alpha1.StaticSiteSpec); s.EdgeLogs != nil {
		t.Error("a site without the opt-out got edgeLogs")
	}
}
