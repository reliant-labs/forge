package serverkit_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/otelconnect"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/reliant-labs/forge/pkg/observe"
	"github.com/reliant-labs/forge/pkg/serverkit"
)

// A chain authenticates: a missing Auth, an audience-less one, or one wrapping
// no interceptor fails at NewChain, so a server with no authenticator is a
// boot error and never a running process.
func TestNewChain_RequiresAuth(t *testing.T) {
	cases := map[string]struct {
		deps serverkit.ChainDeps
		want string
	}{
		"nil auth":           {serverkit.ChainDeps{}, "ChainDeps.Auth is required"},
		"no audience":        {serverkit.ChainDeps{Auth: serverkit.AuthFor("", tokenAuth{})}, "names no audience"},
		"nil interceptor":    {serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, nil)}, "wraps a nil interceptor"},
		"omit in wrong slot": {serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{}), Audit: serverkit.NoTrace}, `ChainDeps.Audit is serverkit.Omit("trace")`},
		"omit as deadline":   {serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{}), Deadline: serverkit.NoAudit}, `ChainDeps.Deadline is serverkit.Omit("audit")`},
		"bad deadline key":   {serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{}), Deadline: serverkit.Deadline{Procedures: map[string]time.Duration{"svc.v1.Svc/Do": time.Second}}}, `"svc.v1.Svc/Do" is not a procedure`},
		"omit as rate limit": {serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{}), RateLimit: serverkit.NoAudit}, "ChainDeps.RateLimit"},
		"nil extra":          {serverkit.ChainDeps{Auth: serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{}), Extras: []connect.Interceptor{nil}}, "Extras[0] is nil"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := serverkit.NewChain(tc.deps)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("NewChain = %v, %v; want an error containing %q", c, err, tc.want)
			}
		})
	}
}

// recorder is an interceptor that records, in order, which slot it fills.
type recorder struct {
	name string
	mu   *sync.Mutex
	got  *[]string
}

func (r recorder) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		r.mu.Lock()
		*r.got = append(*r.got, r.name)
		r.mu.Unlock()
		return next(ctx, req)
	}
}

func (r recorder) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (r recorder) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// The order a request passes through the chain is the order serve.go used to
// spell inline: the span outermost, then the deadline before auth (the RPC's
// budget bounds authenticating), auth before audit (audit knows who called),
// rate limit, the project's extras, the activity stream (next to the handler,
// so it judges the handler's outcome), the stream drain, and protovalidate
// innermost, right before the handler.
func TestNewChain_SlotOrder(t *testing.T) {
	var (
		mu  sync.Mutex
		got []string
	)
	rec := func(name string) recorder { return recorder{name: name, mu: &mu, got: &got} }
	chain, err := serverkit.NewChain(serverkit.ChainDeps{
		Auth:        serverkit.AuthFor(serverkit.AudienceUser, rec("auth")),
		Trace:       rec("trace"),
		Deadline:    rec("deadline"),
		Audit:       rec("audit"),
		RateLimit:   rec("rate-limit"),
		Extras:      []connect.Interceptor{rec("extra-1"), rec("extra-2")},
		Activity:    rec("activity"),
		StreamDrain: rec("stream-drain"),
		Validate:    rec("validate"),
	})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	const procedure = "/serverkit.chain.v1.Svc/Do"
	mux := http.NewServeMux()
	mux.Handle(procedure, connect.NewUnaryHandler(procedure,
		func(context.Context, *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
			mu.Lock()
			got = append(got, "handler")
			mu.Unlock()
			return connect.NewResponse(&emptypb.Empty{}), nil
		}, chain.HandlerOptions()...))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+procedure)
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{})); err != nil {
		t.Fatalf("call: %v", err)
	}
	want := []string{"trace", "deadline", "auth", "audit", "rate-limit", "extra-1", "extra-2", "activity", "stream-drain", "validate", "handler"}
	if !slices.Equal(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	if chain.Audience() != serverkit.AudienceUser {
		t.Fatalf("Audience = %q", chain.Audience())
	}
}

// Every slot but Auth defaults, and only its Omit drops it: a chain with
// every default dropped is exactly observe's fixed core plus Auth.
func TestNewChain_DefaultsAndOmits(t *testing.T) {
	auth := serverkit.AuthFor(serverkit.AudienceUser, tokenAuth{token: "t"})
	full, err := serverkit.NewChain(serverkit.ChainDeps{Auth: auth})
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	bare, err := serverkit.NewChain(serverkit.ChainDeps{
		Auth:        auth,
		Trace:       serverkit.NoTrace,
		Deadline:    serverkit.NoDeadline,
		Audit:       serverkit.NoAudit,
		Activity:    serverkit.NoActivity,
		StreamDrain: serverkit.NoStreamDrain,
		Validate:    serverkit.NoValidate,
	})
	if err != nil {
		t.Fatalf("NewChain (all omitted): %v", err)
	}
	core := len(observe.Chain(observe.Deps{Auth: auth}))
	if n := len(bare.Interceptors()); n != core {
		t.Fatalf("all defaults omitted: %d interceptors, want observe's core plus auth (%d)", n, core)
	}
	// trace (+ its profile-labels and traceresponse companions), deadline,
	// audit, activity, stream drain, validate.
	if n := len(full.Interceptors()); n != core+8 {
		t.Fatalf("all defaults: %d interceptors, want %d", n, core+8)
	}
}

