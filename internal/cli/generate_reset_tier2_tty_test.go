package cli

import (
	"strings"
	"testing"
)

// TestResetTier2WouldHangWithoutTTY pins the pure decision behind the
// non-TTY guard: forge must refuse to start a `--reset-tier2` run that
// would block on the per-file overwrite prompt with no terminal to answer
// it — and only that case.
func TestResetTier2WouldHangWithoutTTY(t *testing.T) {
	cases := []struct {
		name       string
		resetTier2 bool
		assumeYes  bool
		stdinIsTTY bool
		wantHang   bool
	}{
		{"reset, no yes, no tty -> hang", true, false, false, true},
		{"reset, no yes, tty -> ok (human answers)", true, false, true, false},
		{"reset, yes, no tty -> ok (no prompt)", true, true, false, false},
		{"reset, yes, tty -> ok", true, true, true, false},
		{"no reset, no tty -> ok (no prompt path)", false, false, false, false},
		{"no reset, yes, no tty -> ok", false, true, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resetTier2WouldHangWithoutTTY(tc.resetTier2, tc.assumeYes, tc.stdinIsTTY)
			if got != tc.wantHang {
				t.Errorf("resetTier2WouldHangWithoutTTY(%v,%v,%v) = %v, want %v",
					tc.resetTier2, tc.assumeYes, tc.stdinIsTTY, got, tc.wantHang)
			}
		})
	}
}

// TestGuardResetTier2NeedsTTY exercises the guard end to end under the
// test process's real (non-TTY) stdin: --reset-tier2 without --yes must
// fail fast with an actionable error naming the flag that avoids the
// prompt, and never return nil.
func TestGuardResetTier2NeedsTTY(t *testing.T) {
	// go test stdin is not a terminal, so this is the agent/CI path.
	err := guardResetTier2NeedsTTY(pipelineFlags{ResetTier2: true})
	if err == nil {
		t.Fatal("expected fail-fast error for --reset-tier2 without --yes and no TTY")
	}
	msg := err.Error()
	for _, want := range []string{"--reset-tier2", "--yes", "without a TTY"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message missing %q; got: %s", want, msg)
		}
	}

	// --yes short-circuits the prompt: no error even without a TTY.
	if err := guardResetTier2NeedsTTY(pipelineFlags{ResetTier2: true, AssumeYes: true}); err != nil {
		t.Errorf("--reset-tier2 --yes must not fail-fast, got: %v", err)
	}
	// No --reset-tier2 means no prompt path at all.
	if err := guardResetTier2NeedsTTY(pipelineFlags{}); err != nil {
		t.Errorf("plain generate must not fail-fast, got: %v", err)
	}
}
