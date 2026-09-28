package deploytarget

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// Custom domains: forge's half.
//
// A custom hostname is DECLARED — `domains` on a workload's exposed port, or
// on an OnHosted frontend — and the control plane converges it: it records
// the claim, verifies ownership from DNS, issues a certificate, serves the
// name, and reports per-domain state plus the DNS records the author still
// has to set. forge never adds a domain imperatively; there is no
// `forge domain add`, because the declaration in KCL is the source of truth.
//
// That leaves forge one job the control plane cannot do for it: refusing a
// declaration the platform will not honour. A control plane that does not
// converge domains accepts the spec, publishes it, and serves only the
// allocated hostname — so the author's deploy goes green while the domain
// they declared does nothing, and nothing anywhere says why. The capability
// handshake below is what turns that silence into a refusal.

// HostedCapabilityCustomDomains is the capability name a control plane
// advertises on its environment when it converges `Port.domains` and
// `StaticSite.domains`. Absent, forge refuses to publish a declaration
// carrying either.
const HostedCapabilityCustomDomains = "custom_domains"

// hostedDomainClaim is one workload's declared custom hostnames.
type hostedDomainClaim struct {
	// Workload is the deployment name that declared them.
	Workload string
	// Tier distinguishes a Workload port's domains from a StaticSite's,
	// so the refusal can name where to go and edit.
	Tier    HostedTier
	Domains []string
}

// hostedDomainClaims is every custom domain a plan declares, sorted by
// workload. Pure, and read by both the deploy's refusal and the dry-run
// report, so the two can never disagree about what was declared.
func hostedDomainClaims(plan []hostedPlanItem) []hostedDomainClaim {
	var out []hostedDomainClaim
	for _, item := range plan {
		switch spec := item.Spec.(type) {
		case v1alpha1.WorkloadSpec:
			var domains []string
			for _, p := range spec.Ports {
				domains = append(domains, p.Domains...)
			}
			if len(domains) > 0 {
				out = append(out, hostedDomainClaim{Workload: item.Name, Tier: item.Tier, Domains: domains})
			}
		case v1alpha1.StaticSiteSpec:
			if len(spec.Domains) > 0 {
				out = append(out, hostedDomainClaim{Workload: item.Name, Tier: item.Tier, Domains: spec.Domains})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Workload < out[j].Workload })
	return out
}

// hasCapability reports whether the environment advertises name.
func hasCapability(capabilities []string, name string) bool {
	for _, c := range capabilities {
		if strings.EqualFold(strings.TrimSpace(c), name) {
			return true
		}
	}
	return false
}

// checkCustomDomains refuses a deploy whose plan declares custom domains
// against a control plane that does not advertise custom_domains. It runs
// after the environment is known (the capability list comes back on it) and
// BEFORE any EnsureDeployment or publish, so a declaration the platform would
// ignore costs no deployment write and no ledger entry.
//
// REFUSING IS THE POINT. Publishing anyway is the one outcome that cannot be
// debugged from the outside: the spec is accepted, the deploy reports
// success, and the domain silently never resolves. Every declared domain is
// named, so one refusal tells the author everything they declared and what to
// do about it.
func checkCustomDomains(envName string, capabilities []string, plan []hostedPlanItem) error {
	claims := hostedDomainClaims(plan)
	if len(claims) == 0 || hasCapability(capabilities, HostedCapabilityCustomDomains) {
		return nil
	}
	var lines []string
	for _, c := range claims {
		lines = append(lines, fmt.Sprintf("    %s (%s): %s", c.Workload, c.Tier, strings.Join(c.Domains, ", ")))
	}
	return fmt.Errorf("hosted env %q: refusing to publish anything — %d workload(s) declare custom domains, "+
		"and this control plane does not serve them (its environment advertises no %q capability):\n%s\n"+
		"  Publishing would succeed and the domains would never resolve, with nothing to read that says why.\n"+
		"  fix: remove the `domains` declaration to deploy on the platform-allocated hostname, or wait for the "+
		"control plane to serve custom domains and deploy again — the declaration is converged once it does",
		envName, len(claims), HostedCapabilityCustomDomains, strings.Join(lines, "\n"))
}

// ─── Per-domain status ───────────────────────────────────────────────────────

// The DeployCustomDomainState value names the control plane reports. Decoded
// tolerantly: an unrecognised value is "unknown", never defaulted toward
// live. A domain forge reports as live is one the platform said is live.
const (
	wireDomainStatePrefix = "DEPLOY_CUSTOM_DOMAIN_STATE_"
)

// wireDNSRecord is one DNS record the author must set at their registrar.
type wireDNSRecord struct {
	Type  string `json:"type,omitempty"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// wireCustomDomainStatus is one declared domain as the control plane
// observes it.
type wireCustomDomainStatus struct {
	Domain          string          `json:"domain,omitempty"`
	State           string          `json:"state,omitempty"`
	RequiredRecords []wireDNSRecord `json:"requiredRecords,omitempty"`
	LastError       string          `json:"lastError,omitempty"`
	LiveSince       *time.Time      `json:"liveSince,omitempty"`
}

// HostedDNSRecord is one record the author must publish at their registrar
// before a domain can go live.
type HostedDNSRecord struct {
	// Type is A | CNAME | TXT.
	Type string `json:"type,omitempty"`
	// Name is the record name, as the registrar wants it.
	Name string `json:"name,omitempty"`
	// Value is the record's value — an address, a target, or a token.
	Value string `json:"value,omitempty"`
}

// HostedCustomDomain is one declared custom hostname's observed state.
type HostedCustomDomain struct {
	Domain string `json:"domain"`
	// State is the lower-case vocabulary: pending_dns | verifying |
	// issuing | live | failed | conflict | unknown.
	State string `json:"state"`
	// RequiredRecords are the DNS records still to be set. The platform
	// reports them for as long as they matter, so a domain stuck in
	// pending_dns always carries the reason it is stuck.
	RequiredRecords []HostedDNSRecord `json:"required_records,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
	// LiveSince is RFC3339 for when the domain started serving, empty
	// until it does.
	LiveSince string `json:"live_since,omitempty"`
}

// The domain-state vocabulary forge reports.
const (
	DomainStatePendingDNS = "pending_dns"
	DomainStateVerifying  = "verifying"
	DomainStateIssuing    = "issuing"
	DomainStateLive       = "live"
	DomainStateFailed     = "failed"
	DomainStateConflict   = "conflict"
	DomainStateUnknown    = "unknown"
)

// domainStateName renders a DeployCustomDomainState value name in forge's
// lower-case vocabulary. UNSPECIFIED and anything forge does not recognise
// are "unknown" — a control plane newer than this forge reports a state this
// binary has never heard of, and guessing at it is how a not-yet-serving
// domain reads as serving.
func domainStateName(wire string) string {
	switch v := strings.ToLower(strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(wire)), wireDomainStatePrefix)); v {
	case DomainStatePendingDNS, DomainStateVerifying, DomainStateIssuing, DomainStateLive, DomainStateFailed, DomainStateConflict:
		return v
	default:
		return DomainStateUnknown
	}
}

