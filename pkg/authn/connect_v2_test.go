package authn

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	connectv2 "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/reliant-labs/forge/pkg/auth"
)

const (
	echoProcedure  = "/test.v1.EchoService/Echo"
	watchProcedure = "/test.v1.EchoService/Watch"
)

// echoHandler answers a call from its context: the reply text, or an error.
type echoHandler func(ctx context.Context) (string, error)

// serveV2 mounts a v2 server with a unary Echo and a server-streaming Watch
// behind ints and returns a client for it.
func serveV2(t *testing.T, h echoHandler, ints ...connectv2.ServerInterceptor) *connectv2.Client {
	t.Helper()
	serve := func(ctx context.Context, _ connectv2.Spec, stream connectv2.ServerStream) error {
		var req wrapperspb.StringValue
		if err := stream.Receive(&req); err != nil {
			return err
		}
		out, err := h(ctx)
		if err != nil {
			return err
		}
		return stream.Send(wrapperspb.String(out))
	}
	srv := connectv2.NewServer(ints...)
	srv.Register(
		connectv2.Method{Spec: connectv2.Spec{StreamType: connectv2.StreamTypeUnary, Procedure: echoProcedure}, Handler: serve},
		connectv2.Method{Spec: connectv2.Spec{StreamType: connectv2.StreamTypeServer, Procedure: watchProcedure}, Handler: serve},
	)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	return connectv2.NewClient(connecthttp.NewTransport(hs.Client(), hs.URL))
}

// callV2 calls Echo, or Watch when stream is set, with headers.
func callV2(t *testing.T, c *connectv2.Client, stream bool, headers map[string]string) (string, error) {
	t.Helper()
	ctx, info := connectv2.NewClientContext(context.Background())
	for k, v := range headers {
		info.RequestHeader().Set(k, v)
	}
	if !stream {
		var res wrapperspb.StringValue
		err := c.CallUnary(ctx, connectv2.Spec{StreamType: connectv2.StreamTypeUnary, Procedure: echoProcedure}, wrapperspb.String("hi"), &res)
		return res.GetValue(), err
	}
	cs, err := c.CallServerStream(ctx, connectv2.Spec{StreamType: connectv2.StreamTypeServer, Procedure: watchProcedure}, wrapperspb.String("hi"))
	if err != nil {
		return "", err
	}
	defer cs.Close()
	var res wrapperspb.StringValue
	if err := cs.Receive(&res); err != nil {
		return "", err
	}
	if err := cs.Receive(&wrapperspb.StringValue{}); !errors.Is(err, io.EOF) {
		return "", err
	}
	return res.GetValue(), nil
}

// callV1 serves the same unary Echo on connect-go v1 behind ic and calls it.
func callV1(t *testing.T, h echoHandler, ic connect.Interceptor, headers map[string]string) (string, error) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(echoProcedure, connect.NewUnaryHandler(echoProcedure,
		func(ctx context.Context, _ *connect.Request[wrapperspb.StringValue]) (*connect.Response[wrapperspb.StringValue], error) {
			out, err := h(ctx)
			if err != nil {
				return nil, err
			}
			return connect.NewResponse(wrapperspb.String(out)), nil
		}, connect.WithInterceptors(ic)))
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	req := connect.NewRequest(wrapperspb.String("hi"))
	for k, v := range headers {
		req.Header().Set(k, v)
	}
	res, err := connect.NewClient[wrapperspb.StringValue, wrapperspb.StringValue](hs.Client(), hs.URL+echoProcedure).CallUnary(context.Background(), req)
	if err != nil {
		return "", err
	}
	return res.Msg.GetValue(), nil
}

// assertSameWireError fails unless the v1 and v2 calls failed with the same
// code and the same message, and that code is want.
func assertSameWireError(t *testing.T, errV1, errV2 error, want connect.Code) {
	t.Helper()
	e1, ok := errors.AsType[*connect.Error](errV1)
	if !ok {
		t.Fatalf("v1 call: %v, want a connect error", errV1)
	}
	e2, ok := errors.AsType[*connectv2.Error](errV2)
	if !ok {
		t.Fatalf("v2 call: %v, want a connect error", errV2)
	}
	if e1.Code() != want || uint32(e2.Code()) != uint32(e1.Code()) {
		t.Errorf("codes v1 %v / v2 %v, want both %v", e1.Code(), e2.Code(), want)
	}
	if e2.Message() != e1.Message() {
		t.Errorf("messages differ on the wire:\n  v1 %q\n  v2 %q", e1.Message(), e2.Message())
	}
}

