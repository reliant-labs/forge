package svcerr_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// serve mounts one unary handler behind svcerr.Interceptor and calls it.
func serve(t *testing.T, handlerErr error) error {
	t.Helper()
	const procedure = "/svcerr.v1.Svc/Do"
	mux := http.NewServeMux()
	mux.Handle(procedure, connect.NewUnaryHandler(procedure,
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			if handlerErr != nil {
				return nil, handlerErr
			}
			return connect.NewResponse(&emptypb.Empty{}), nil
		}, connect.WithInterceptors(svcerr.Interceptor())))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+procedure)
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	return err
}

func TestInterceptor_UnwrappedConstructorKeepsItsCode(t *testing.T) {
	// The defect: without the interceptor connect-go sends CodeUnknown.
	if got := connect.CodeOf(svcerr.PermissionDenied("nope")); got != connect.CodeUnknown {
		t.Fatalf("precondition: connect.CodeOf(unwrapped svcerr) = %v, want unknown", got)
	}
	if got := connect.CodeOf(serve(t, svcerr.PermissionDenied("nope"))); got != connect.CodePermissionDenied {
		t.Fatalf("client code = %v, want permission_denied", got)
	}
	if got := connect.CodeOf(serve(t, svcerr.NotFound("note"))); got != connect.CodeNotFound {
		t.Fatalf("client code = %v, want not_found", got)
	}
}

func TestInterceptor_IdempotentAndPassThrough(t *testing.T) {
	if err := serve(t, nil); err != nil {
		t.Fatalf("success path: %v", err)
	}
	if got := connect.CodeOf(serve(t, svcerr.Wrap(svcerr.FailedPrecondition("later")))); got != connect.CodeFailedPrecondition {
		t.Fatalf("already-wrapped code = %v", got)
	}
	reasoned := svcerr.WithReason(svcerr.FailedPrecondition("later"), "TOO_EARLY")
	err := serve(t, reasoned)
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Meta().Get(svcerr.ReasonHeader) != "TOO_EARLY" {
		t.Fatalf("reason header lost: %v", err)
	}
}

func TestInterceptor_RawErrorIsRedactedInternal(t *testing.T) {
	err := serve(t, errors.New(`ERROR: relation "accounts" does not exist dsn=postgres://u:p@db/x`))
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeInternal || ce.Message() != svcerr.InternalMessage {
		t.Fatalf("raw error reached the client as %v", err)
	}
}
