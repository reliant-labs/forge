package cli

// Is this checkout's FILE ledger the ledger?
//
// A self-managed env records its promotions in .forge/promotions/<env>.jsonl,
// committed to git. So "what is prod supposed to run" is answered by whatever
// commit this checkout happens to have. A checkout that has not pulled the
// latest `chore(ledger): record vX` commit holds an OLDER promotion, and every
// verdict computed against it is wrong in a way the verdict cannot show:
//
//   - the cluster runs the new release → every image reports DRIFT from the
//     old one, and an operator chases a deploy that went fine;
//   - the cluster ALSO runs the old release (the new one never deployed) →
//     MATCH, and the gate goes green on exactly the failure it exists for.
//
// The file is append-only, which makes the comparison exact: the working
// tree's log and the upstream's log are either equal, or one is a prefix of
// the other, or they diverged. No heuristics, no timestamps.
//
// The comparison is against origin's DEFAULT branch — where the ledger's
// truth lands — not this branch's upstream: a feature branch cut before the
// last release is behind in the sense that matters even when it is level
// with its own remote. It reads only what the last fetch brought in and never
// fetches itself: verify is a read-only command that runs in shared
// checkouts, and a network call it did not need would make an offline
// verify fail for a reason unrelated to the cluster.
//
// The hosted backend implements none of this. A control plane IS the ledger;
// there is no copy that can be behind.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ledgerFreshness is the answer to "does this checkout hold the newest copy
// of the env's ledger".
type ledgerFreshness int

const (
	// ledgerFreshnessUnknown: no comparison could be made — not a git
	// checkout, no origin, no default branch fetched. Reported, never a
	// failure: a project outside git is a supported way to run forge.
	ledgerFreshnessUnknown ledgerFreshness = iota
	// ledgerCurrent: the working tree's log equals origin's.
	ledgerCurrent
	// ledgerBehind: origin's log extends this one. Origin knows promotions
	// this checkout does not, so the declaration verify reads is stale.
	ledgerBehind
	// ledgerAhead: this log extends origin's — the normal state between
	// recording a release locally and merging the ledger PR.
	ledgerAhead
	// ledgerDiverged: neither is a prefix of the other. Two writers
	// appended to different copies; neither copy is the ledger.
	ledgerDiverged
)

func (f ledgerFreshness) String() string {
	switch f {
	case ledgerCurrent:
		return "current"
	case ledgerBehind:
		return "behind"
	case ledgerAhead:
		return "ahead"
	case ledgerDiverged:
		return "diverged"
	default:
		return "unknown"
	}
}

// MarshalJSON emits the lowercase name, so --json carries the same word the
// text report prints.
func (f ledgerFreshness) MarshalJSON() ([]byte, error) {
	return []byte(`"` + f.String() + `"`), nil
}

// UnmarshalJSON reads the name back, REFUSING one it does not know — the
// house rule for every --json enum. Defaulting an unknown word to "current"
// would read a newer binary's "this ledger is stale" as "fine".
func (f *ledgerFreshness) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return fmt.Errorf("ledger state must be a string: %w", err)
	}
	for _, s := range []ledgerFreshness{ledgerFreshnessUnknown, ledgerCurrent, ledgerBehind, ledgerAhead, ledgerDiverged} {
		if s.String() == name {
			*f = s
			return nil
		}
	}
	return fmt.Errorf("unknown ledger state %q (expected current, behind, ahead, diverged or unknown)", name)
}

// stale reports whether the declaration cannot be trusted: a verdict against
// it would be about a ledger that is not the ledger.
func (f ledgerFreshness) stale() bool { return f == ledgerBehind || f == ledgerDiverged }

// ledgerFreshnessReport is what verify says about the declaration's source.
type ledgerFreshnessReport struct {
	State ledgerFreshness `json:"state"`
	// Ref is the upstream compared against, e.g. "origin/main".
	Ref string `json:"ref,omitempty"`
	// LocalEntries and UpstreamEntries count promotions in each copy.
	// "Behind" is only ever as of this checkout's last fetch of Ref.
	LocalEntries    int `json:"local_entries"`
	UpstreamEntries int `json:"upstream_entries"`
	// Detail explains the state in one line, including why it is unknown.
	Detail string `json:"detail,omitempty"`
}

// ledgerFreshnessChecker is the optional half of a binding store that can say
// whether its copy of an env's ledger is the newest one. Declared here, at
// the consumer: the file store implements it, the hosted store has nothing to
// be behind and does not.
type ledgerFreshnessChecker interface {
	LedgerFreshness(ctx context.Context, env string) ledgerFreshnessReport
}