func whoAmI(ctx context.Context) (string, error) {
	c, err := GetUser(ctx)
	if err != nil {
		return "", err
	}
	return c.UserID, nil
}

// Every rejection the v1 interceptor sends, the v2 interceptor sends with
// the same code and message — including the policy hooks' remaps.
func TestNewInterceptorV2_WireParityWithV1(t *testing.T) {
	revoked := errors.New("account revoked")
	validate := func(tok string) (*auth.Claims, error) {
		switch tok {
		case "good":
			return &auth.Claims{UserID: "u1"}, nil
		case "revoked":
			return nil, revoked
		}
		return nil, errors.New("signature invalid")
	}
	base := Policy{ValidatorConfigured: true, Validate: validate}

	for name, tc := range map[string]struct {
		policy  func(Policy) Policy
		headers map[string]string
		want    connect.Code
	}{
		"missing header": {policy: func(p Policy) Policy { return p }, want: connect.CodeUnauthenticated},
		"malformed":      {policy: func(p Policy) Policy { return p }, headers: map[string]string{"Authorization": "Token x"}, want: connect.CodeUnauthenticated},
		"invalid token":  {policy: func(p Policy) Policy { return p }, headers: map[string]string{"Authorization": "Bearer nope"}, want: connect.CodeUnauthenticated},
		"MapError remap": {
			policy: func(p Policy) Policy {
				p.MapError = func(err error, fb *connect.Error) *connect.Error {
					if errors.Is(err, revoked) {
						return connect.NewError(connect.CodePermissionDenied, errors.New("account revoked"))
					}
					return nil
				}
				return p
			},
			headers: map[string]string{"Authorization": "Bearer revoked"}, want: connect.CodePermissionDenied,
		},
		"MapErrorV2 remap": {
			policy: func(p Policy) Policy {
				p.MapErrorV2 = func(err error, fb *connectv2.Error) *connectv2.Error {
					if errors.Is(err, revoked) {
						return connectv2.NewError(connectv2.CodePermissionDenied, "account revoked").WithCause(fb)
					}
					return nil
				}
				return p
			},
			headers: map[string]string{"Authorization": "Bearer revoked"}, want: connect.CodePermissionDenied,
		},
		"MapErrorV2 keeps fallback": {
			policy: func(p Policy) Policy {
				p.MapErrorV2 = func(error, *connectv2.Error) *connectv2.Error { return nil }
				return p
			},
			headers: map[string]string{"Authorization": "Bearer nope"}, want: connect.CodeUnauthenticated,
		},
		"Enrich plain error": {
			policy: func(p Policy) Policy {
				p.Enrich = func(context.Context, *auth.Claims) (*auth.Claims, error) { return nil, errors.New("db down") }
				return p
			},
			headers: map[string]string{"Authorization": "Bearer good"}, want: connect.CodeUnauthenticated,
		},
		"Enrich v1 connect error": {
			policy: func(p Policy) Policy {
				p.Enrich = func(context.Context, *auth.Claims) (*auth.Claims, error) {
					return nil, connect.NewError(connect.CodePermissionDenied, errors.New("suspended"))
				}
				return p
			},
			headers: map[string]string{"Authorization": "Bearer good"}, want: connect.CodePermissionDenied,
		},
		"Enrich v2 connect error": {
			policy: func(p Policy) Policy {
				p.Enrich = func(context.Context, *auth.Claims) (*auth.Claims, error) {
					return nil, connectv2.NewError(connectv2.CodePermissionDenied, "suspended")
				}
				return p
			},
			headers: map[string]string{"Authorization": "Bearer good"}, want: connect.CodePermissionDenied,
		},
		"no principal in handler": {
			policy:  func(p Policy) Policy { p.AnonymousOK = true; return p },
			headers: nil, want: connect.CodeUnauthenticated,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := tc.policy(base)
			ic1, err := NewInterceptor(p)
			if err != nil {
				t.Fatal(err)
			}
			ic2, err := NewInterceptorV2(p)
			if err != nil {
				t.Fatal(err)
			}
			_, errV1 := callV1(t, whoAmI, ic1, tc.headers)
			_, errV2 := callV2(t, serveV2(t, func(ctx context.Context) (string, error) {
				c, err := GetUserV2(ctx)
				if err != nil {
					return "", err
				}
				return c.UserID, nil
			}, ic2), false, tc.headers)
			assertSameWireError(t, errV1, errV2, tc.want)
		})
	}
}

