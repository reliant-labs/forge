package migrationlint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeMigrationIn(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func findingsForRule(result Result, rule string) []Finding {
	var out []Finding
	for _, f := range result.Findings {
		if f.Rule == rule {
			out = append(out, f)
		}
	}
	return out
}

// TestDuplicateVersionIsAnError is the collision this rule set exists for.
// Two files claiming one version means the schema version cannot say which was
// applied, and whichever reached a database first leaves the other
// permanently unapplied.
func TestDuplicateVersionIsAnError(t *testing.T) {
	dir := t.TempDir()
	writeMigrationIn(t, dir, "20260101120000_add_orgs.up.sql", "CREATE TABLE orgs (id INT);")
	writeMigrationIn(t, dir, "20260101120000_add_teams.up.sql", "CREATE TABLE teams (id INT);")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := findingsForRule(result, RuleDuplicateVersion)
	// Both claimants are reported: a reviewer looking at one file must be
	// told it is contested and by which file.
	if len(got) != 2 {
		t.Fatalf("duplicate-version findings = %d (%+v); want one per claimant (2)", len(got), result.Findings)
	}
	for _, f := range got {
		if f.Severity != SeverityError {
			t.Errorf("severity = %v; want error", f.Severity)
		}
		if !strings.Contains(f.Message, "20260101120000") {
			t.Errorf("message must name the contested version: %q", f.Message)
		}
	}
	if !result.HasErrors() {
		t.Error("a duplicate version must fail the lint")
	}
	if rem := RemediationFor(RuleDuplicateVersion); !strings.Contains(rem, "re-version") {
		t.Errorf("remediation should tell the author to re-version: %q", rem)
	}
}

// TestDistinctVersionsAreClean is the negative: a directory of distinct
// versions, sequential and timestamp mixed, reports nothing. A rule that
// fires on healthy input is worse than no rule.
func TestDistinctVersionsAreClean(t *testing.T) {
	dir := t.TempDir()
	writeMigrationIn(t, dir, "00001_init.up.sql", "CREATE TABLE a (id INT);")
	writeMigrationIn(t, dir, "00002_more.up.sql", "CREATE TABLE b (id INT);")
	writeMigrationIn(t, dir, "20260101120000_add_orgs.up.sql", "CREATE TABLE orgs (id INT);")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsForRule(result, RuleDuplicateVersion); len(got) != 0 {
		t.Errorf("duplicate-version findings on distinct versions = %+v; want none", got)
	}
	if got := findingsForRule(result, RuleNonTimestampVersion); len(got) != 0 {
		t.Errorf("non-timestamp findings = %+v; want none — existing sequential files are never flagged", got)
	}
}

// TestExistingSequentialMigrationsAreNotFlagged is the adoption guarantee. A
// project entirely on sequential numbering must lint clean: the rule is about
// NEW migrations, and asking anyone to renumber history would make it
// unadoptable.
func TestExistingSequentialMigrationsAreNotFlagged(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"00001_a.up.sql", "00002_b.up.sql", "00111_c.up.sql"} {
		writeMigrationIn(t, dir, name, "CREATE TABLE t (id INT);")
	}
	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsForRule(result, RuleNonTimestampVersion); len(got) != 0 {
		t.Errorf("findings on a wholly sequential project = %+v; want none", got)
	}
}

// TestNoGitLeavesSequentialHistoryAlone pins the deliberate silence of the
// no-git fallback.
//
// Without a merge-base, "added on this branch" is unknowable. A hand-typed
// 00112 beside existing timestamps is a real mistake, but it is
// indistinguishable from pre-adoption history here: every sequential version
// sorts below every timestamp, so the newest sequential file and a file added
// years ago look the same. Guessing would flag a project's whole history,
// which is a false positive on every migration. The merge-base test below
// covers the case properly.
func TestNoGitLeavesSequentialHistoryAlone(t *testing.T) {
	dir := t.TempDir()
	writeMigrationIn(t, dir, "00111_existing.up.sql", "CREATE TABLE a (id INT);")
	writeMigrationIn(t, dir, "00112_hand_typed.up.sql", "CREATE TABLE b (id INT);")
	writeMigrationIn(t, dir, "20260101120000_adopted.up.sql", "CREATE TABLE c (id INT);")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsForRule(result, RuleNonTimestampVersion); len(got) != 0 {
		t.Errorf("findings without git = %+v; want none — flagging here would indict every historical migration", got)
	}
}

