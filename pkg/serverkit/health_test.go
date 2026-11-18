package serverkit_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/reliant-labs/forge/pkg/authn"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/serverkit"
)

// passAuth admits every request: the health rpc's own auth exemption is the
// project's (middleware UnauthenticatedProcedures), not serverkit's.
type passAuth struct{}

func (passAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc { return next }
func (passAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}
func (passAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func passAuthenticator() serverkit.Authenticator {
	return serverkit.AuthWithCaller(serverkit.AudienceUser, passAuth{}, func(context.Context) (*authn.Claims, bool) { return nil, false })
}

// A listener serves grpc.health.v1.Health/Check through its chain: the call
// is traced as a server span marked forge.probe=true (so release health
// leaves it out) and records no rpc.server.* metric (so metric alerts do).
func TestListenerServesTheHealthRPCAsAMarkedProbe(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	trace, err := otelconnect.NewInterceptor(otelconnect.WithTracerProvider(tp), otelconnect.WithoutMetrics())
	if err != nil {
		t.Fatal(err)
	}
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")
	chain, err := serverkit.NewChain(serverkit.ChainDeps{
		Auth:  passAuthenticator(),
		Trace: trace,
		Meter: meter,
	})
	if err != nil {
		t.Fatal(err)
	}
	l, err := serverkit.NewListener(serverkit.ListenerDeps{
		Config: serverkit.ListenerConfig{Addr: "127.0.0.1:0"},
		Chain:  chain,
		Routes: serverkit.RoutesFunc(func(*http.ServeMux, ...connect.HandlerOption) []string { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := l.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = l.Run(ctx) }()
	t.Cleanup(func() { _ = l.Close(context.Background()) })

	client := connect.NewClient[healthpb.HealthCheckRequest, healthpb.HealthCheckResponse](
		http.DefaultClient, "http://"+l.Addr().String()+serverkit.HealthCheckProcedure, connect.WithProtoJSON())
	resp, err := client.CallUnary(context.Background(), connect.NewRequest(&healthpb.HealthCheckRequest{}))
	if err != nil {
		t.Fatalf("Health/Check: %v", err)
	}
	if resp.Msg.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.Msg.GetStatus())
	}
	_, err = client.CallUnary(context.Background(), connect.NewRequest(&healthpb.HealthCheckRequest{Service: "acme.v1.Nope"}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("a service the listener does not serve: %v, want NotFound", err)
	}

	var marked int
	for _, s := range spans.Ended() {
		if s.Name() != "grpc.health.v1.Health/Check" {
			continue
		}
		for _, kv := range s.Attributes() {
			if kv.Key == attribute.Key(release.ProbeAttr) && kv.Value.Emit() == "true" {
				marked++
			}
		}
	}
	if marked != 2 {
		t.Errorf("%d of the 2 health calls' server spans carry %s=true; release health would count the rest as real traffic", marked, release.ProbeAttr)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "rpc.server.calls" || m.Name == "rpc.server.errors" || m.Name == "rpc.server.duration" {
				t.Errorf("the health rpc recorded %s: metric alerts would count a probe", m.Name)
			}
		}
	}
}

// A project that mounts its own health service keeps it: serverkit does not
// panic on the duplicate pattern, and the project's handler answers.
func TestListenerKeepsAProjectsOwnHealthService(t *testing.T) {
	chain, err := serverkit.NewChain(serverkit.ChainDeps{Auth: passAuthenticator()})
	if err != nil {
		t.Fatal(err)
	}
	own := errors.New("the project's own")
	l, err := serverkit.NewListener(serverkit.ListenerDeps{
		Config: serverkit.ListenerConfig{Addr: "127.0.0.1:0"},
		Chain:  chain,
		Routes: serverkit.RoutesFunc(func(mux *http.ServeMux, opts ...connect.HandlerOption) []string {
			mux.Handle(serverkit.HealthCheckProcedure, connect.NewUnaryHandler(serverkit.HealthCheckProcedure,
				func(context.Context, *connect.Request[healthpb.HealthCheckRequest]) (*connect.Response[healthpb.HealthCheckResponse], error) {
					return nil, connect.NewError(connect.CodeUnavailable, own)
				}, opts...))
			return nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := l.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = l.Run(ctx) }()
	t.Cleanup(func() { _ = l.Close(context.Background()) })
	client := connect.NewClient[healthpb.HealthCheckRequest, healthpb.HealthCheckResponse](
		http.DefaultClient, "http://"+l.Addr().String()+serverkit.HealthCheckProcedure)
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&healthpb.HealthCheckRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Errorf("got %v, want the project's own handler's Unavailable", err)
	}
}