// customDomainsOf decodes the observed per-domain statuses, in the order the
// control plane reported them.
func customDomainsOf(obs *wireObserved) []HostedCustomDomain {
	if obs == nil || len(obs.Domains) == 0 {
		return nil
	}
	out := make([]HostedCustomDomain, 0, len(obs.Domains))
	for _, d := range obs.Domains {
		hd := HostedCustomDomain{Domain: d.Domain, State: domainStateName(d.State), LastError: d.LastError}
		if d.LiveSince != nil {
			hd.LiveSince = d.LiveSince.UTC().Format(time.RFC3339)
		}
		for _, r := range d.RequiredRecords {
			hd.RequiredRecords = append(hd.RequiredRecords, HostedDNSRecord(r))
		}
		out = append(out, hd)
	}
	return out
}

// FormatCustomDomains renders the per-domain block that `forge env status`
// and the post-deploy summary print, one line per domain plus the DNS
// records still required, indented under indent. Empty for a workload with
// no custom domains, so a caller can print it unconditionally.
//
// The DNS records are printed in the shape a registrar's form asks for —
// type, name, value — because the author's next action is to type them into
// that form, and a status line that says "pending DNS" without saying WHICH
// record is missing sends them to a doc instead.
func FormatCustomDomains(indent string, domains []HostedCustomDomain) string {
	if len(domains) == 0 {
		return ""
	}
	var b strings.Builder
	for _, d := range domains {
		fmt.Fprintf(&b, "%s%s: %s", indent, d.Domain, d.State)
		if d.LiveSince != "" {
			fmt.Fprintf(&b, " since %s", d.LiveSince)
		}
		b.WriteString("\n")
		if d.LastError != "" {
			fmt.Fprintf(&b, "%s  error: %s\n", indent, d.LastError)
		}
		for _, r := range d.RequiredRecords {
			fmt.Fprintf(&b, "%s  DNS record to set: %s %s → %s\n", indent, r.Type, r.Name, r.Value)
		}
	}
	return b.String()
}
