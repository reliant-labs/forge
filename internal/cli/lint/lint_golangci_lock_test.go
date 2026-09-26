// File: internal/cli/lint/lint_golangci_lock_test.go
//
// `forge lint` must QUEUE for golangci-lint's machine-global lock, not fail
// on it.
//
// golangci-lint takes an exclusive lock on $TMPDIR/golangci-lint.lock — one
// path for every golangci-lint on the machine — and by default gives up after
// five seconds with exit 3, "parallel golangci-lint is running". On a shared
// box where several agents lint different projects, that failed `forge lint`
// whenever anyone else happened to be linting. The scaffolded .golangci.yml
// sets `allow-serial-runners: true`, but only projects scaffolded or upgraded
// after that line landed have it; control-plane did not, and its lint lane
// failed on contention (reported by the FREE-DAEMON run as F2).
//
// The stub below is golangci-lint with the lock HELD by someone else: it
// exits 3 exactly as the real binary does unless the invocation asked to wait
// for the lock. Every lane that shells golangci-lint is driven through it.

package lint

import (
	"context"
	"strings"
	"testing"
)

// contendedUnlessSerialStub is golangci-lint while another instance holds the
// machine-global lock. With --allow-serial-runners the real binary blocks
// until the lock frees and then runs; the stub models the run that follows.
const contendedUnlessSerialStub = `for a in "$@"; do
  if [ "$a" = "--allow-serial-runners" ]; then echo "0 issues."; exit 0; fi
done
echo "Error: parallel golangci-lint is running" >&2
echo "The command is terminated due to an error: parallel golangci-lint is running" >&2
exit 3`

func TestGolangciLanesQueueForTheMachineLock(t *testing.T) {
	t.Run("text gate", func(t *testing.T) {
		fakeGolangciLint(t, contendedUnlessSerialStub)
		if err := runGolangciLint(context.Background(), false, []string{"./..."}); err != nil {
			t.Fatalf("golangci-lint gate failed on a lock another process held: %v — forge lint must "+
				"pass --allow-serial-runners so a contended lock waits instead of exiting 3", err)
		}
	})
	t.Run("text gate with --fix", func(t *testing.T) {
		fakeGolangciLint(t, contendedUnlessSerialStub)
		if err := runGolangciLint(context.Background(), true, []string{"./..."}); err != nil {
			t.Fatalf("golangci-lint --fix failed on a contended lock: %v", err)
		}
	})
	t.Run("text advisory guardrail", func(t *testing.T) {
		fakeGolangciLint(t, contendedUnlessSerialStub)
		if err := runTypedAccessGuardAdvisory(context.Background(), []string{"./..."}); err != nil {
			t.Fatalf("typed-config guardrail could not run on a contended lock: %v", err)
		}
	})
	t.Run("json gate", func(t *testing.T) {
		fakeGolangciLint(t, contendedUnlessSerialStub)
		fs, gated := collectGolangciLintJSON(context.Background(), []string{"./..."})
		if gated || len(fs) != 0 {
			t.Fatalf("--json golangci-lint gated on a contended lock: gated=%v findings=%+v", gated, fs)
		}
	})
	t.Run("json advisory guardrail", func(t *testing.T) {
		fakeGolangciLint(t, contendedUnlessSerialStub)
		// Strict, so an unavailable lane would gate. The advisory lane
		// surfaces every output line as a warning, so the stub's own
		// "0 issues." arrives as one; what must be absent is the
		// could-not-run rule.
		fs, gated, err := collectTypedAccessGuardJSON(testRunCtx(t, true))
		if err != nil || gated {
			t.Fatalf("--json guardrail gated on a contended lock: err=%v gated=%v findings=%+v", err, gated, fs)
		}
		for _, f := range fs {
			if f.Rule == ruleTypedAccessGuardUnavailable {
				t.Fatalf("--json guardrail reported it could not run on a contended lock: %+v", f)
			}
		}
	})
}

// TestGolangciRunArgsNeverDropsTheLock pins the half of the choice the stub
// cannot see. --allow-parallel-runners would ALSO get past a contended lock,
// by not taking it at all — and two full golangci-lint runs at once can
// exhaust a small machine's memory, which is why the lock exists.
func TestGolangciRunArgsNeverDropsTheLock(t *testing.T) {
	args := golangciRunArgs([]string{"--fix"}, []string{"./internal/..."})
	got := strings.Join(args, " ")
	if args[0] != "run" {
		t.Errorf("argv must start with the run subcommand: %q", got)
	}
	if !strings.Contains(got, "--allow-serial-runners") {
		t.Errorf("argv does not wait for the lock: %q", got)
	}
	if strings.Contains(got, "--allow-parallel-runners") {
		t.Errorf("argv drops golangci-lint's lock entirely, so concurrent full runs can OOM the machine: %q", got)
	}
	if !strings.HasSuffix(got, "--fix ./internal/...") {
		t.Errorf("extra flags and paths must be passed through in order: %q", got)
	}
}
