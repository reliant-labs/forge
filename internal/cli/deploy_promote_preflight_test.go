package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// errPreflightMissingImage is the refusal the 2026-10-07 prod release got.
var errPreflightMissingImage = errors.New("deploy preflight failed — the live target is missing dependencies the rendered manifests require:\n\n" +
	"  Images not found:\n    - temporalio/auto-setup:1.26.2\n\nNothing was applied")

// A deploy whose preflight refuses the release records NOTHING. On
// 2026-10-07 the preflight ran inside the apply, after the compare-and-set
// write: prod's binding moved to a release that never shipped, `env status`
// showed it as current, and the suggested re-run was refused as plan_stale
// because the failed run had moved the binding the plan was computed from.
func TestDeployRelease_PreflightRefusalRecordsNothing(t *testing.T) {
	prev := runPromoteClientDeploy
	runPromoteClientDeploy = func(context.Context, string, deployOptions) error { return errPreflightMissingImage }
	t.Cleanup(func() { runPromoteClientDeploy = prev })

	store, releases := selfManagedFixture()
	opts := promoteOptions{
		ProjectDir: t.TempDir(), Git: allCommitsPresent(), Follow: waitByDefault(),
		Ledger: envLedger{Bindings: store, Releases: releases},
	}
	opts.Run.None = true
	var err error
	captureStdout(t, func() { err = runPromote(context.Background(), "v2", "prod", opts) })
	if err == nil || !strings.Contains(err.Error(), "Images not found") {
		t.Fatalf("the preflight's refusal must fail the deploy with its report, got %v", err)
	}
	cur, _, _ := store.Current(context.Background(), "prod")
	if cur.Release != "v1" || cur.ID != "p-1" {
		t.Fatalf("a deploy the preflight refused moved the binding: prod is on %s (%s), want v1 (p-1)", cur.Release, cur.ID)
	}
}

// The pre-write preflight checks the TARGET release, in a mode that applies
// nothing, and it runs before the approval gate — so `--plan-only` shows the
// refusal instead of an approvable plan.
func TestDeployRelease_PreflightChecksTheTargetBeforeAnythingIsWritten(t *testing.T) {
	var pinned []string
	var seen []deployOptions
	prev := runPromoteClientDeploy
	runPromoteClientDeploy = func(ctx context.Context, _ string, o deployOptions) error {
		pinned = append(pinned, hostedPinReleaseFrom(ctx))
		seen = append(seen, o)
		return nil
	}
	t.Cleanup(func() { runPromoteClientDeploy = prev })

	store, releases := selfManagedFixture()
	opts := promoteOptions{
		ProjectDir: t.TempDir(), Git: allCommitsPresent(), Follow: waitByDefault(),
		Ledger: envLedger{Bindings: store, Releases: releases},
	}
	opts.Run.None = true
	var err error
	captureStdout(t, func() { err = runPromote(context.Background(), "v2", "prod", opts) })
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("client deploy ran %d time(s), want 2 (the preflight, then the apply)", len(seen))
	}
	if !seen[0].preflightOnly || !seen[0].dryRun {
		t.Errorf("the first run must be the read-only preflight, got preflightOnly=%v dryRun=%v", seen[0].preflightOnly, seen[0].dryRun)
	}
	if pinned[0] != "v2" {
		t.Errorf("the preflight rendered release %q, want the TARGET v2 (the binding still names v1)", pinned[0])
	}
	if seen[1].preflightOnly || seen[1].dryRun {
		t.Errorf("the second run must be the real apply, got preflightOnly=%v dryRun=%v", seen[1].preflightOnly, seen[1].dryRun)
	}
}

// --skip-preflight skips the pre-write preflight as well as the one in the
// apply; a hub-converged env has no client apply, so nothing to gate here.
func TestDeployRelease_PreflightNotRunWhenItHasNothingToGate(t *testing.T) {
	var apply capturedClientDeploy
	apply.install(t)
	store, releases := selfManagedFixture()
	opts := promoteOptions{
		ProjectDir: t.TempDir(), Git: allCommitsPresent(),
		Follow: &promoteFollowOptions{clientDeploy: deployOptions{skipPreflight: true}},
		Ledger: envLedger{Bindings: store, Releases: releases},
	}
	opts.Run.None = true
	var err error
	captureStdout(t, func() { err = runPromote(context.Background(), "v2", "prod", opts) })
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if len(apply.preflights) != 0 {
		t.Errorf("--skip-preflight still ran the pre-write preflight %d time(s)", len(apply.preflights))
	}
}
