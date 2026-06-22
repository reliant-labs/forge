package observe_test

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/observe"
)

// markerInterceptor is a test collaborator that records, on a shared
// trace slice, that it ran — so a test can prove an explicit Auth/Audit/
// RateLimit collaborator was actually threaded into the chain (no global
// needed) and that ordering is correct.
type markerInterceptor struct {
	name  string
	trace *[]string
}

func (m markerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		*m.trace = append(*m.trace, m.name)
		return next(ctx, req)
	}
}

func (m markerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (m markerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// fakeRequest is the minimal connect.AnyRequest needed to drive a unary
// chain in-process without a real transport.
type fakeRequest struct{ connect.AnyRequest }

func (fakeRequest) Spec() connect.Spec  { return connect.Spec{Procedure: "/test.v1.Svc/M"} }
func (fakeRequest) Header() http.Header { return http.Header{} }

// run threads a request through the chain, innermost handler last, and
// returns the recorded interceptor execution order.
func runChain(t *testing.T, chain []connect.Interceptor, trace *[]string) {
	t.Helper()
	// Compose the chain the way connect does: the first interceptor is
	// outermost. Build a terminal handler, then wrap from the inside out.
	var handler connect.UnaryFunc = func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		*trace = append(*trace, "handler")
		return nil, nil
	}
	for i := len(chain) - 1; i >= 0; i-- {
		handler = chain[i].WrapUnary(handler)
	}
	_, _ = handler(context.Background(), fakeRequest{})
}

func TestChain_ThreadsExplicitCollaborators(t *testing.T) {
	var trace []string
	auth := markerInterceptor{name: "auth", trace: &trace}
	audit := markerInterceptor{name: "audit", trace: &trace}
	rl := markerInterceptor{name: "ratelimit", trace: &trace}
	extra := markerInterceptor{name: "extra", trace: &trace}

	chain := observe.Chain(observe.Deps{
		// Logger/Tracer/Meter nil → observability layers degrade to
		// pass-through / slog.Default; they don't append to the trace.
		Auth:      auth,
		Audit:     audit,
		RateLimit: rl,
		Extras:    []connect.Interceptor{extra},
	})

	runChain(t, chain, &trace)

	// The application collaborators must all have run (no global needed —
	// they were passed in directly), and in the documented order:
	// auth → audit → ratelimit → extra → handler.
	want := []string{"auth", "audit", "ratelimit", "extra", "handler"}
	if len(trace) != len(want) {
		t.Fatalf("trace = %v, want %v", trace, want)
	}
	for i := range want {
		if trace[i] != want[i] {
			t.Fatalf("trace = %v, want %v", trace, want)
		}
	}
}

func TestChain_NilCollaboratorsSkipped(t *testing.T) {
	// All application collaborators nil → only the observability layer
	// (no-op with nil deps) + handler. Nothing appended by markers.
	var trace []string
	chain := observe.Chain(observe.Deps{})
	runChain(t, chain, &trace)
	// Only the terminal handler records.
	if len(trace) != 1 || trace[0] != "handler" {
		t.Fatalf("nil collaborators: trace = %v, want [handler]", trace)
	}
	// Chain still has the fixed observability prefix (recovery, request-id,
	// logging, tracing, metrics) = 5 interceptors, none of which is a
	// marker.
	if len(chain) != 5 {
		t.Errorf("nil-collaborator chain length = %d, want 5 (observability only)", len(chain))
	}
}

func TestChain_AuditOnlyThreaded(t *testing.T) {
	// Prove a SINGLE explicit collaborator threads in without the others —
	// the core "no global" assertion for audit specifically.
	var trace []string
	audit := markerInterceptor{name: "audit", trace: &trace}
	chain := observe.Chain(observe.Deps{Audit: audit})
	runChain(t, chain, &trace)
	if len(trace) != 2 || trace[0] != "audit" || trace[1] != "handler" {
		t.Fatalf("audit-only: trace = %v, want [audit handler]", trace)
	}
}
