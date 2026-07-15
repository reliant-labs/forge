package operatorkit

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// TestCacheByObject asserts the per-object namespace-scoping conversion:
// scoped types get a cache.ByObject row with exactly the usable namespaces;
// unscopable entries (empty list, empty-string namespaces) are dropped so the
// type keeps the cluster-wide default; and a fully-empty input returns nil so
// the manager receives a zero-value cache.Options (the legacy shape).
func TestCacheByObject(t *testing.T) {
	pod := &corev1.Pod{}
	cm := &corev1.ConfigMap{}

	tests := []struct {
		name   string
		scopes map[client.Object][]string
		// wantNamespaces maps each object expected in the result to the
		// namespace set its ByObject row must carry. Objects absent from this
		// map must be absent from the result.
		wantNamespaces map[client.Object][]string
		wantNil        bool
	}{
		{
			name:    "nil input returns nil",
			scopes:  nil,
			wantNil: true,
		},
		{
			name:    "empty input returns nil",
			scopes:  map[client.Object][]string{},
			wantNil: true,
		},
		{
			name:    "entry with no namespaces is dropped",
			scopes:  map[client.Object][]string{pod: {}},
			wantNil: true,
		},
		{
			name:    "entry with only empty-string namespaces is dropped",
			scopes:  map[client.Object][]string{pod: {"", ""}},
			wantNil: true,
		},
		{
			name:           "scoped entry carries its namespaces",
			scopes:         map[client.Object][]string{pod: {"stack-dev"}},
			wantNamespaces: map[client.Object][]string{pod: {"stack-dev"}},
		},
		{
			name: "mixed: scoped entry kept, unscopable sibling dropped, empty strings filtered",
			scopes: map[client.Object][]string{
				pod: {"", "stack-dev", "stack-dev-wt1"},
				cm:  {""},
			},
			wantNamespaces: map[client.Object][]string{pod: {"stack-dev", "stack-dev-wt1"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cacheByObject(tc.scopes)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("cacheByObject() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tc.wantNamespaces) {
				t.Fatalf("cacheByObject() has %d entries, want %d: %v", len(got), len(tc.wantNamespaces), got)
			}
			for obj, wantNS := range tc.wantNamespaces {
				row, ok := got[obj]
				if !ok {
					t.Fatalf("cacheByObject() missing entry for %T", obj)
				}
				if len(row.Namespaces) != len(wantNS) {
					t.Fatalf("entry for %T has namespaces %v, want %v", obj, row.Namespaces, wantNS)
				}
				for _, ns := range wantNS {
					if _, ok := row.Namespaces[ns]; !ok {
						t.Errorf("entry for %T missing namespace %q (got %v)", obj, ns, row.Namespaces)
					}
				}
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
