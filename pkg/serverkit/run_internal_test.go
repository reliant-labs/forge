package serverkit

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
)

// opStub is a minimal Operator for shouldRunOperators gating tests.
type opStub struct{ name string }

func (o opStub) Name() string { return o.name }

// appStub is a minimal Application carrying only the operator list
// shouldRunOperators inspects.
type appStub struct{ operators []Operator }

func (a appStub) WorkerList() []Worker     { return nil }
func (a appStub) OperatorList() []Operator { return a.operators }
func (a appStub) HasOperators() bool       { return len(a.operators) > 0 }
func (a appStub) RunOperators(context.Context, *slog.Logger, string) error {
	return nil
}
func (a appStub) RESTHandler() http.Handler      { return nil }
func (a appStub) Shutdown(context.Context) error { return nil }

// TestShouldRunOperators covers the filter gating AND the RUN_OPERATORS
// opt-out. The opt-out must win over the empty-filter default so a
// catch-all API server can run no controller manager (and need no
// operator RBAC) while a separate `server <operator>` process runs it.
func TestShouldRunOperators(t *testing.T) {
	app := appStub{operators: []Operator{opStub{name: "scaler"}}}

	tests := []struct {
		name        string
		nameSet     map[string]bool
		runOperator string // RUN_OPERATORS env; "" means leave unset
		setEnv      bool
		want        bool
	}{
		{
			name:    "empty filter starts operators (legacy default)",
			nameSet: nil,
			want:    true,
		},
		{
			name:    "filter including an operator starts operators",
			nameSet: map[string]bool{"scaler": true},
			want:    true,
		},
		{
			name:    "filter excluding all operators does not start them",
			nameSet: map[string]bool{"api": true},
			want:    false,
		},
		{
			name:        "RUN_OPERATORS=false wins over empty-filter default",
			nameSet:     nil,
			runOperator: "false",
			setEnv:      true,
			want:        false,
		},
		{
			name:        "RUN_OPERATORS=false wins even when filter names an operator",
			nameSet:     map[string]bool{"scaler": true},
			runOperator: "false",
			setEnv:      true,
			want:        false,
		},
		{
			name:        "RUN_OPERATORS=0 disables operators",
			nameSet:     nil,
			runOperator: "0",
			setEnv:      true,
			want:        false,
		},
		{
			name:        "RUN_OPERATORS=true keeps default-on behaviour",
			nameSet:     nil,
			runOperator: "true",
			setEnv:      true,
			want:        true,
		},
		{
			name:        "RUN_OPERATORS empty keeps default-on behaviour",
			nameSet:     nil,
			runOperator: "",
			setEnv:      true,
			want:        true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv("RUN_OPERATORS", tc.runOperator)
			} else {
				// Ensure no ambient value leaks in; empty == unset for the
				// opt-out logic.
				t.Setenv("RUN_OPERATORS", "")
			}
			if got := shouldRunOperators(app, tc.nameSet); got != tc.want {
				t.Fatalf("shouldRunOperators() = %v, want %v", got, tc.want)
			}
		})
	}
}