func TestNewInterceptorV2_AdmitsAndStashesClaims(t *testing.T) {
	ic, err := NewInterceptorV2(Policy{
		ValidatorConfigured: true,
		Validate:            func(string) (*auth.Claims, error) { return &auth.Claims{UserID: "u-default"}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	c := serveV2(t, whoAmI, ic)
	for _, stream := range []bool{false, true} {
		got, err := callV2(t, c, stream, map[string]string{"Authorization": "Bearer tok"})
		if err != nil {
			t.Fatalf("stream=%v: %v", stream, err)
		}
		if got != "u-default" {
			t.Errorf("stream=%v: GetUser read %q, want the admitted u-default", stream, got)
		}
	}
	if _, err := callV2(t, c, true, nil); connectv2.CodeOf(err) != connectv2.CodeUnauthenticated {
		t.Errorf("a stream with no credential must be rejected, got %v", err)
	}
}

func TestNewInterceptorV2_RefusesWhatNewInterceptorRefuses(t *testing.T) {
	validate := func(string) (*auth.Claims, error) { return nil, nil }
	for name, p := range map[string]Policy{
		"unconfigured":   {},
		"no validator":   {ValidatorConfigured: true},
		"two validators": {ValidatorConfigured: true, Validate: validate, ValidateContext: func(context.Context, string) (*auth.Claims, error) { return nil, nil }},
	} {
		t.Run(name, func(t *testing.T) {
			_, err1 := NewInterceptor(p)
			ic, err2 := NewInterceptorV2(p)
			if err1 == nil || err2 == nil || ic != nil {
				t.Fatalf("v1 err %v, v2 (%v, %v); want both refused", err1, ic != nil, err2)
			}
			if !strings.HasPrefix(err2.Error(), "authn.NewInterceptorV2: ") ||
				strings.TrimPrefix(err2.Error(), "authn.NewInterceptorV2: ") != strings.TrimPrefix(err1.Error(), "authn.NewInterceptor: ") {
				t.Errorf("refusals differ beyond the constructor name:\n  v1 %s\n  v2 %s", err1, err2)
			}
		})
	}
}

func TestNewInterceptorV2_ExternalAuthIsPassthrough(t *testing.T) {
	ic, err := NewInterceptorV2(Policy{ExternalAuth: true})
	if err != nil || ic == nil {
		t.Fatalf("NewInterceptorV2(ExternalAuth) = %v, %v; want a passthrough interceptor", ic != nil, err)
	}
	var called atomic.Bool
	c := serveV2(t, func(context.Context) (string, error) { called.Store(true); return "ok", nil }, ic)
	if _, err := callV2(t, c, false, map[string]string{"Authorization": "garbage"}); err != nil {
		t.Fatalf("passthrough must not inspect the header: %v", err)
	}
	if !called.Load() {
		t.Fatal("passthrough must reach the handler")
	}
}

// The allow-list is the exact procedure string, as in v1.
func TestNewInterceptorV2_AllowListBypassesValidation(t *testing.T) {
	var validated atomic.Bool
	p := Policy{
		ValidatorConfigured: true,
		Validate:            func(string) (*auth.Claims, error) { validated.Store(true); return &auth.Claims{UserID: "u"}, nil },
		Unauthenticated:     map[string]struct{}{echoProcedure: {}},
	}
	ic, err := NewInterceptorV2(p)
	if err != nil {
		t.Fatal(err)
	}
	c := serveV2(t, func(context.Context) (string, error) { return "ok", nil }, ic)
	if _, err := callV2(t, c, false, nil); err != nil {
		t.Fatalf("allow-listed procedure: %v", err)
	}
	if validated.Load() {
		t.Error("an allow-listed procedure must bypass the validator")
	}
	if _, err := callV2(t, c, true, nil); connectv2.CodeOf(err) != connectv2.CodeUnauthenticated {
		t.Errorf("the allow-list is exact: %s must still authenticate, got %v", watchProcedure, err)
	}
}

// A v1 chain honours MapErrorV2 when MapError is unset, so one Policy can
// serve both majors while a project migrates.
func TestNewInterceptor_HonoursMapErrorV2(t *testing.T) {
	p := Policy{
		ValidatorConfigured: true,
		Validate:            func(string) (*auth.Claims, error) { return nil, errors.New("expired") },
		MapErrorV2: func(err error, fb *connectv2.Error) *connectv2.Error {
			if fb.Code() != connectv2.CodeUnauthenticated || fb.Message() != "invalid token: expired" {
				t.Errorf("fallback = %v, want unauthenticated: invalid token: expired", fb)
			}
			return connectv2.NewError(connectv2.CodePermissionDenied, "token expired")
		},
	}
	ic, err := NewInterceptor(p)
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := callV1(t, whoAmI, ic, map[string]string{"Authorization": "Bearer x"})
	e1, ok := errors.AsType[*connect.Error](callErr)
	if !ok || e1.Code() != connect.CodePermissionDenied || e1.Message() != "token expired" {
		t.Fatalf("v1 wire error = %v, want permission_denied: token expired", callErr)
	}
}

func TestPolicyClaimsReader(t *testing.T) {
	validate := func(string) (*auth.Claims, error) { return &auth.Claims{UserID: "u1"}, nil }
	defaultStash := validatePolicy(validate)
	defaultStash.ContextWithClaims = nil
	for name, tc := range map[string]struct {
		policy     Policy
		wantReader bool
	}{
		"default stash":   {defaultStash, true},
		"project's stash": {validatePolicy(validate), false},
		"external auth":   {Policy{ExternalAuth: true}, false},
		"unconfigured":    {Policy{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := tc.policy.ClaimsReader() != nil; got != tc.wantReader {
				t.Fatalf("ClaimsReader() != nil is %v, want %v", got, tc.wantReader)
			}
			// It answers what the v1 interceptor's Claims method answers.
			if ic, err := NewInterceptor(tc.policy); err == nil {
				if v1 := ic.(claimsReader).Claims() != nil; v1 != tc.wantReader {
					t.Errorf("v1 Claims() != nil is %v, ClaimsReader says %v", v1, tc.wantReader)
				}
			}
		})
	}

	// The reader reads what the v2 interceptor stashed.
	ic, err := NewInterceptorV2(defaultStash)
	if err != nil {
		t.Fatal(err)
	}
	read := defaultStash.ClaimsReader()
	got, err := callV2(t, serveV2(t, func(ctx context.Context) (string, error) {
		c, _ := read(ctx)
		if c == nil {
			return "<none>", nil
		}
		return c.UserID, nil
	}, ic), false, map[string]string{"Authorization": "Bearer t"})
	if err != nil || got != "u1" {
		t.Fatalf("the reader read %q (%v), want the admitted caller u1", got, err)
	}
}

func TestGetUserV2(t *testing.T) {
	want := &Claims{UserID: "u1"}
	got, err := GetUserV2(ContextWithClaims(context.Background(), want))
	if err != nil || got != want {
		t.Fatalf("GetUserV2 = %v, %v; want the stashed claims", got, err)
	}
	_, err1 := GetUser(context.Background())
	_, err2 := GetUserV2(context.Background())
	e1, _ := errors.AsType[*connect.Error](err1)
	e2, ok := errors.AsType[*connectv2.Error](err2)
	if !ok || e1 == nil || uint32(e2.Code()) != uint32(e1.Code()) || e2.Message() != e1.Message() || e2.Error() != e1.Error() {
		t.Fatalf("GetUserV2 error %v, GetUser error %v; want the same code and message", err2, err1)
	}
}

func TestErrorConversionKeepsTheOutermostVerdict(t *testing.T) {
	inner := connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token: x"))
	outer := connectv2.NewError(connectv2.CodePermissionDenied, "revoked").WithCause(inner)
	if got := errorToV1(outer); connect.CodeOf(got) != connect.CodePermissionDenied {
		t.Errorf("errorToV1 took the wrapped v1 cause's code: %v", got)
	}
	if got := errorToV2(outer); got != error(outer) {
		t.Errorf("errorToV2 must keep a v2 error as is, got %v", got)
	}
	plain := errors.New("x")
	if errorToV1(plain) != plain || errorToV1(inner) != error(inner) {
		t.Error("errorToV1 must return v1 and plain errors unchanged")
	}
	back := errorToV1(connectv2.NewError(connectv2.CodeNotFound, ""))
	if e, ok := errors.AsType[*connect.Error](back); !ok || e.Code() != connect.CodeNotFound || e.Message() != "" || e.Error() != "not_found" {
		t.Errorf("an empty v2 message must stay empty in v1, got %v", back)
	}
}
