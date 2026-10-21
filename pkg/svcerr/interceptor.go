package svcerr

import (
	"context"

	"connectrpc.com/connect"
)

// Interceptor returns the Connect interceptor that applies [Wrap] to every
// error a handler returns, so `return nil, svcerr.PermissionDenied("…")`
// reaches the client as CodePermissionDenied. connect-go itself turns any
// error that is not already a *connect.Error into CodeUnknown, and the
// svcerr constructors build sentinel errors, not Connect ones.
//
// Wrap is idempotent: a handler that already wrapped its error (or built a
// *connect.Error itself) is passed through unchanged, so writing
// `svcerr.Wrap` in a handler stays correct. An error nothing recognises
// becomes CodeInternal with [InternalMessage], never its raw text.
//
// Install it innermost, next to the handler (serverkit.NewChain does), so
// every interceptor outside it — audit, logging, metrics — sees the wire
// error rather than the sentinel.
func Interceptor() connect.Interceptor { return interceptor{} }

type interceptor struct{}

func (interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		resp, err := next(ctx, req)
		if err != nil {
			return resp, Wrap(err)
		}
		return resp, nil
	}
}

func (interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return Wrap(next(ctx, conn))
	}
}
