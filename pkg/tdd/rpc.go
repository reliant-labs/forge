package tdd

import (
	"context"
	"testing"

	"connectrpc.com/connect"
)

// Case is a table-driven test row for a Connect RPC.
//
// Req is the request proto type and Resp is the response proto type;
// Forge handlers receive *connect.Request[Req] and return
// *connect.Response[Resp], which matches the signature the helper expects.
//
// Either WantErr or Check should be set — never both. WantErr asserts the
// handler returned a Connect error with the given code; Check is a
// caller-supplied function for asserting on the response (typically used
// for happy-path cases). If neither is set, the helper only verifies
// that the call did not return an error.
//
// There is deliberately no "tolerate any outcome" mode: every row must be
// able to fail. Scaffold rows for not-yet-implemented handlers assert
// WantErr: connect.CodeUnimplemented — such a row self-destructs (goes
// red) the moment the handler is implemented, forcing it to be rewritten
// with a real Check / WantErr assertion.
type Case[Req, Resp any] struct {
	// Name identifies the row; passed straight to t.Run.
	Name string

	// Req is the inbound *connect.Request used in the call.
	Req *connect.Request[Req]

	// WantErr, if non-zero, asserts the handler returned a connect.Error
	// with this code. The Check function is not consulted in this case.
	WantErr connect.Code

	// Check is invoked on a successful (err == nil) response so the test
	// can assert on the returned message. Optional.
	Check func(t *testing.T, resp *connect.Response[Resp])

	// Setup runs before the handler is invoked. Use it to seed mocks or
	// populate per-row state. Cleanup should be registered via t.Cleanup
	// inside the closure.
	Setup func(t *testing.T)

	// Ctx, if non-nil, overrides the default context.Background() passed
	// to the handler. Use [WithTimeout] for a deadlined context.
	Ctx context.Context
}

// HandlerFunc is the Connect RPC handler signature TableRPC drives.
type HandlerFunc[Req, Resp any] func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error)

// TableRPC runs a slice of [Case] rows against a Connect handler.
// Each row becomes a t.Run subtest. The helper:
//   - calls Setup if present,
//   - invokes the handler with the row's request,
//   - asserts on WantErr (if set) or runs Check (if the call succeeded).
//
// Subtests run with t.Parallel disabled by default so per-row Setup that
// touches shared state stays correct. Wrap the call site in a parallel
// outer test if you want concurrency; per-row parallelism is the caller's
// choice and not the library's default.
func TableRPC[Req, Resp any](
	t *testing.T,
	cases []Case[Req, Resp],
	handler HandlerFunc[Req, Resp],
) {
	t.Helper()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			if tc.Setup != nil {
				tc.Setup(t)
			}

			ctx := tc.Ctx
			if ctx == nil {
				ctx = context.Background()
			}

			got, err := handler(ctx, tc.Req)

			if tc.WantErr != 0 {
				AssertConnectError(t, err, tc.WantErr)
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.Check != nil {
				tc.Check(t, got)
			}
		})
	}
}

// RPCCase is a type alias for [Case] — used by the codegen-emitted
// `handlers_crud_test_gen.go` shim so the generated identifier matches
// the migration skill's documentation. Hand-written tests may use
// either name; they are the same type.
type RPCCase[Req, Resp any] = Case[Req, Resp]

// RunRPCCases is a function alias for [TableRPC] — see [RPCCase]. The
// codegen-emitted test shim uses RunRPCCases so the generated call
// site mirrors the migration skill's documentation. Hand-written
// tests may call either; they have identical behaviour.
func RunRPCCases[Req, Resp any](
	t *testing.T,
	cases []RPCCase[Req, Resp],
	handler HandlerFunc[Req, Resp],
) {
	t.Helper()
	TableRPC(t, cases, handler)
}

// AssertConnectError asserts that err is a non-nil Connect error with the
// given code. It is the canonical assertion helper used inside TableRPC
// and is exported for direct use in hand-written tests.
func AssertConnectError(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected connect error with code %s, got nil", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("expected connect code %s, got %s (err=%v)", want, got, err)
	}
}