// TestFourteenDigitNonDateIsFlaggedWithoutGit pins that the rule checks a
// CALENDAR instant, not a digit count, and that this one case needs no branch
// history to judge.
//
// 99999999999999 cannot be a sequence number (no project has 99 trillion
// migrations) and is not a date, so it is a hand-typed value that will sort
// ahead of every genuine timestamp forever. That is wrong regardless of when
// it was added, which is why the no-git fallback still reports it.
func TestFourteenDigitNonDateIsFlaggedWithoutGit(t *testing.T) {
	dir := t.TempDir()
	writeMigrationIn(t, dir, "00111_existing.up.sql", "CREATE TABLE a (id INT);")
	writeMigrationIn(t, dir, "99999999999999_not_a_date.up.sql", "CREATE TABLE b (id INT);")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := findingsForRule(result, RuleNonTimestampVersion)
	if len(got) != 1 {
		t.Fatalf("findings = %+v; want the 14-digit non-date flagged", got)
	}
	if !strings.Contains(got[0].File, "99999999999999") {
		t.Errorf("finding names %q; want the non-date file", got[0].File)
	}
}

// TestMergeBaseMakesNewMeanAddedOnThisBranch is the PR-time check, on a real
// repository. A migration committed on the default branch is NOT new; one
// added on the feature branch IS, even though both are sequential. This is
// what moves the out-of-order failure from a deploy-time refusal to a lint a
// reviewer sees.
func TestMergeBaseMakesNewMeanAddedOnThisBranch(t *testing.T) {
	if testing.Short() {
		t.Skip("runs git; skipped under -short")
	}
	repo := t.TempDir()
	migrations := filepath.Join(repo, "db", "migrations")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "--initial-branch=main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")

	// On main: two sequential migrations. These are history.
	writeMigrationIn(t, migrations, "00001_a.up.sql", "CREATE TABLE a (id INT);")
	writeMigrationIn(t, migrations, "00002_b.up.sql", "CREATE TABLE b (id INT);")
	git("add", "-A")
	git("commit", "-qm", "main migrations")

	// On a feature branch: a hand-typed sequential migration. New.
	git("checkout", "-q", "-b", "feature")
	writeMigrationIn(t, migrations, "00003_new_on_branch.up.sql", "CREATE TABLE c (id INT);")
	git("add", "-A")
	git("commit", "-qm", "branch migration")

	result, err := LintMigrationsDir(migrations, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := findingsForRule(result, RuleNonTimestampVersion)
	if len(got) != 1 {
		t.Fatalf("findings = %+v; want exactly the migration added on this branch", got)
	}
	if !strings.Contains(got[0].File, "00003_new_on_branch") {
		t.Errorf("finding names %q; want the branch's migration", got[0].File)
	}

	// A timestamp on the branch is the correct form and must lint clean.
	if err := os.Remove(filepath.Join(migrations, "00003_new_on_branch.up.sql")); err != nil {
		t.Fatal(err)
	}
	writeMigrationIn(t, migrations, "20260101120000_new_on_branch.up.sql", "CREATE TABLE c (id INT);")
	result, err = LintMigrationsDir(migrations, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if got := findingsForRule(result, RuleNonTimestampVersion); len(got) != 0 {
		t.Errorf("a timestamp migration on a branch must lint clean, got %+v", got)
	}
}

// TestLintOutsideAGitRepoStillWorks pins the degradation. The linter runs in
// checkouts it does not control — shallow CI clones, tarballs, directories
// with no git at all — and a version rule that errored there is a rule people
// turn off.
func TestLintOutsideAGitRepoStillWorks(t *testing.T) {
	dir := t.TempDir()
	writeMigrationIn(t, dir, "00001_a.up.sql", "CREATE TABLE a (id INT);")
	writeMigrationIn(t, dir, "00001_b.up.sql", "CREATE TABLE b (id INT);")

	result, err := LintMigrationsDir(dir, DefaultConfig())
	if err != nil {
		t.Fatalf("lint must not error without git: %v", err)
	}
	// The duplicate rule needs no git and must still fire.
	if got := findingsForRule(result, RuleDuplicateVersion); len(got) != 2 {
		t.Errorf("duplicate-version findings = %+v; want both claimants even with no git", got)
	}
}
