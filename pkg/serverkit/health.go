package serverkit

import (
	"context"
	"errors"
	"net/http"

	"connectrpc.com/connect"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/reliant-labs/forge/pkg/observe"
)

// health.go — grpc.health.v1.Health/Check, served as a Connect rpc.
//
// The standard health rpc, through the listener's own chain: unlike /healthz
// and /readyz, which are served in front of the edge and record nothing (a
// kubelet calls them every few seconds), a call to it is a real request of the
// process — traced, timed and logged like any rpc. That is what makes it the
// request `forge env up` sends once a stack is up: it shows the stack's
// telemetry working end to end before the app has served anyone, and it has
// no side effects to send it for.
//
// IT IS A PROBE, AND SAYS SO. Its server span carries forge.probe=true
// (observe.MarkProbeSpans), the attribute release health keeps out of real
// traffic (obsquery.SpanNotProbeSQL): `forge obs check`, RED and the issue
// views never count it, while `forge obs endpoints` lists it under its own
// name. Marking it needs no trust: whoever calls the health rpc is probing,
// and the mark can hide no other rpc.
//
// Auth: generated projects allow /grpc.health.v1.Health/Check and Watch
// unauthenticated (pkg/middleware UnauthenticatedProcedures, pkg/auth's
// built-in skip). Watch is not served: nothing forge runs streams health.

// HealthCheckProcedure is the procedure of the health rpc serverkit mounts.
const HealthCheckProcedure = "/grpc.health.v1.Health/Check"

// healthCheck answers grpc.health.v1.Health/Check: SERVING for the process
// ("") and for every service the listener mounted, NotFound for any other.
func healthCheck(services []string) func(context.Context, *connect.Request[healthpb.HealthCheckRequest]) (*connect.Response[healthpb.HealthCheckResponse], error) {
	known := make(map[string]bool, len(services))
	for _, s := range services {
		known[s] = true
	}
	return func(ctx context.Context, req *connect.Request[healthpb.HealthCheckRequest]) (*connect.Response[healthpb.HealthCheckResponse], error) {
		observe.MarkProbeSpans(ctx)
		if svc := req.Msg.GetService(); svc != "" && !known[svc] {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown service "+svc))
		}
		return connect.NewResponse(&healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}), nil
	}
}

// mountHealth serves the health rpc on mux, built with the listener's chain
// (opts), for the services the listener mounted. A project that already
// mounts it keeps its own: ServeMux panics on a pattern registered twice, and
// that is the one panic recovered here.
func mountHealth(mux *http.ServeMux, services []string, opts ...connect.HandlerOption) (mounted bool) {
	defer func() {
		if recover() != nil {
			mounted = false
		}
	}()
	mux.Handle(HealthCheckProcedure, connect.NewUnaryHandler(HealthCheckProcedure, healthCheck(services), opts...))
	return true
}

// withHealthRoute sends the health rpc to mux ahead of handler: a REST
// transcoder serves only the project's own services, so the health rpc
// mounted on the mux beneath it would be unreachable through it.
func withHealthRoute(mux *http.ServeMux, handler http.Handler) http.Handler {
	routed := http.NewServeMux()
	routed.Handle(HealthCheckProcedure, mux)
	routed.Handle("/", handler)
	return routed
}
