package mlrecs

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/mldiscover"
)

// applyUnifiedDiff applies a unified diff (difflib's output) to base,
// failing on any hunk whose context or removed lines are not in base at
// the line the hunk names: a diff that applies is one taken against base.
func applyUnifiedDiff(base, diff string) (string, error) {
	src := strings.SplitAfter(base, "\n")
	if src[len(src)-1] == "" {
		src = src[:len(src)-1]
	}
	lines := strings.SplitAfter(diff, "\n")
	var out []string
	next := 0 // index in src of the first line not yet copied
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if strings.HasPrefix(l, "--- ") || strings.HasPrefix(l, "+++ ") || l == "" {
			continue
		}
		if !strings.HasPrefix(l, "@@ -") {
			return "", fmt.Errorf("line %d: expected a hunk header, got %q", i, l)
		}
		spec := strings.Fields(l)[1][1:] // "58,6" of "-58,6"
		start, err := strconv.Atoi(strings.SplitN(spec, ",", 2)[0])
		if err != nil {
			return "", fmt.Errorf("hunk header %q: %w", l, err)
		}
		at := start - 1
		if strings.HasSuffix(spec, ",0") {
			at = start // an empty source range names the line before it
		}
		if at < next {
			return "", fmt.Errorf("hunk %q overlaps the previous one", l)
		}
		out = append(out, src[next:at]...)
		next = at
		for i+1 < len(lines) && lines[i+1] != "" && !strings.HasPrefix(lines[i+1], "@@ ") {
			i++
			h := lines[i]
			body := h[1:]
			switch h[0] {
			case ' ', '-':
				if next >= len(src) || src[next] != body {
					return "", fmt.Errorf("hunk %q: base line %d is %q, the diff expects %q", l, next+1, at1(src, next), body)
				}
				if h[0] == ' ' {
					out = append(out, body)
				}
				next++
			case '+':
				out = append(out, body)
			default:
				return "", fmt.Errorf("hunk %q: unexpected line %q", l, h)
			}
		}
	}
	out = append(out, src[next:]...)
	return strings.Join(out, ""), nil
}

func at1(src []string, i int) string {
	if i < len(src) {
		return src[i]
	}
	return "<end of file>"
}

// TestEveryActionableFindingHasAnApplicableDiff: each undismissed FixKCL
// finding, on every fixture, carries a diff of its own that applies to the
// ml.k Propose writes and yields the proposal with that finding applied.
// Findings that share one suggested install (the DCGM field-set findings
// all reconfigure the one exporter) each activate it.
func TestEveryActionableFindingHasAnApplicableDiff(t *testing.T) {
	checked := map[string]bool{}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			s := loadSnapshot(t, name)
			inv := mldiscover.Discover(s)
			popts := ProposeOptions{Decisions: fixtureDecisions(name)}
			fs := Evaluate(s, inv, Options{Decisions: popts.Decisions, Propose: popts})
			base := Propose(inv, fs, popts)
			for _, f := range fs {
				if f.Fix.Kind != FixKCL || f.Dismissed != nil {
					continue
				}
				checked[name+"/"+f.ID] = true
				if f.Fix.KCLDiff == "" {
					t.Errorf("%s: fix.kind=kcl and no kcl_diff: applying it cannot change the proposal", f.ID)
					continue
				}
				got, err := applyUnifiedDiff(base, f.Fix.KCLDiff)
				if err != nil {
					t.Errorf("%s: kcl_diff does not apply to the proposal: %v", f.ID, err)
					continue
				}
				if want := propose(inv, fs, popts, map[string]bool{f.ID: true}); got != want {
					t.Errorf("%s: applying kcl_diff does not yield the proposal with the finding applied", f.ID)
				}
			}
		})
	}
	// The probed forge stack has three DCGM field-set findings on one
	// forge-installed exporter; each must be actionable on its own.
	for _, id := range []string{"dcgm-profiling-off", "accel-error-counters-off", "dcgm-nvlink-off"} {
		if !checked["probed-forge-stack/"+id] {
			t.Errorf("probed-forge-stack: no actionable %s finding to check", id)
		}
	}
}
