package deploytarget

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// domainGroup is hounders' shape once it declares custom domains: the `web`
// frontend answers on the apex and www, and the `membership` service answers
// on api.hounders.club through its exposed port.
func domainGroup() ServiceGroup {
	keep := int32(10)
	return ServiceGroup{
		Env: "prod", ProviderID: HostedProviderID,
		Hosted: &HostedTarget{Endpoint: "https://cp.example", Release: "v1",
			Digests: map[string]string{"web": digestA, "membership": digestB}},
		Services: []ResolvedService{
			{Name: "web", Hosted: &HostedWorkload{Tier: HostedTierStatic, Artifact: "web", Static: &v1alpha1.StaticSiteSpec{
				KeepReleases: &keep, Domains: []string{"hounders.club", "www.hounders.club"},
			}}},
			{Name: "membership", Hosted: &HostedWorkload{Tier: HostedTierWorkload, Artifact: "membership", Workload: &v1alpha1.WorkloadSpec{
				Kind: v1alpha1.KindService, Image: "ghcr.io/acme/membership:v1", Args: []string{"serve"},
				Probes: &v1alpha1.Probes{},
				Ports: []v1alpha1.Port{{Name: "http", Port: 8080, Expose: true,
					Domains: []string{"api.hounders.club"}}},
			}}},
		},
	}
}

// capabilityCP is a control plane whose EnsureEnvironment advertises a given
// capability list, and whose status is always converged. It is the fake the
// capability handshake is tested against: everything else about it matches
// fakeCP's answers.
type capabilityCP struct {
	fakeCP
	capabilities []string
}

func (c *capabilityCP) Call(ctx context.Context, proc string, req, out any) error {
	if strings.HasSuffix(proc, "/EnsureEnvironment") {
		raw, _ := json.Marshal(req)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		c.mu.Lock()
		c.calls = append(c.calls, fakeCall{Proc: proc, Body: body})
		c.mu.Unlock()
		caps, _ := json.Marshal(c.capabilities)
		reply := fmt.Sprintf(`{"environment":{"id":"env-1","name":"prod","imagePushBase":"ghcr.io/acme","capabilities":%s},"created":true}`, caps)
		if out == nil {
			return nil
		}
		return json.Unmarshal([]byte(reply), out)
	}
	return c.fakeCP.Call(ctx, proc, req, out)
}

func domainProvider(cp HostedCaller) HostedProvider {
	return HostedProvider{Client: cp, Rollout: cluster.RolloutPolicy{Mode: cluster.RolloutSkip}}
}

// THE SAFETY PROPERTY. A control plane that does not advertise custom_domains
// ignores `domains` entirely: it accepts the spec, the deploy goes green, and
// the hostname never resolves, with nothing anywhere saying why. forge refuses
// BEFORE writing anything, and the refusal names every domain declared.
//
// MUTATIONS VERIFIED RED:
//   - dropping the checkCustomDomains call from Deploy → the deploy succeeds
//     and publishes;
//   - moving it after publish() → EnsureDeployment appears in the call log.
func TestHostedDeployRefusesDomainsWithoutTheCapability(t *testing.T) {
	cp := &capabilityCP{fakeCP: fakeCP{status: readyStatus(digestA)}}
	err := domainProvider(cp).Deploy(context.Background(), domainGroup())
	if err == nil {
		t.Fatal("the deploy published custom domains to a control plane that does not serve them")
	}
	for _, want := range []string{
		"hounders.club", "www.hounders.club", "api.hounders.club",
		"custom_domains", "refusing to publish anything",
		"would succeed and the domains would never resolve",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q:\n%v", want, err)
		}
	}
	// Refused BEFORE any write: the environment was ensured (that is how
	// the capability is known), and nothing past it ran.
	for _, proc := range cp.procs() {
		if proc == "EnsureDeployment" || proc == "PublishDeploymentConfig" {
			t.Fatalf("refused, but %s was called: %v", proc, cp.procs())
		}
	}
}

