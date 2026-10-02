package cli

import (
	"errors"
	"strings"
	"testing"
)

// TestUnpublishedHostedRefusal pins the guard that replaces a 15-minute silent
// wait with one sentence.
//
// The case that matters is the FIRST row: a hosted env whose tiers were never
// published answers GetRollout with `status=ok` and an empty workload list,
// because an env with no deployments has no unhealthy workload to report. That
// is indistinguishable from "healthy" to a phase check, which is why the guard
// keys on the workload list instead.
//
// MUTATION VERIFIED RED: drop the `len(rollout.Workloads) > 0` guard and the
// "published backend" / "unpinned database only" rows start refusing a
// perfectly good deploy — which is the failure mode worth guarding against,
// since a false refusal here would block every legitimate release.
func TestUnpublishedHostedRefusal(t *testing.T) {
	published := []wireWorkloadRollout{{DeploymentID: "dep-api", Name: "api"}}
	database := []wireWorkloadRollout{{DeploymentID: "dep-db", Name: "db"}}

	for _, tc := range []struct {
		name    string
		rollout wireRollout
		refuse  bool
	}{
		{
			// The defect: nothing published, so nothing to converge.
			name:    "no workloads at all",
			rollout: wireRollout{Phase: wireRolloutPhaseUnspecified},
			refuse:  true,
		},
		{
			// A phase that READS fine must not rescue an empty rollout: the
			// server has no workload to call unhealthy.
			name:    "succeeded phase but still no workloads",
			rollout: wireRollout{Phase: "DEPLOY_ROLLOUT_PHASE_SUCCEEDED"},
			refuse:  true,
		},
		{
			name:    "published backend",
			rollout: wireRollout{Phase: "DEPLOY_ROLLOUT_PHASE_PROGRESSING", Workloads: published},
			refuse:  false,
		},
		{
			// An env whose only tier is a database IS published. The database
			// cannot carry a release artifact, so it is unpinned — but its
			// presence proves the declaration deploy ran.
			name:    "unpinned database only",
			rollout: wireRollout{Phase: wireRolloutPhaseUnspecified, Unpinned: database},
			refuse:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := unpublishedHostedRefusal("staging", tc.rollout)
			if !tc.refuse {
				if err != nil {
					t.Fatalf("refused a published env: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("no refusal: the deploy would wait out its whole budget for a rollout that cannot start")
			}
			// Exit 2 — we could not determine health — not 1. The release is
			// not bad; it was never applied, and reporting it as a failed
			// release would be a lie a pipeline acts on.
			var coded *exitCodeError
			if !errors.As(err, &coded) || coded.code != exitUndetermined {
				t.Errorf("exit code = %v, want exitUndetermined (%d)", err, exitUndetermined)
			}
			// The message must carry the remedy and say the promotion
			// survived; without both, the operator's next move is a guess.
			for _, want := range []string{"forge env deploy staging", "promotion IS recorded"} {
				if !strings.Contains(coded.msg, want) {
					t.Errorf("refusal does not mention %q:\n%s", want, coded.msg)
				}
			}
		})
	}
}
