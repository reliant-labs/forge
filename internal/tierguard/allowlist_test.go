package tierguard

// The documented exemption list, and the test that keeps it honest.
//
// It lives in a _test.go file because it is consumed only by the guard.
// Keeping it out of the production build also means an EMPTY list is
// representable without leaving a struct in production code whose fields
// nothing ever writes (which internal/deadcodeguard correctly flags as a
// phantom-field: a shape only tests can produce).

import (
	"strings"
	"testing"
)

// allowEntry documents one path that is byte-constant across the
// fixtures TODAY but is genuinely per-project in principle, so Tier-1
// remains the right tier for it.
//
// A Reason is mandatory and is enforced by
// TestAllowListEntriesAreJustified: this list is a record of decisions,
// not a place to park failures. Each reason must name the user input the
// file WOULD respond to and say why the fixtures do not happen to move
// it.
type allowEntry struct {
	Path   string
	Reason string
}

// allowList is EMPTY BY DESIGN. Nothing has been granted an exemption:
// every path the differential render calls constant is reported as a
// mis-tier candidate, and deciding what to do about each one is
// follow-up work, not something this guard should pre-absolve.
//
// Two paths were considered and deliberately NOT exempted, because in
// both cases the honest fix was to make the fixtures exercise the input:
//
//   - pkg/middleware/procedures_gen.go looked constant until fixture B
//     declared an RPC with `auth_required: false`. It is now genuinely
//     Derived. An exemption would have hidden a real, testable property.
//   - pkg/config/config.go looked like it "must" be per-project because
//     of its `type Config = configv1.AppConfig` alias. Adding a component
//     config block to proto/config/v1/config.proto moved
//     deploy/kcl/config_projection.k and gen/config/v1/config.pb.go and
//     left config.go byte-identical — so the alias text is invariant and
//     the file stays a reported candidate on the evidence.
//
// Add an entry only with a reason naming the specific user input the file
// is a function of and why no fixture moves it — and prefer extending a
// fixture instead, which turns the entry into a real Derived verdict and
// is strictly better evidence than an exemption.
var allowList = []allowEntry{}

// allowed indexes allowList by path.
func allowed() map[string]string {
	out := map[string]string{}
	for _, e := range allowList {
		out[e.Path] = e.Reason
	}
	return out
}

// TestAllowListEntriesAreJustified keeps allowList a record of decisions
// rather than a place to hide failures: every entry needs a reason with
// enough substance to argue with, and an entry for a path the guard no
// longer calls constant is dead weight that must be removed.
func TestAllowListEntriesAreJustified(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range allowList {
		if strings.TrimSpace(e.Path) == "" {
			t.Error("allowList entry with an empty Path")
			continue
		}
		if seen[e.Path] {
			t.Errorf("allowList has duplicate entries for %q", e.Path)
		}
		seen[e.Path] = true

		// A reason short enough to be a shrug is not a reason. Entries
		// must name the user input the file would respond to.
		if len(strings.TrimSpace(e.Reason)) < 40 {
			t.Errorf("allowList entry %q has no substantive reason (%q).\n"+
				"State which user input the file IS a function of, and why no fixture moves it — "+
				"the list is a record of decisions, not a mute suppression list.", e.Path, e.Reason)
		}
	}

	if testing.Short() || len(allowList) == 0 {
		t.Logf("allowList has %d entr(ies); nothing to re-verify against a render", len(allowList))
		return
	}

	// Every exemption must still be load-bearing. A path that now
	// classifies as derived does not need an exemption, and leaving one
	// behind would mask a future regression at that path.
	a, b, identity := renders(t)
	byPath := map[string]Classification{}
	for _, c := range Classify(a, b, identity) {
		byPath[c.Path] = c
	}
	for _, e := range allowList {
		c, ok := byPath[e.Path]
		if !ok {
			t.Errorf("allowList exempts %q, which is not a Tier-1 target in any fixture — "+
				"stale entry, remove it", e.Path)
			continue
		}
		if c.Verdict != Constant {
			t.Errorf("allowList exempts %q but the guard classifies it as %s (%s) — "+
				"the exemption is unnecessary and would mask a real regression there; remove it",
				e.Path, c.Verdict, c.Evidence)
		}
	}
}
