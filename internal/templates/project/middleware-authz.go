//go:build ignore

package middleware

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
)

// GetUser extracts the authenticated user's claims from the context.
// Returns a connect CodeUnauthenticated error if no claims are present.
// Use this in handlers to get the current user before calling Authorizer.Can.
func GetUser(ctx context.Context) (*Claims, error) {
	claims, ok := ClaimsFromContext(ctx)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("no authenticated user"))
	}
	return claims, nil
}

// Standard CRUD action constants for use with Authorizer.Can.
const (
	ActionCreate = "create"
	ActionRead   = "read"
	ActionUpdate = "update"
	ActionDelete = "delete"
	ActionList   = "list"
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

	// Can checks if the user has permission to perform the action on the resource.
	// action is one of the Action* constants (create, read, update, delete, list).
	// resource is the entity name in lowercase (e.g., "patient", "invoice").
	// Return nil to allow, or a connect.Error (typically CodePermissionDenied) to deny.
	Can(ctx context.Context, claims *Claims, action string, resource string) error
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
