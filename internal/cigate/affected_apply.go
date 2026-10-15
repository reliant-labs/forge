package cigate

import (
	"fmt"

	"github.com/reliant-labs/forge/internal/affected"
)

// ApplyAffected narrows the unit-tests row to a's packages. In full mode it
// only annotates the row.
func ApplyAffected(checks []Check, a affected.Result) []Check {
	out := append([]Check(nil), checks...)
	for i := range out {
		c := &out[i]
		if c.Name != UnitTests || c.NotApplicable != "" {
			continue
		}
		switch {
		case a.Full:
			c.Note = "full suite: " + a.Reason
		case len(a.Packages) == 0:
			c.NotApplicable = fmt.Sprintf("affected mode: no package with tests reaches the changes since %s (%s=1 runs the full suite)", a.BaseRef, affected.FullEnv)
		default:
			// `task test -- <packages>` replaces the default ./... with the
			// list. It also skips the frontend lane, which is right: affected
			// mode means every changed file is Go or claimed by a rule.
			c.Argv = append(append(append([]string(nil), c.Argv...), "--"), a.Packages...)
			c.Note = fmt.Sprintf("affected mode: %d of %d packages (since %s)", len(a.Packages), a.Total, a.BaseRef)
		}
	}
	return out
}
