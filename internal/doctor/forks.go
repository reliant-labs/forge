package doctor

// forks.go — forked generated-file check.
//
// A "fork" (`forge generate --accept`) permanently opts a Tier-1
// generated file out of regeneration: forge skips it on every
// subsequent `forge generate`, even with --force. That's a standing
// capability loss — proto / forge.yaml / contract changes stop flowing
// into the forked file — and the failure shows up far from the cause
// (.forge/backlog.md 2026-06-05: a forked wire_gen.go made "adding a
// Deps field" look like a no-op).
//
// CheckForkedFiles surfaces the fork count as a doctor warning so the
// state stays visible in the routine health view, not just in
// `forge generate` output the user may have scrolled past. It's a
// `warn` rather than `fail` because forking is a legal, intentional
// state — the check's job is to keep it a *conscious* one.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// CheckForkedFiles reports how many Tier-1 generated files are
// currently forked (opted out of regeneration) in the project's
// `.forge/checksums.json`. Tier-2 forked entries are excluded — there
// the flag records an intentional scaffold ownership transfer, which
// is the expected steady state.
func CheckForkedFiles(_ context.Context, env *Environment) CheckResult {
	cs, err := checksums.Load(env.ProjectDir)
	if err != nil {
		return CheckResult{
			Status:   StatusWarn,
			Message:  "could not read .forge/checksums.json",
			Evidence: err.Error(),
		}
	}

	var forked []string
	for rel, entry := range cs.Files {
		if !entry.Forked || entry.Tier == 2 {
			continue
		}
		forked = append(forked, rel)
	}
	if len(forked) == 0 {
		return CheckResult{Status: StatusPass, Message: "no forked generated files"}
	}

	sort.Strings(forked)
	return CheckResult{
		Status: StatusWarn,
		Message: fmt.Sprintf("%d forked generated file(s) — forge cannot regenerate them; "+
			"run `forge unfork --merge <path>` to reconcile", len(forked)),
		Evidence: strings.Join(forked, "\n"),
	}
}
