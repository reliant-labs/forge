package doctor

// forks.go — disowned generated-file check.
//
// A disowned file (`forge disown`) is a deliberate, one-way transfer of
// a generated file to user ownership: forge never regenerates it again.
// Unlike the old fork limbo this is a legitimate END STATE, so the
// check is informational — it reports the count and the paths (PASS,
// never a warning) purely so the routine health view keeps the
// project's ownership map visible. Legacy `forked: true` entries (left
// by pre-disown forge versions, converted on the next `forge generate`)
// are converted to disowned.json by the legacy-manifest migration on
// the next `forge generate`, so this check reads one source of truth.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// CheckDisownedFiles reports how many generated files have been
// disowned (transferred to permanent user ownership) in the project's
// `.forge/disowned.json`. Informational: disowning is a sanctioned end
// state, so the result is always a pass — the value is the visibility.
func CheckDisownedFiles(_ context.Context, env *Environment) CheckResult {
	cs, err := checksums.Load(env.ProjectDir)
	if err != nil {
		return CheckResult{
			Status:   StatusWarn,
			Message:  "could not read .forge/disowned.json",
			Evidence: err.Error(),
		}
	}

	var disowned []string
	for rel := range cs.Disowned {
		disowned = append(disowned, rel)
	}
	if len(disowned) == 0 {
		return CheckResult{Status: StatusPass, Message: "no disowned generated files"}
	}

	sort.Strings(disowned)
	return CheckResult{
		Status: StatusPass,
		Message: fmt.Sprintf("%d disowned generated file(s) — user-owned; forge never regenerates them "+
			"(re-adopt one by deleting it and running `forge generate`)", len(disowned)),
		Evidence: strings.Join(disowned, "\n"),
	}
}