// With the capability present the same declaration publishes, and the
// StaticSite spec on the wire carries the domains in the order declared.
func TestHostedDeployPublishesDomainsWithTheCapability(t *testing.T) {
	cp := &capabilityCP{
		fakeCP:       fakeCP{status: readyStatus(digestA)},
		capabilities: []string{HostedCapabilityCustomDomains},
	}
	if err := domainProvider(cp).Deploy(context.Background(), domainGroup()); err != nil {
		t.Fatalf("deploy with the capability present: %v", err)
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	var static, workload []any
	for _, c := range cp.calls {
		if !strings.HasSuffix(c.Proc, "/EnsureDeployment") {
			continue
		}
		spec, _ := c.Body["spec"].(map[string]any)
		switch c.Body["name"] {
		case "web":
			static, _ = spec["domains"].([]any)
		case "membership":
			ports, _ := spec["ports"].([]any)
			if len(ports) == 1 {
				p, _ := ports[0].(map[string]any)
				workload, _ = p["domains"].([]any)
			}
		}
	}
	if got := fmt.Sprint(static); got != "[hounders.club www.hounders.club]" {
		t.Errorf("StaticSite spec.domains on the wire = %v, want the two declared names in order", static)
	}
	if got := fmt.Sprint(workload); got != "[api.hounders.club]" {
		t.Errorf("Workload port domains on the wire = %v, want [api.hounders.club]", workload)
	}
}

// A capability the control plane advertises alongside others still counts,
// and an env that declares NO domains never consults the list at all — a
// control plane with no capabilities must keep deploying ordinary workloads.
func TestCheckCustomDomainsScope(t *testing.T) {
	plan, err := planHosted(domainGroup())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if err := checkCustomDomains("prod", []string{"managed_databases", "custom_domains"}, plan); err != nil {
		t.Errorf("refused with the capability present among others: %v", err)
	}
	noDomains, err := planHosted(hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{}))
	if err != nil {
		t.Fatalf("plan without domains: %v", err)
	}
	if err := checkCustomDomains("prod", nil, noDomains); err != nil {
		t.Errorf("an env declaring no domains was refused for a missing capability: %v", err)
	}
}

// --dry-run makes no calls, so it cannot know the capability — and must not
// therefore go quiet about a declaration a real deploy would refuse.
func TestHostedPlanPrintReportsDeclaredDomains(t *testing.T) {
	plan, err := planHosted(domainGroup())
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	claims := hostedDomainClaims(plan)
	if len(claims) != 2 {
		t.Fatalf("hostedDomainClaims = %+v, want one per workload", claims)
	}
	if claims[0].Workload != "membership" || claims[1].Workload != "web" {
		t.Errorf("claims are not sorted by workload: %+v", claims)
	}
	if got := strings.Join(claims[1].Domains, ","); got != "hounders.club,www.hounders.club" {
		t.Errorf("web's domains = %q", got)
	}
}

// ─── Status decoding ─────────────────────────────────────────────────────────

const domainStatusBody = `{"deployments":[{"deployment":{"id":"dep-web","name":"web","tier":"DEPLOY_TIER_STATIC",
 "observed":{"state":"DEPLOY_OBSERVED_STATE_READY","url":"https://web.example","domains":[
   {"domain":"hounders.club","state":"DEPLOY_CUSTOM_DOMAIN_STATE_PENDING_DNS","requiredRecords":[
      {"type":"A","name":"hounders.club","value":"34.63.203.181"},
      {"type":"TXT","name":"_forge-challenge.hounders.club","value":"tok-123"}]},
   {"domain":"www.hounders.club","state":"DEPLOY_CUSTOM_DOMAIN_STATE_LIVE","liveSince":"2026-02-01T10:00:00Z"},
   {"domain":"shop.hounders.club","state":"DEPLOY_CUSTOM_DOMAIN_STATE_FAILED","lastError":"CAA record forbids this issuer"},
   {"domain":"blog.hounders.club","state":"DEPLOY_CUSTOM_DOMAIN_STATE_SOMETHING_NEW"}]}},
 "verdict":"DEPLOY_VERDICT_CONVERGED"}]}`

