package migrationlint

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/migrationver"
)

// RuleDuplicateVersion is the rule ID for two migrations claiming one version
// number.
//
// Two files under the same version is not a style problem, it is a schema that
// cannot be described. golang-migrate records ONE integer per applied
// migration, so "the schema is at 91" cannot say WHICH 91 — and whichever file
// reaches a shared database first leaves the other permanently unapplied while
// every later `up` reports the schema current.
//
// This is the failure this whole rule set exists for: ten version numbers in
// control-plane were each claimed by two different migrations across branches,
// and one number was claimed on eight separate branches.
const RuleDuplicateVersion = "duplicate-migration-version"

// DuplicateVersionRemediation is the fix text for a duplicate-version finding.
const DuplicateVersionRemediation = `two migrations claim the same version, so the schema version cannot say which one was applied — re-version the newer file to a fresh UTC timestamp (` + "`forge db migration new`" + ` allocates one, or rename it to <YYYYMMDDHHMMSS>_<same-name>.up.sql). The file that has already been applied somewhere must keep its version; rename the one that has not`

// RuleNonTimestampVersion is the rule ID for a NEW migration that is not
// timestamp-versioned.
//
// Sequential numbering is what makes parallel branches collide: every branch
// cut from the same commit reads the same highest number and picks the same
// next one. A timestamp cannot collide without two branches allocating in the
// same second.
const RuleNonTimestampVersion = "non-timestamp-migration-version"

// NonTimestampVersionRemediation is the fix text for a non-timestamp finding.
const NonTimestampVersionRemediation = `new migrations must be versioned with a UTC timestamp (<YYYYMMDDHHMMSS>_<name>.up.sql) so parallel branches cannot claim the same version — create it with ` + "`forge db migration new <name>`" + `, or rename the file to a 14-digit UTC timestamp. Existing sequential migrations are left alone: a timestamp sorts after any of them, so nothing needs renumbering`

// lintVersions reports every duplicate version among the migration files of
// one directory, and every NEW migration that is not timestamp-versioned.
//
// files are the *.up.sql paths of a migrations directory.
func lintVersions(files []string, mergeBaseMax uint64, haveMergeBase bool) []Finding {
	var findings []Finding

	byVersion := map[uint64][]string{}
	var versions []uint64
	for _, file := range files {
		v, ok := migrationver.ParseVersion(filepath.Base(file))
		if !ok {
			continue
		}
		if _, seen := byVersion[v]; !seen {
			versions = append(versions, v)
		}
		byVersion[v] = append(byVersion[v], file)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })

	for _, v := range versions {
		claimants := byVersion[v]
		if len(claimants) < 2 {
			continue
		}
		sort.Strings(claimants)
		// Report on every claimant: a reviewer looking at one file has to
		// be told it is contested, and which file contests it.
		others := make([]string, 0, len(claimants))
		for _, c := range claimants {
			others = append(others, filepath.Base(c))
		}
		for _, file := range claimants {
			findings = append(findings, Finding{
				File:     file,
				Line:     1,
				Rule:     RuleDuplicateVersion,
				Severity: SeverityError,
				Message: fmt.Sprintf("migration version %d is claimed by %d files (%s) — the schema version cannot say which one was applied",
					v, len(claimants), strings.Join(others, ", ")),
			})
		}
	}

	// THE "NEW" TEST. A migration is new when its version is greater than
	// the highest version on the default branch's merge-base: that is
	// exactly "added on this branch", which is the set a PR can still fix.
	// Every pre-existing sequential migration is at or below that mark and
	// is therefore never flagged — the rule cannot ask anyone to renumber
	// history.
	//
	// Without git (a shallow clone, a tarball, no default branch) newness is
	// unknowable and the rule falls back to a directory-local signal. See
	// isNewNonTimestamp.
	for _, file := range files {
		base := filepath.Base(file)
		versionText, _, ok := strings.Cut(base, "_")
		if !ok {
			continue
		}
		v, ok := migrationver.ParseVersion(base)
		if !ok {
			continue
		}
		if migrationver.IsTimestamp(versionText) {
			continue
		}
		if !isNewNonTimestamp(v, versions, mergeBaseMax, haveMergeBase) {
			continue
		}
		findings = append(findings, Finding{
			File:     file,
			Line:     1,
			Rule:     RuleNonTimestampVersion,
			Severity: SeverityError,
			Message:  fmt.Sprintf("migration version %q is new but not a UTC timestamp — parallel branches allocating sequential numbers claim the same version", versionText),
		})
	}

	return findings
}

