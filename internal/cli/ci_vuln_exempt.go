package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/config"
)

// The Go vulnerability gate's decision layer.
//
// govulncheck itself has no allowlist: it prints what it finds and exits
// non-zero, so a project that links an advisory with NO fixed version has
// only two native options — a permanently red gate, or no gate at all.
// Both are bad in the same direction. A red gate stops being read after
// the second week, and a disabled one cannot report the NEXT advisory,
// which is the one that might actually be reachable.
//
// So forge makes the pass/fail call itself. It runs govulncheck in JSON
// mode (which exits 0 regardless of findings, handing the decision over
// deliberately), keeps the findings govulncheck judged CALLED, and drops
// only the advisory IDs the project has explicitly accepted. Everything
// else fails exactly as before.
//
// What keeps this from becoming a place to hide problems:
//   - Exemptions are per-ID. Accepting one CVE in a module does not accept
//     the module; a new advisory in the same dependency still fails.
//   - Every entry carries a reason and an expiry, and an expired entry
//     fails the gate. The re-review happens on a date instead of never.
//   - An entry matching nothing is reported as stale, so the list shrinks
//     when advisories get fixed rather than silently pre-authorizing a
//     future finding.

// govulncheckFinding is the subset of govulncheck's JSON stream this gate
// reads. The stream is a sequence of concatenated objects, each carrying
// exactly one of "config" / "SBOM" / "progress" / "osv" / "finding"; the
// fields not named here are ignored.
type govulncheckFinding struct {
	OSV   string `json:"osv"`
	Trace []struct {
		Module   string `json:"module"`
		Version  string `json:"version"`
		Package  string `json:"package"`
		Function string `json:"function"`
	} `json:"trace"`
}

// govulncheckMessage is one element of the JSON stream.
type govulncheckMessage struct {
	OSV *struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
	} `json:"osv"`
	Finding *govulncheckFinding `json:"finding"`
}

// vulnFinding is one called advisory, reduced to what the gate reports.
type vulnFinding struct {
	ID      string
	Summary string
	Module  string
	Version string
}