// ReadHostedStatus carries each declared domain's state, the records still
// required, and the last error — and decodes a state name this binary has
// never heard of as "unknown" rather than guessing at it.
func TestReadHostedStatusDecodesCustomDomains(t *testing.T) {
	cp := &fakeCP{
		envs:   `{"environments":[{"id":"env-1","name":"prod"}]}`,
		status: func(int) string { return domainStatusBody },
	}
	st, err := ReadHostedStatus(context.Background(), cp, "", "prod")
	if err != nil {
		t.Fatalf("ReadHostedStatus: %v", err)
	}
	if len(st.Workloads) != 1 {
		t.Fatalf("workloads = %+v", st.Workloads)
	}
	got := st.Workloads[0].Domains
	if len(got) != 4 {
		t.Fatalf("domains = %+v, want 4", got)
	}
	want := []struct {
		domain, state, lastError, liveSince string
		records                             int
	}{
		{"hounders.club", DomainStatePendingDNS, "", "", 2},
		{"www.hounders.club", DomainStateLive, "", "2026-02-01T10:00:00Z", 0},
		{"shop.hounders.club", DomainStateFailed, "CAA record forbids this issuer", "", 0},
		// A state from a newer control plane must NOT read as live.
		{"blog.hounders.club", DomainStateUnknown, "", "", 0},
	}
	for i, w := range want {
		g := got[i]
		if g.Domain != w.domain || g.State != w.state || g.LastError != w.lastError || g.LiveSince != w.liveSince || len(g.RequiredRecords) != w.records {
			t.Errorf("domain[%d] = %+v, want %+v", i, g, w)
		}
	}
	if r := got[0].RequiredRecords[0]; r.Type != "A" || r.Name != "hounders.club" || r.Value != "34.63.203.181" {
		t.Errorf("first required record = %+v", r)
	}
}

// The printed block names each domain's state and the exact record to set —
// the author's next action is to type it into a registrar's form.
func TestFormatCustomDomains(t *testing.T) {
	cp := &fakeCP{
		envs:   `{"environments":[{"id":"env-1","name":"prod"}]}`,
		status: func(int) string { return domainStatusBody },
	}
	st, err := ReadHostedStatus(context.Background(), cp, "", "prod")
	if err != nil {
		t.Fatalf("ReadHostedStatus: %v", err)
	}
	out := FormatCustomDomains("  ", st.Workloads[0].Domains)
	for _, want := range []string{
		"hounders.club: pending_dns",
		"DNS record to set: A hounders.club → 34.63.203.181",
		"DNS record to set: TXT _forge-challenge.hounders.club → tok-123",
		"www.hounders.club: live since 2026-02-01T10:00:00Z",
		"shop.hounders.club: failed",
		"error: CAA record forbids this issuer",
		"blog.hounders.club: unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("formatted block is missing %q:\n%s", want, out)
		}
	}
	if FormatCustomDomains("  ", nil) != "" {
		t.Error("a workload with no domains must format to nothing, so callers can print unconditionally")
	}
}

// A StaticSite spec's domains go through the same validation a port's do:
// DNS form, no wildcard, no duplicate, at most MaxDomains.
func TestStaticSiteSpecRefusesBadDomains(t *testing.T) {
	for _, tc := range []struct {
		name    string
		domains []string
		want    string
	}{
		{"wildcard", []string{"*.hounders.club"}, "wildcard"},
		{"uppercase", []string{"Hounders.club"}, "not a lowercase DNS hostname"},
		{"not a hostname", []string{"http://hounders.club"}, "not a lowercase DNS hostname"},
		{"duplicate", []string{"hounders.club", "hounders.club"}, "listed twice"},
		{"too many", []string{"a.club", "b.club", "c.club", "d.club", "e.club", "f.club", "g.club", "h.club", "i.club"}, "at most 8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := v1alpha1.StaticSiteSpec{Domains: tc.domains}.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
	if err := (v1alpha1.StaticSiteSpec{Domains: []string{"hounders.club", "www.hounders.club"}}).Validate(); err != nil {
		t.Errorf("a valid pair of domains was refused: %v", err)
	}
}
