package observe

import (
	"log/slog"
	"testing"

	"connectrpc.com/connect"
)

func TestDefaultMiddlewares_CanonicalOrder(t *testing.T) {
	chain := DefaultMiddlewares(DefaultMiddlewareDeps{})
	// Order matters; this test pins the canonical sequence so future
	// reorderings show up as a deliberate test edit. See the package
	// docstring for the rationale.
	if len(chain) != 5 {
		t.Fatalf("expected 5 interceptors, got %d", len(chain))
	}
	if _, ok := chain[0].(*recoveryInterceptor); !ok {
		t.Errorf("position 0 = %T, want *recoveryInterceptor", chain[0])
	}
	if _, ok := chain[1].(*requestIDInterceptor); !ok {
		t.Errorf("position 1 = %T, want *requestIDInterceptor", chain[1])
	}
	if _, ok := chain[2].(*loggingInterceptor); !ok {
		t.Errorf("position 2 = %T, want *loggingInterceptor", chain[2])
	}
	// positions 3 and 4 are tracer/meter interceptors but they degrade
	// to noopInterceptor when those deps are nil — assert noop here.
	if _, ok := chain[3].(*noopInterceptor); !ok {
		t.Errorf("position 3 (no tracer) = %T, want *noopInterceptor", chain[3])
	}
	if _, ok := chain[4].(*noopInterceptor); !ok {
		t.Errorf("position 4 (no meter) = %T, want *noopInterceptor", chain[4])
	}
}

func TestDefaultMiddlewares_AppendsExtras(t *testing.T) {
	stub := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc { return next })
	chain := DefaultMiddlewares(DefaultMiddlewareDeps{
		Logger: slog.Default(),
		Extras: []connect.Interceptor{stub, stub},
	})
	if len(chain) != 7 {
		t.Fatalf("expected 5 canonical + 2 extras = 7, got %d", len(chain))
	}
	// Extras come last.
	if _, ok := chain[5].(connect.UnaryInterceptorFunc); !ok {
		t.Errorf("extra position 5 = %T, want connect.UnaryInterceptorFunc", chain[5])
	}
}

func TestDefaultMiddlewares_NilDepsDontPanic(t *testing.T) {
	chain := DefaultMiddlewares(DefaultMiddlewareDeps{})
	if len(chain) == 0 {
		t.Fatal("empty chain")
	}
}
