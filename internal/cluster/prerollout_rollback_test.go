package cluster

import (
	"context"
	"strings"
	"testing"
)

// TestApply_RollbackSkipsPreRolloutJobs is the control-plane v1.7.0 → v1.6.0
// rollback, at the apply.
//
// A rollback deploys the OLDER release's images. Its migrate Job runs the
// older binary's `db migrate up` against a database the newer release already
// migrated past it, and that binary predates any schema-ahead handling:
// golang-migrate fails with "no migration found for version 92". Held as a
// pre-rollout gate, that failure aborts the deploy with NO workload applied —
// the rollback is impossible exactly when it is needed. The fake kubectl here
// reports the Job as FAILED to model it.
//
// On a rollback the Job must therefore not be applied at all, the workloads
// must go out, and the skip must be reported by name rather than happen
// silently.
func TestApply_RollbackSkipsPreRolloutJobs(t *testing.T) {
	calls := fakeGateKubectl(t, jobFails, "admin-server")
	var skipped []string
	opts := gateOpts(RolloutWait)
	opts.PromotionRollback = true
	opts.OnSkippedJobs = func(jobs []string) { skipped = append(skipped, jobs...) }

	if err := applyRendered(context.Background(), opts, gateStream); err != nil {
		t.Fatalf("a rollback deploy stopped at the pre-rollout gate: %v\ncalls:\n%s", err, describeCalls(calls()))
	}
	got := calls()
	if at := firstIndex(got, appliesKind("Job")); at >= 0 {
		t.Errorf("a rollback applied the older release's pre-rollout Job (call %d) — it can only no-op or fail\ncalls:\n%s", at, describeCalls(got))
	}
	if at := firstIndex(got, waitsForJobComplete("app-migrate-abc123")); at >= 0 {
		t.Errorf("a rollback waited on a Job it did not apply (call %d)", at)
	}
	for _, kind := range gatedKinds {
		if firstIndex(got, appliesKind(kind)) < 0 {
			t.Errorf("a rollback never applied the %s — the rollback did not happen\ncalls:\n%s", kind, describeCalls(got))
		}
	}
	// The Job's identity is still ordinary support and still lands.
	if firstIndex(got, appliesKind("ServiceAccount")) < 0 {
		t.Errorf("a rollback dropped the support objects along with the Job\ncalls:\n%s", describeCalls(got))
	}
	if len(skipped) != 1 || skipped[0] != "app-migrate-abc123" {
		t.Errorf("skipped Jobs reported = %v; want [app-migrate-abc123] — a skip nobody can see is the silent failure this replaces", skipped)
	}
}

// TestApply_RollbackStillRunsPostRolloutJobs: the skip is scoped to the
// PRE-rollout phase — the schema step. A post-rollout Job (an IdP provisioner)
// configures things for the workloads being deployed, older release or not,
// so a rollback keeps it.
func TestApply_RollbackStillRunsPostRolloutJobs(t *testing.T) {
	calls := fakeGateKubectl(t, jobCompletes, "admin-server")
	provision := strings.Replace(migrateJob, "  name: app-migrate-abc123\n",
		"  name: app-provision-def456\n  annotations:\n    "+DeployPhaseAnnotation+": "+DeployPhasePostRollout+"\n", 1)
	opts := gateOpts(RolloutWait)
	opts.PromotionRollback = true

	if err := applyRendered(context.Background(), opts, strings.Join([]string{gateStream, provision}, docDelimiter)); err != nil {
		t.Fatalf("applyRendered: %v", err)
	}
	got := calls()
	if firstIndex(got, waitsForJobComplete("app-provision-def456")) < 0 {
		t.Errorf("a rollback dropped a post-rollout Job\ncalls:\n%s", describeCalls(got))
	}
	if firstIndex(got, waitsForJobComplete("app-migrate-abc123")) >= 0 {
		t.Errorf("a rollback ran the pre-rollout migrate Job\ncalls:\n%s", describeCalls(got))
	}
}