// ledgerGitTimeout bounds each read-only git call. A wedged git must not hang
// a verify whose real work is a cluster read.
const ledgerGitTimeout = 10 * time.Second

// LedgerFreshness compares env's promotion log in the working tree with the
// same path on origin's default branch.
func (s fileBindingStore) LedgerFreshness(ctx context.Context, env string) ledgerFreshnessReport {
	git := func(args ...string) (string, error) {
		cctx, cancel := context.WithTimeout(ctx, ledgerGitTimeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, "git", args...)
		cmd.Dir = s.projectDir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
	unknown := func(detail string) ledgerFreshnessReport {
		return ledgerFreshnessReport{State: ledgerFreshnessUnknown, Detail: detail}
	}

	// The project's path WITHIN the repository, as git computes it. Asking
	// git — rather than filepath.Rel against --show-toplevel — is what
	// keeps a symlinked checkout path (macOS's /var → /private/var, a
	// ~/src symlink) from reading as "outside the repository": git resolves
	// both sides the same way.
	prefix, err := git("rev-parse", "--show-prefix")
	if err != nil {
		return unknown("not a git checkout, so there is no upstream copy of the ledger to compare against")
	}
	ref := defaultUpstreamRef(git)
	if ref == "" {
		return unknown("no origin default branch has been fetched (tried origin/HEAD, origin/main, origin/master)")
	}

	logPath := promotionLogPath(s.projectDir, env)
	logRel, err := filepath.Rel(s.projectDir, logPath)
	if err != nil {
		return unknown(fmt.Sprintf("locate %s: %v", logPath, err))
	}
	rel := strings.TrimSpace(prefix) + filepath.ToSlash(logRel)

	local, err := os.ReadFile(logPath) //nolint:gosec // promotionLogPath confines the env stem to the promotions dir
	if err != nil && !os.IsNotExist(err) {
		return unknown(fmt.Sprintf("read %s: %v", logPath, err))
	}
	// An absent file on the upstream is an empty log, exactly as an absent
	// file locally is: "never promoted there".
	upstream, _ := git("show", ref+":"+rel)

	report := ledgerFreshnessReport{Ref: ref}
	localLines, upstreamLines := ledgerLines(string(local)), ledgerLines(upstream)
	report.LocalEntries, report.UpstreamEntries = len(localLines), len(upstreamLines)
	report.State = compareLedgerLogs(localLines, upstreamLines)
	switch report.State {
	case ledgerCurrent:
		report.Detail = fmt.Sprintf("%s matches %s", rel, ref)
	case ledgerBehind:
		report.Detail = fmt.Sprintf("%s on %s has %d promotion(s) this checkout does not — the release verify compares against is stale",
			rel, ref, len(upstreamLines)-len(localLines))
	case ledgerAhead:
		report.Detail = fmt.Sprintf("this checkout has %d promotion(s) not yet on %s — merge the ledger change so other checkouts verify against it",
			len(localLines)-len(upstreamLines), ref)
	case ledgerDiverged:
		report.Detail = fmt.Sprintf("%s and %s each hold promotions the other does not — two copies of the ledger were written separately, and neither is authoritative",
			rel, ref)
	}
	return report
}

// defaultUpstreamRef is origin's default branch as this checkout last saw it.
func defaultUpstreamRef(git func(...string) (string, error)) string {
	if out, err := git("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return ref
		}
	}
	for _, ref := range []string{"origin/main", "origin/master"} {
		if _, err := git("rev-parse", "--verify", "-q", ref); err == nil {
			return ref
		}
	}
	return ""
}

// ledgerLines splits a promotion log into its non-blank lines, ignoring
// line-ending differences a checkout's autocrlf may have introduced.
func ledgerLines(log string) []string {
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(log, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// compareLedgerLogs classifies two append-only logs. Because entries are only
// ever appended, "one is a prefix of the other" is the whole relation.
func compareLedgerLogs(local, upstream []string) ledgerFreshness {
	common := min(len(local), len(upstream))
	for i := 0; i < common; i++ {
		if local[i] != upstream[i] {
			return ledgerDiverged
		}
	}
	switch {
	case len(local) == len(upstream):
		return ledgerCurrent
	case len(local) < len(upstream):
		return ledgerBehind
	default:
		return ledgerAhead
	}
}
