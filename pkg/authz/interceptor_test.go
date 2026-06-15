package authz

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
)

type stubChecker struct {
	err  error
	seen []string
}

func (s *stubChecker) CanAccess(_ context.Context, procedure string) error {
	s.seen = append(s.seen, procedure)
	return s.err
}

func TestInterceptor_NilCheckerPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("Interceptor(nil) must panic at construction")
		}
	}()
	Interceptor(nil)
}

// Deny path: WrapUnary must not invoke next and must surface the
// checker's error verbatim. Allow path: next runs and its response
// flows back. connect.NewRequest gives a real AnyRequest (procedure ""
// — fine, the stub checker records whatever it gets).
func TestInterceptor_UnaryGate(t *testing.T) {
	t.Parallel()

	t.Run("deny short-circuits", func(t *testing.T) {
		t.Parallel()
		denied := connect.NewError(connect.CodePermissionDenied, errors.New("nope"))
		ic := Interceptor(&stubChecker{err: denied})
		called := false
		wrapped := ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return nil, nil
		})
		_, err := wrapped(context.Background(), connect.NewRequest(&struct{}{}))
		if !errors.Is(err, denied) {
			t.Fatalf("want the checker's error verbatim, got %v", err)
		}
		if called {
			t.Fatal("next must not run when access is denied")
		}
	})

	t.Run("allow invokes next", func(t *testing.T) {
		t.Parallel()
		chk := &stubChecker{}
		ic := Interceptor(chk)
		called := false
		wrapped := ic.WrapUnary(func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return nil, nil
		})
		if _, err := wrapped(context.Background(), connect.NewRequest(&struct{}{})); err != nil {
			t.Fatalf("allow path must not error: %v", err)
		}
		if !called {
			t.Fatal("next must run when access is allowed")
		}
		if len(chk.seen) != 1 {
			t.Fatalf("checker must be consulted exactly once, got %d", len(chk.seen))
		}
	})
}