// parseGovulncheckJSON reduces a govulncheck JSON stream to the advisories
// it judged CALLED, in stable ID order.
//
// "Called" is the distinction that makes this gate meaningful, and it is
// carried in the shape of the trace rather than in a field of its own:
// govulncheck emits a finding per level of detail, and only the
// symbol-level one has a Function set on the frame. Module- and
// package-level findings for the same advisory mean "present in the build
// graph", which govulncheck's own text output reports separately as "your
// code doesn't appear to call these". Treating those as failures would
// fail the gate on advisories in code that is compiled but never run.
func parseGovulncheckJSON(r io.Reader) ([]vulnFinding, error) {
	dec := json.NewDecoder(r)

	summaries := map[string]string{}
	called := map[string]vulnFinding{}

	for {
		var msg govulncheckMessage
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("parse govulncheck JSON output: %w", err)
		}

		if msg.OSV != nil {
			summaries[msg.OSV.ID] = msg.OSV.Summary
			continue
		}
		if msg.Finding == nil || len(msg.Finding.Trace) == 0 {
			continue
		}
		// Frame 0 is the vulnerable symbol itself. A Function there is
		// what separates a call trace from a mere presence report.
		top := msg.Finding.Trace[0]
		if top.Function == "" {
			continue
		}
		called[msg.Finding.OSV] = vulnFinding{
			ID:      msg.Finding.OSV,
			Module:  top.Module,
			Version: top.Version,
		}
	}

	out := make([]vulnFinding, 0, len(called))
	for id, f := range called {
		f.Summary = summaries[id]
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// vulnGateResult is the verdict over one scan.
type vulnGateResult struct {
	// Blocking are findings that must fail the gate: not exempted, or
	// exempted by an entry that has expired.
	Blocking []vulnFinding
	// Accepted are findings suppressed by a live exemption.
	Accepted []vulnFinding
	// StaleExemptions name entries that matched no finding.
	StaleExemptions []string
	// Malformed name entries that cannot be honored as written.
	Malformed []string
}

// evaluateVulnFindings applies the project's exemptions to a scan.
//
// now is injected so the expiry rule is testable without waiting for a
// calendar date to pass.
func evaluateVulnFindings(findings []vulnFinding, exemptions []config.CIVulnExemption, now time.Time) vulnGateResult {
	var res vulnGateResult

	// live maps an advisory ID to the exemption accepting it. An entry
	// that is malformed or expired is deliberately NOT entered here, so
	// its advisory falls through and blocks — the fail-closed direction.
	live := map[string]config.CIVulnExemption{}
	matched := map[string]bool{}

	for _, ex := range exemptions {
		id := strings.TrimSpace(ex.ID)
		switch {
		case id == "":
			res.Malformed = append(res.Malformed,
				"an exemption entry has no `id` — it cannot accept anything, so it is ignored")
			continue
		case strings.TrimSpace(ex.Reason) == "":
			res.Malformed = append(res.Malformed, fmt.Sprintf(
				"%s has no `reason` — an unexplained exemption cannot be reviewed, so it is not honored", id))
			continue
		case strings.TrimSpace(ex.Expires) == "":
			res.Malformed = append(res.Malformed, fmt.Sprintf(
				"%s has no `expires` — an exemption without a review date never gets re-examined, so it is not honored", id))
			continue
		}

		expiry, err := time.Parse("2006-01-02", strings.TrimSpace(ex.Expires))
		if err != nil {
			res.Malformed = append(res.Malformed, fmt.Sprintf(
				"%s has an unparseable `expires: %s` — want YYYY-MM-DD, so it is not honored", id, ex.Expires))
			continue
		}
		// Expiry is end-of-day: an exemption dated today is still live
		// today and lapses tomorrow.
		if now.After(expiry.AddDate(0, 0, 1)) {
			res.Malformed = append(res.Malformed, fmt.Sprintf(
				"%s expired on %s — re-review it (is it still unreachable? is there a fix now?) and either "+
					"extend `expires` with a fresh justification or remove the entry", id, ex.Expires))
			continue
		}
		live[id] = ex
	}

	for _, f := range findings {
		if _, ok := live[f.ID]; ok {
			matched[f.ID] = true
			res.Accepted = append(res.Accepted, f)
			continue
		}
		res.Blocking = append(res.Blocking, f)
	}

	for id := range live {
		if !matched[id] {
			res.StaleExemptions = append(res.StaleExemptions, id)
		}
	}
	sort.Strings(res.StaleExemptions)
	return res
}

// report prints the verdict. Accepted findings are printed in full, with
// their justification: an accepted risk that scrolls by as a count is one
// nobody re-reads, and this output is the only place most people will
// ever see the reasoning.
func (r vulnGateResult) report(w io.Writer) {
	for _, f := range r.Accepted {
		fmt.Fprintf(w, "  ⚠️  accepted: %s (%s@%s)\n", f.ID, f.Module, f.Version)
		if f.Summary != "" {
			fmt.Fprintf(w, "      %s\n", f.Summary)
		}
	}
	for _, id := range r.StaleExemptions {
		fmt.Fprintf(w, "  🧹 exemption %s matched nothing in this scan — the advisory is gone or now "+
			"unreachable. Remove it from forge.yaml (ci.vuln_scan.exemptions) so it cannot silently "+
			"accept a future finding.\n", id)
	}
	for _, m := range r.Malformed {
		fmt.Fprintf(w, "  ❌ exemption not honored: %s\n", m)
	}
	for _, f := range r.Blocking {
		fmt.Fprintf(w, "  ❌ %s (%s@%s)\n", f.ID, f.Module, f.Version)
		if f.Summary != "" {
			fmt.Fprintf(w, "      %s\n", f.Summary)
		}
	}
}
