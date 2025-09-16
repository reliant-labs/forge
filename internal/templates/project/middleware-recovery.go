//go:build ignore

package middleware

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"

	"connectrpc.com/connect"
)

// recoveryInterceptor implements connect.Interceptor with panic recovery
// for both unary and streaming RPCs.
type recoveryInterceptor struct {
	logger *slog.Logger
}

// RecoveryInterceptor creates a Connect RPC interceptor that recovers from panics.
// It logs the panic and stack trace, then returns an Internal error.
func RecoveryInterceptor(logger *slog.Logger) connect.Interceptor {
	return &recoveryInterceptor{logger: logger}
}

func (i *recoveryInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return connect.UnaryFunc(func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				i.logger.ErrorContext(ctx, "panic recovered",
					"procedure", req.Spec().Procedure,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				err = connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error"))
				resp = nil
			}
		}()
		return next(ctx, req)
	})
}

func (i *recoveryInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *recoveryInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return connect.StreamingHandlerFunc(func(ctx context.Context, conn connect.StreamingHandlerConn) (err error) {
		defer func() {
			if r := recover(); r != nil {
				i.logger.ErrorContext(ctx, "panic recovered in stream",
					"procedure", conn.Spec().Procedure,
					"panic", r,
					"stack", string(debug.Stack()),
				)
				err = connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error"))
			}
		}()
		return next(ctx, conn)
	})
}
