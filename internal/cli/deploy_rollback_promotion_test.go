package cli

import (
	"context"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

// TestDeployPromotionIsRollback is how `forge env deploy` learns it is
// shipping a rollback: from the env's CURRENT ledger entry, the one `forge env
// promote --rollback` wrote. It is the switch that makes the deploy skip the
// older release's pre-rollout Jobs (cluster.ApplyOpts.PromotionRollback), so
// both directions matter:
//
//   - v1.6.0 → v1.7.0 → rollback to v1.6.0: the current entry is a rollback,
//     so the deploy must skip — otherwise v1.6.0's migrator fails against
//     v1.7.0's schema and the gate aborts the rollback.
//   - a later forward promote to v1.7.1: the current entry is a promote, so
//     the migration runs again. A rollback that stuck would silently stop
//     every future release from migrating.
func TestDeployPromotionIsRollback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store := newFileBindingStore(dir)
	appendEntry := func(version string, kind release.PromotionKind) {
		t.Helper()
		if _, err := store.Append(ctx, release.Promotion{
			Env: "prod", Release: version, Kind: kind,
			Resolved: map[string]string{"control-plane": sha(version[len(version)-1:])},
		}); err != nil {
			t.Fatalf("append %s %s: %v", kind, version, err)
		}
	}

	if got, err := deployPromotionIsRollback(ctx, store, "prod", false); err != nil || got {
		t.Errorf("unbound env: rollback = %v, %v; want false — an env with no ledger is not rolling back", got, err)
	}
	appendEntry("v1.6.0", release.KindPromote)
	appendEntry("v1.7.0", release.KindPromote)
	if got, _ := deployPromotionIsRollback(ctx, store, "prod", false); got {
		t.Error("forward promote: rollback = true; the migration must run on a forward deploy")
	}
	appendEntry("v1.6.0", release.KindRollback)
	if got, err := deployPromotionIsRollback(ctx, store, "prod", false); err != nil || !got {
		t.Errorf("after `promote v1.6.0 --rollback`: rollback = %v, %v; want true — the deploy must skip v1.6.0's migrate Job", got, err)
	}
	// --no-digest deploys the mutable tag, not the ledger's release, so the
	// ledger's kind says nothing about what is being shipped.
	if got, _ := deployPromotionIsRollback(ctx, store, "prod", true); got {
		t.Error("--no-digest: rollback = true; a deploy that ignores the ledger must not take its kind")
	}
	appendEntry("v1.7.1", release.KindPromote)
	if got, _ := deployPromotionIsRollback(ctx, store, "prod", false); got {
		t.Error("forward promote after a rollback: rollback = true; a rollback must not stop every later release from migrating")
	}
}
