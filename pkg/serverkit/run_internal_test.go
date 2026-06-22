package serverkit

import (
	"testing"
)

// TestShouldRunOperators covers the RUN_OPERATORS opt-out — the only
// gate serverkit still owns now that service selection moved above it.
// The opt-out lets a catch-all API-server process run no controller
// manager (and need no operator RBAC) while a separate operator process
// runs it.
func TestShouldRunOperators(t *testing.T) {
	tests := []struct {
		name        string
		runOperator string // RUN_OPERATORS env; "" means leave unset
		want        bool
	}{
		{
			name:        "unset keeps default-on behaviour",
			runOperator: "",
			want:        true,
		},
		{
			name:        "RUN_OPERATORS=true keeps default-on behaviour",
			runOperator: "true",
			want:        true,
		},
		{
			name:        "RUN_OPERATORS=false disables operators",
			runOperator: "false",
			want:        false,
		},
		{
			name:        "RUN_OPERATORS=0 disables operators",
			runOperator: "0",
			want:        false,
		},
		{
			name:        "RUN_OPERATORS=off disables operators",
			runOperator: "off",
			want:        false,
		},
		{
			name:        "RUN_OPERATORS=no disables operators",
			runOperator: "no",
			want:        false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUN_OPERATORS", tc.runOperator)
			if got := shouldRunOperators(); got != tc.want {
				t.Fatalf("shouldRunOperators() = %v, want %v", got, tc.want)
			}
		})
	}
}
