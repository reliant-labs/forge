package operatorkit

import "testing"

// TestResolveLeaderElectionID asserts the LEADER_ELECTION_ID env override
// wins over the generated Options.LeaderElectionID default (env > opts),
// so two processes that both run the manager can take distinct leases. An
// unset/empty env preserves the generated default unchanged.
func TestResolveLeaderElectionID(t *testing.T) {
	tests := []struct {
		name   string
		optsID string
		envID  string // empty is treated identically to unset
		want   string
	}{
		{
			name:   "empty env keeps opts default",
			optsID: "example.com/myproj-leader",
			envID:  "",
			want:   "example.com/myproj-leader",
		},
		{
			name:   "env overrides opts default",
			optsID: "example.com/myproj-leader",
			envID:  "example.com/myproj-admin-leader",
			want:   "example.com/myproj-admin-leader",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("LEADER_ELECTION_ID", tc.envID)
			if got := resolveLeaderElectionID(tc.optsID); got != tc.want {
				t.Fatalf("resolveLeaderElectionID(%q) = %q, want %q", tc.optsID, got, tc.want)
			}
		})
	}
}
