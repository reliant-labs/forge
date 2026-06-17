package cli

import "testing"

// TestOneShotJobNamesFromKCL_IncludesEmptyScheduleCronJob is the
// CLI-side half of the GAP 2 regression: a forge.CronJob with
// schedule=="" (the migrate pattern) renders as a run-to-completion Job
// and MUST land in the OneShotJobs wait set so `forge deploy` blocks on
// it before rolling the dependent workloads. A CronJob with a non-empty
// schedule renders as a recurring k8s CronJob and MUST NOT be waited on.
func TestOneShotJobNamesFromKCL_IncludesEmptyScheduleCronJob(t *testing.T) {
	e := &KCLEntities{
		CronJobs: []CronJobEntity{
			{Name: "cp-forge-migrate", Schedule: ""},        // one-shot Job — wait
			{Name: "nightly-report", Schedule: "0 0 * * *"}, // scheduled — skip
		},
	}

	got := oneShotJobNamesFromKCL(e)

	if len(got) != 1 || got[0] != "cp-forge-migrate" {
		t.Fatalf("expected [cp-forge-migrate] in one-shot wait set, got %v", got)
	}
}

// TestOneShotJobNamesFromKCL_NilSafe confirms the nil-entities case
// (KCL render failed upstream) returns no wait set rather than panicking.
func TestOneShotJobNamesFromKCL_NilSafe(t *testing.T) {
	if got := oneShotJobNamesFromKCL(nil); got != nil {
		t.Errorf("expected nil for nil entities, got %v", got)
	}
}