// isNewNonTimestamp decides whether a sequential migration is NEW, which is
// the only kind the timestamp rule may flag.
//
// WITH GIT the answer is exact: new means "added on this branch", i.e. a
// version above the highest one at the merge-base with the default branch.
// That is the set a PR can still fix, and it is why pre-existing sequential
// history is never flagged.
//
// WITHOUT GIT "added on this branch" is unknowable, and the rule narrows to
// what is wrong REGARDLESS of newness: a version with 14 or more digits that
// is not a real calendar instant. 99999999999999 is the canonical case — it
// cannot be a sequence number (no project has 99 trillion migrations) and it
// cannot be a timestamp, so it is a hand-typed value that will sort ahead of
// every genuine timestamp forever. No branch history is needed to call that a
// mistake.
//
// Everything else is left alone without git, including a hand-typed 00112 in
// a directory that already holds timestamps. That case is real, but it is
// indistinguishable from history here: sequential versions all sort BELOW
// every timestamp, so "the newest sequential file" and "a sequential file
// added years ago" look identical. Guessing would flag a project's entire
// pre-adoption history — 111 files, in control-plane's case — which is a
// false positive on every migration and precisely how a rule gets switched
// off. With git, the merge-base answers it exactly, and git is present in
// every context where a PR is being reviewed.
func isNewNonTimestamp(version uint64, allVersions []uint64, mergeBaseMax uint64, haveMergeBase bool) bool {
	if haveMergeBase {
		return version > mergeBaseMax
	}
	return len(fmt.Sprint(version)) >= migrationver.Digits
}

// repoRootOf returns the git repository root containing dir, or dir itself
// when there is none. Every git call is scoped to it, so the lint never reads
// a parent repository that happens to sit above an untracked project.
func repoRootOf(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	out, ok := runGit(abs, "rev-parse", "--show-toplevel")
	if !ok {
		return abs
	}
	root := strings.TrimSpace(out)
	if root == "" {
		return abs
	}
	return root
}

// mergeBaseMaxVersion returns the highest migration version present in dir as
// of the merge-base between HEAD and the default branch — the state this
// branch started from.
//
// Every failure is reported as "unknown" (false) rather than an error. The
// linter runs in a checkout it does not control: a shallow CI clone, an
// exported tarball, a repository with no default branch, or a migrations
// directory that is not tracked at all. A version rule that failed the lint
// in those conditions would be a rule people disable.
func mergeBaseMaxVersion(repoRoot, migrationsDir string) (uint64, bool) {
	defaultBranch, ok := defaultBranchRef(repoRoot)
	if !ok {
		return 0, false
	}
	base, ok := runGit(repoRoot, "merge-base", "HEAD", defaultBranch)
	if !ok {
		return 0, false
	}
	// Both paths go through EvalSymlinks before Rel. `git rev-parse
	// --show-toplevel` reports a fully resolved path, while the caller's dir
	// may still contain a symlink — on macOS every /var/... temp dir is
	// really /private/var/..., so comparing the two unresolved produces a
	// relative path like ../../private/var/... that ls-tree cannot resolve.
	// The rule then silently degraded to its no-git fallback, which is the
	// worst outcome: no error, no finding, and a reviewer told nothing.
	rel, err := filepath.Rel(resolvePath(repoRoot), resolvePath(migrationsDir))
	if err != nil {
		return 0, false
	}
	out, ok := runGit(repoRoot, "ls-tree", "--name-only", strings.TrimSpace(base)+":"+filepath.ToSlash(rel))
	if !ok {
		// The directory did not exist at the merge-base: every migration
		// in it is new, which is a real and useful answer (0).
		return 0, true
	}
	var max uint64
	for _, name := range strings.Split(out, "\n") {
		name = strings.TrimSpace(name)
		if name == "" || !strings.HasSuffix(name, ".sql") {
			continue
		}
		if v, ok := migrationver.ParseVersion(name); ok && v > max {
			max = v
		}
	}
	return max, true
}

// defaultBranchRef resolves the default branch to compare against, preferring
// the remote's own declaration over a guess.
func defaultBranchRef(repoRoot string) (string, bool) {
	// What the remote says its default branch is — the authoritative answer
	// when the repository has a remote.
	if out, ok := runGit(repoRoot, "symbolic-ref", "refs/remotes/origin/HEAD"); ok {
		ref := strings.TrimSpace(out)
		if ref != "" {
			return ref, true
		}
	}
	// A local-only repository, or one whose origin/HEAD was never set.
	for _, candidate := range []string{"origin/main", "origin/master", "main", "master"} {
		if _, ok := runGit(repoRoot, "rev-parse", "--verify", candidate); ok {
			return candidate, true
		}
	}
	return "", false
}

// resolvePath returns path with every symlink resolved, falling back to the
// input when it cannot be resolved. See mergeBaseMaxVersion for why this
// matters.
func resolvePath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// runGit runs one git command in dir, returning its stdout and whether it
// succeeded. A missing git binary is indistinguishable from a failed command
// on purpose: both mean "cannot know", which is the only thing callers act on.
func runGit(dir string, args ...string) (string, bool) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return string(out), true
}
