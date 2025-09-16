//go:build ignore

package middleware

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
)

// Authorizer determines whether the current request is authorized to access
// a given RPC procedure. Implementations should inspect the context for
// authentication claims and make access control decisions.
//
// Return nil to allow access, or a connect.Error to deny.
type Authorizer interface {
	// CanAccess checks whether the authenticated user in ctx is authorized
	// to call the given procedure. The procedure string is the full RPC
	// method name (e.g., "/proto.services.users.v1.UsersService/Create").
	CanAccess(ctx context.Context, procedure string) error
}

// AuthzInterceptor returns a Connect interceptor that checks authorization
// on every RPC call using the provided Authorizer.
func AuthzInterceptor(authz Authorizer) connect.Interceptor {
	return &authzInterceptor{authz: authz}
}

type authzInterceptor struct {
	authz Authorizer
}

func (a *authzInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.authz.CanAccess(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *authzInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // Client-side, no server authz needed
}

func (a *authzInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.authz.CanAccess(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// UnimplementedAuthorizer returns a PermissionDenied error for every procedure,
// indicating that authorization logic has not been implemented yet.
// This is the default authorizer generated for each service. You MUST replace
// the CanAccess implementation with real authorization logic before deploying.
type UnimplementedAuthorizer struct{}

// CanAccess always returns PermissionDenied. Override this method with real
// authorization logic before deploying.
func (UnimplementedAuthorizer) CanAccess(_ context.Context, procedure string) error {
	return connect.NewError(
		connect.CodePermissionDenied,
		fmt.Errorf("authorization not implemented for %s: implement CanAccess in your service's authorizer", procedure),
	)
}
