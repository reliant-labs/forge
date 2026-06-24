package operatorkit

import (
	"testing"
	"time"
)

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

// TestEnvDuration asserts the leader-election timing override: a valid value
// wins, and an unset/blank/garbage/non-positive value falls back to the
// hardened default rather than zeroing the timing.
func TestEnvDuration(t *testing.T) {
	const def = 45 * time.Second
	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"unset keeps default", "", def},
		{"valid overrides", "90s", 90 * time.Second},
		{"garbage keeps default", "not-a-duration", def},
		{"zero keeps default", "0s", def},
		{"negative keeps default", "-5s", def},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPERATOR_LEASE_DURATION", tc.val)
			if got := envDuration("OPERATOR_LEASE_DURATION", def); got != tc.want {
				t.Fatalf("envDuration(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestEnvFloat32 asserts the client-QPS override falls back to the default on
// unset/garbage/non-positive input.
func TestEnvFloat32(t *testing.T) {
	const def float32 = 50
	tests := []struct {
		name string
		val  string
		want float32
	}{
		{"unset keeps default", "", def},
		{"valid overrides", "75", 75},
		{"garbage keeps default", "nope", def},
		{"zero keeps default", "0", def},
		{"negative keeps default", "-1", def},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPERATOR_CLIENT_QPS", tc.val)
			if got := envFloat32("OPERATOR_CLIENT_QPS", def); got != tc.want {
				t.Fatalf("envFloat32(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

// TestEnvInt asserts the client-Burst override falls back to the default on
// unset/garbage/non-positive input.
func TestEnvInt(t *testing.T) {
	const def = 100
	tests := []struct {
		name string
		val  string
		want int
	}{
		{"unset keeps default", "", def},
		{"valid overrides", "200", 200},
		{"garbage keeps default", "x", def},
		{"zero keeps default", "0", def},
		{"negative keeps default", "-1", def},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OPERATOR_CLIENT_BURST", tc.val)
			if got := envInt("OPERATOR_CLIENT_BURST", def); got != tc.want {
				t.Fatalf("envInt(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
