// Package cli — `forge audit` prerequisites category.
//
// Surfaces the env's DECLARED external prerequisites (forge.ExternalSecret /
// forge.DNSRecord on Bundle.required_secrets / required_dns) — the
// out-of-band facts a deploy depends on but forge does NOT create. Today
// these prereqs live only in KCL docstrings, so `forge deploy` renders
// green and THEN cert-manager's ACME challenge or the workspace-proxy DNS
// hangs silently because the prereq was never satisfied. Modeling them as
// first-class entities turns the docstring into:
//
//   - a render-time CHECKLIST (this audit category + the deploy banner),
//   - a deploy PREFLIGHT block on a declared-required-but-absent
//     ExternalSecret (see cluster.Preflight / RequiredSecrets), and
//   - a cross-secret BYTE-MATCH consistency check: ExternalSecrets sharing
//     a `value_group` carry the SAME logical value; this category reports
//     the group membership (the KCL schema already blocks a group whose
//     members declare different key sets, and the live byte-compare runs in
//     the preflight).
//
// Like auditIngress this renders the dev env (the only env every project is
// guaranteed to have) purely to ENUMERATE the declarations; the category is
// informational (status ok/warn), never an error — a declared prereq is a
// good thing, and whether it's SATISFIED is a live-cluster question the
// deploy preflight answers, not a static audit.
package cli

import (
	"context"
	"fmt"
	"sort"

	"github.com/reliant-labs/forge/internal/cli/audittype"
	"github.com/reliant-labs/forge/internal/config"
)

// auditPrerequisites renders the dev env and reports the declared external
// prerequisites. Degrades to warn (never error) when KCL can't be evaluated
// — a missing toolchain shouldn't fail the whole audit, and an absent prereq
// declaration is the unremarkable default.
func auditPrerequisites(cfg *config.ProjectConfig, projectDir string) audittype.Category {
	if cfg == nil {
		return audittype.Category{Status: audittype.StatusError, Summary: "no forge.yaml"}
	}
	entities, err := RenderKCL(context.Background(), projectDir, "dev")
	if err != nil {
		return audittype.Category{
			Status:  audittype.StatusWarn,
			Summary: fmt.Sprintf("could not evaluate dev KCL: %v", err),
		}
	}
	return crossCheckPrereqs(entities.RequiredSecrets, entities.RequiredDNS)
}

// crossCheckPrereqs is the pure decision core: takes the declared external
// Secret + DNS prerequisites and returns the audittype.Category. Split out
// so unit tests exercise it without shelling kcl.
//
// Findings (all informational — a declared prereq never makes the audit
// fail):
//
//   - one line per declared ExternalSecret (name @ namespace, keys, reason),
//   - one line per declared DNSRecord (host, type, target, reason),
//   - one line per cross-secret byte-match group naming its members (so the
//     operator sees which Secrets must carry identical bytes).
func crossCheckPrereqs(secrets []ExternalSecretEntity, dns []DNSRecordEntity) audittype.Category {
	var findings []string

	for _, s := range secrets {
		line := fmt.Sprintf("secret: %s/%s keys=%v", s.Namespace, s.Name, s.Keys)
		if s.Reason != "" {
			line += " — " + s.Reason
		}
		findings = append(findings, line)
	}
	for _, r := range dns {
		line := fmt.Sprintf("dns: %s (%s)", r.Host, r.Type)
		if r.Target != "" {
			line += " -> " + r.Target
		}
		if r.Reason != "" {
			line += " — " + r.Reason
		}
		findings = append(findings, line)
	}

	// Cross-secret byte-match groups: a value_group with >1 member is a set
	// of Secrets that must carry the same logical bytes (the live compare is
	// a preflight check; here we surface membership). A group of size 1 is a
	// declaration smell — a byte-match group with one member matches nothing
	// — so we surface it as a (non-blocking) note.
	groups := byteMatchGroups(secrets)
	groupIDs := make([]string, 0, len(groups))
	for id := range groups {
		groupIDs = append(groupIDs, id)
	}
	sort.Strings(groupIDs)
	singletonGroups := 0
	for _, id := range groupIDs {
		members := groups[id]
		sort.Strings(members)
		if len(members) == 1 {
			singletonGroups++
			findings = append(findings, fmt.Sprintf(
				"value-group %q: only one member (%s) — a byte-match group with a single Secret matches nothing; add the other ref or drop value_group",
				id, members[0]))
			continue
		}
		findings = append(findings, fmt.Sprintf(
			"value-group %q: %d Secrets must carry identical bytes [%s]",
			id, len(members), joinSorted(members)))
	}

	sort.Strings(findings)

	status := audittype.StatusOK
	if singletonGroups > 0 {
		status = audittype.StatusWarn
	}
	summary := fmt.Sprintf("%d external secret(s), %d dns record(s), %d byte-match group(s)",
		len(secrets), len(dns), len(groups))
	details := map[string]any{
		"external_secrets":  len(secrets),
		"dns_records":       len(dns),
		"byte_match_groups": len(groups),
	}
	if len(findings) > 0 {
		details["findings"] = findings
	}
	return audittype.Category{Status: status, Summary: summary, Details: details}
}

// printPrerequisiteChecklist prints the env's declared external
// prerequisites at deploy time so the out-of-band facts the deploy depends
// on are visible every run (not buried in a docstring). The ExternalSecret
// half is also ENFORCED by the deploy preflight (a missing one BLOCKS); the
// DNS half can't be authoritatively verified, so the checklist is its only
// signal. No-op when the env declares no prereqs.
func printPrerequisiteChecklist(entities *KCLEntities) {
	if entities == nil {
		return
	}
	if len(entities.RequiredSecrets) == 0 && len(entities.RequiredDNS) == 0 {
		return
	}
	fmt.Println("External prerequisites this env depends on (provisioned out-of-band; forge does NOT create them):")
	for _, s := range entities.RequiredSecrets {
		line := fmt.Sprintf("  - Secret %s/%s keys=%v", s.Namespace, s.Name, s.Keys)
		if s.ValueGroup != "" {
			line += fmt.Sprintf(" [value-group %q]", s.ValueGroup)
		}
		if s.Reason != "" {
			line += " — " + s.Reason
		}
		fmt.Println(line)
	}
	for _, r := range entities.RequiredDNS {
		line := fmt.Sprintf("  - DNS %s (%s)", r.Host, r.Type)
		if r.Target != "" {
			line += " -> " + r.Target
		}
		if r.Reason != "" {
			line += " — " + r.Reason
		}
		fmt.Println(line)
	}
	if len(entities.RequiredSecrets) > 0 {
		fmt.Println("  (declared Secrets are verified by the deploy preflight; a missing one blocks the deploy.)")
	}
	fmt.Println()
}

// byteMatchGroups buckets the declared ExternalSecrets by value_group,
// returning each non-empty group's member labels ("<namespace>/<name>").
// Secrets with no value_group (standalone prereqs) are excluded.
func byteMatchGroups(secrets []ExternalSecretEntity) map[string][]string {
	groups := map[string][]string{}
	for _, s := range secrets {
		if s.ValueGroup == "" {
			continue
		}
		groups[s.ValueGroup] = append(groups[s.ValueGroup], s.Namespace+"/"+s.Name)
	}
	return groups
}

// joinSorted renders a sorted, comma-separated member list.
func joinSorted(items []string) string {
	cp := append([]string(nil), items...)
	sort.Strings(cp)
	out := ""
	for i, it := range cp {
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}
