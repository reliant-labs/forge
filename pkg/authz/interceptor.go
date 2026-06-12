package authz

import (
	"context"

	"connectrpc.com/connect"
)

// AccessChecker is the per-procedure gate [Interceptor] consults on
// every RPC. The project's pkg/middleware.Authorizer interface (Can /
// CanAccess) satisfies it, as does [*Authorizer].
//
// Return nil to allow the call, or a *connect.Error (typically
// CodePermissionDenied / CodeUnauthenticated) to deny it.
type AccessChecker interface {
	// CanAccess checks whether the request in ctx may call the given
	// procedure (the full RPC method name, e.g.
	// "/proto.services.users.v1.UsersService/Create").
	CanAccess(ctx context.Context, procedure string) error
}

// Interceptor returns a Connect interceptor that checks authorization
// on every unary and streaming-handler RPC via checker.CanAccess. The
// project's scaffolded middleware.AuthzInterceptor is a one-line shim
// over this function.
//
// checker must be non-nil; passing nil would nil-panic on every request
// so the constructor panics at boot instead.
func Interceptor(checker AccessChecker) connect.Interceptor {
	if checker == nil {
		panic("authz.Interceptor: checker must not be nil; use authz.New(authz.DenyAll{}) for a fail-closed stub")
	}
	return &accessInterceptor{checker: checker}
}

type accessInterceptor struct {
	checker AccessChecker
}

func (a *accessInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.checker.CanAccess(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *accessInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // client-side: no server authz to enforce
}

func (a *accessInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.checker.CanAccess(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