// The payload caps default to 4 MiB, because connect-go reads a whole body
// before any interceptor can refuse it and treats a zero cap as unlimited.
func TestNewChain_PayloadCapDefault(t *testing.T) {
	const procedure = "/serverkit.chain.v1.Svc/Echo"
	for name, tc := range map[string]struct {
		read int
		want connect.Code
	}{
		"zero is 4 MiB":         {0, connect.CodeResourceExhausted},
		"negative is unlimited": {-1, 0},
	} {
		t.Run(name, func(t *testing.T) {
			chain, err := serverkit.NewChain(serverkit.ChainDeps{
				Auth:         serverkit.AuthFor(serverkit.AudienceUser, recorder{mu: &sync.Mutex{}, got: new([]string)}),
				ReadMaxBytes: tc.read,
			})
			if err != nil {
				t.Fatalf("NewChain: %v", err)
			}
			mux := http.NewServeMux()
			mux.Handle(procedure, connect.NewUnaryHandler(procedure,
				func(_ context.Context, req *connect.Request[wrapperspb.BytesValue]) (*connect.Response[emptypb.Empty], error) {
					return connect.NewResponse(&emptypb.Empty{}), nil
				}, chain.HandlerOptions()...))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			client := connect.NewClient[wrapperspb.BytesValue, emptypb.Empty](srv.Client(), srv.URL+procedure)
			_, err = client.CallUnary(context.Background(), connect.NewRequest(wrapperspb.Bytes(bytes.Repeat([]byte{'x'}, serverkit.DefaultMaxBytes+1))))
			if got := connect.CodeOf(err); err != nil && got != tc.want || err == nil && tc.want != 0 {
				t.Fatalf("5 MiB request: err = %v, want code %v", err, tc.want)
			}
		})
	}
}

// The default Trace slot is otelconnect WithTrustRemote: an inbound
// traceparent (the browser's) becomes the PARENT of the server span, so a
// click and the API call it caused share one trace. The control — otelconnect
// without the option — only links to it, and the handler sees another trace.
func TestNewChain_DefaultTraceTrustsRemoteParent(t *testing.T) {
	// A recording tracer: the no-op one hands back whatever span context the
	// request context carries, which cannot tell a parent from a link.
	prevProp, prevTP := otel.GetTextMapPropagator(), otel.GetTracerProvider()
	tp := sdktrace.NewTracerProvider()
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTextMapPropagator(prevProp)
		otel.SetTracerProvider(prevTP)
		_ = tp.Shutdown(context.Background())
	})

	const (
		procedure   = "/serverkit.chain.v1.Svc/Trace"
		inbound     = "4bf92f3577b34da6a3ce929d0e0e4736"
		traceparent = "00-" + inbound + "-00f067aa0ba902b7-01"
	)
	untrusted, err := otelconnect.NewInterceptor()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		trace connect.Interceptor
		same  bool
	}{
		"default":          {nil, true},
		"untrusted remote": {untrusted, false},
	} {
		t.Run(name, func(t *testing.T) {
			chain, err := serverkit.NewChain(serverkit.ChainDeps{
				Auth:  serverkit.AuthFor(serverkit.AudienceUser, recorder{mu: &sync.Mutex{}, got: new([]string)}),
				Trace: tc.trace,
			})
			if err != nil {
				t.Fatalf("NewChain: %v", err)
			}
			var seen string
			mux := http.NewServeMux()
			mux.Handle(procedure, connect.NewUnaryHandler(procedure,
				func(ctx context.Context, _ *connect.Request[emptypb.Empty]) (*connect.Response[emptypb.Empty], error) {
					seen = oteltrace.SpanContextFromContext(ctx).TraceID().String()
					return connect.NewResponse(&emptypb.Empty{}), nil
				}, chain.HandlerOptions()...))
			srv := httptest.NewServer(mux)
			defer srv.Close()
			req := connect.NewRequest(&emptypb.Empty{})
			req.Header().Set("traceparent", traceparent)
			client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+procedure)
			if _, err := client.CallUnary(context.Background(), req); err != nil {
				t.Fatalf("call: %v", err)
			}
			if (seen == inbound) != tc.same {
				t.Fatalf("handler trace = %s, inbound %s; want same=%v", seen, inbound, tc.same)
			}
		})
	}
}
