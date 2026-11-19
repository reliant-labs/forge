package svcerr_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	connectv2 "connectrpc.com/connect/v2"
	connectv2proto "connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

// parityCase is one handler failure, spelled for each major. The two
// spellings are the same error unless the error is a connect type itself,
// which each major spells its own way (a forwarded remote error, an error the
// handler built).
type parityCase struct {
	name   string
	v1, v2 func() error
}

func sameError(name string, err func() error) parityCase {
	return parityCase{name: name, v1: err, v2: err}
}

func parityCases(t *testing.T) []parityCase {
	t.Helper()
	detailV1, err := connect.NewErrorDetail(wrapperspb.String("sku"))
	if err != nil {
		t.Fatal(err)
	}
	detailV2, err := connectv2proto.NewErrorDetail(wrapperspb.String("sku"))
	if err != nil {
		t.Fatal(err)
	}
	remoteV1 := func() *connect.Error {
		ce := connect.NewWireError(connect.CodeNotFound, errors.New("upstream says no"))
		ce.AddDetail(detailV1)
		return ce
	}
	remoteV2 := func() *connectv2.Error {
		return connectv2.NewError(connectv2.CodeNotFound, "upstream says no").WithDetail(detailV2).WithRemote()
	}
	return []parityCase{
		sameError("kind", func() error { return svcerr.PermissionDenied("admin only") }),
		sameError("kind with reason and metadata", func() error {
			return svcerr.WithMetadata(svcerr.WithReason(svcerr.NotFound("note"), "note_missing"), "x-forge-error-scopes", "a,b")
		}),
		sameError("wrapped kind", func() error { return svcerr.Wrap(svcerr.FailedPrecondition("not yet")) }),
		sameError("bare sentinel", func() error { return svcerr.ErrAborted }),
		sameError("unrecognised error", func() error {
			return errors.New(`ERROR: relation "accounts" does not exist dsn=postgres://app:s3cr3t@db/prod`)
		}),
		sameError("context deadline", func() error { return context.DeadlineExceeded }),
		sameError("with cause", func() error {
			return svcerr.WithCause(svcerr.Unavailable("billing is unavailable"), remoteV1())
		}),
		sameError("with detail", func() error {
			return svcerr.WithDetail(svcerr.InvalidArgument("bad sku"), wrapperspb.String("sku"))
		}),
		sameError("scaffold stub", func() error { return svcerr.ScaffoldStub("Do") }),
		{
			name: "handler-built connect error",
			v1:   func() error { return connect.NewError(connect.CodeFailedPrecondition, errors.New("push rejected")) },
			v2:   func() error { return connectv2.NewError(connectv2.CodeFailedPrecondition, "push rejected") },
		},
		{
			name: "forwarded remote error",
			v1:   func() error { return remoteV1() },
			v2:   func() error { return remoteV2() },
		},
		{
			name: "forwarded remote error with reason",
			v1:   func() error { return svcerr.WithReason(remoteV1(), "ignored_on_remote") },
			v2:   func() error { return svcerr.WithReason(remoteV2(), "ignored_on_remote") },
		},
	}
}

// wireResponse is what a client receives, minus what legitimately varies
// between two responses (the Date header).
type wireResponse struct {
	status int
	header http.Header
	body   []byte
}

func post(t *testing.T, url, contentType string, body []byte) wireResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	resp.Header.Del("Date")
	return wireResponse{status: resp.StatusCode, header: resp.Header, body: data}
}

func envelope(payload []byte) []byte {
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}

func diffHeaders(a, b http.Header) []string {
	var diffs []string
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		if !slices.Equal(a.Values(k), b.Values(k)) {
			diffs = append(diffs, k+": v1="+strings.Join(a.Values(k), ",")+" v2="+strings.Join(b.Values(k), ","))
		}
	}
	slices.Sort(diffs)
	return diffs
}

// TestWireParity_InterceptorV1AndV2 is the proof that moving a server from
// connect-go v1 + svcerr.Interceptor to connect-go v2 + svcerr.InterceptorV2
// changes nothing a client receives: for each handler failure, the raw
// Connect unary response (HTTP status, headers, JSON error body), the raw
// Connect server-streaming response (end-of-stream message with its
// metadata), and the gRPC status, message, details and metadata are
// identical.
func TestWireParity_InterceptorV1AndV2(t *testing.T) {
	for _, tc := range parityCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			v1 := httptest.NewServer(muxV1(tc.v1))
			t.Cleanup(v1.Close)
			v2 := httptest.NewServer(muxV2(tc.v2, svcerr.InterceptorV2()))
			t.Cleanup(v2.Close)

			t.Run("connect unary", func(t *testing.T) {
				a := post(t, v1.URL+unaryProcedure, "application/json", []byte("{}"))
				b := post(t, v2.URL+unaryProcedure, "application/json", []byte("{}"))
				if a.status != b.status {
					t.Errorf("HTTP status: v1 %d, v2 %d", a.status, b.status)
				}
				if !bytes.Equal(a.body, b.body) {
					t.Errorf("error body differs:\n v1 %s\n v2 %s", a.body, b.body)
				}
				if d := diffHeaders(a.header, b.header); len(d) > 0 {
					t.Errorf("headers differ:\n %s", strings.Join(d, "\n "))
				}
				if a.status == http.StatusOK {
					t.Fatalf("handler failure answered 200: %s", a.body)
				}
			})

			t.Run("connect server stream", func(t *testing.T) {
				req := envelope([]byte("{}"))
				a := post(t, v1.URL+streamProcedure, "application/connect+json", req)
				b := post(t, v2.URL+streamProcedure, "application/connect+json", req)
				if a.status != b.status {
					t.Errorf("HTTP status: v1 %d, v2 %d", a.status, b.status)
				}
				if !bytes.Equal(a.body, b.body) {
					t.Errorf("stream body differs:\n v1 %q\n v2 %q", a.body, b.body)
				}
				if d := diffHeaders(a.header, b.header); len(d) > 0 {
					t.Errorf("headers differ:\n %s", strings.Join(d, "\n "))
				}
			})

			t.Run("grpc", func(t *testing.T) {
				a := grpcCall(t, muxV1(tc.v1))
				b := grpcCall(t, muxV2(tc.v2, svcerr.InterceptorV2()))
				if a.code != b.code || a.message != b.message {
					t.Errorf("status: v1 %v %q, v2 %v %q", a.code, a.message, b.code, b.message)
				}
				if !slices.Equal(a.details, b.details) {
					t.Errorf("details: v1 %q, v2 %q", a.details, b.details)
				}
				if d := diffHeaders(a.meta, b.meta); len(d) > 0 {
					t.Errorf("metadata differs:\n %s", strings.Join(d, "\n "))
				}
			})
		})
	}
}

type grpcResult struct {
	code    connect.Code
	message string
	details []string
	meta    http.Header
}

// grpcCall calls the unary procedure over gRPC (HTTP/2) with a v1 client —
// what an old daemon or SDK is — and reports the status it read. The v1
// client is the reader here on purpose: what must not change is what EXISTING
// clients see.
func grpcCall(t *testing.T, h http.Handler) grpcResult {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := connect.NewClient[emptypb.Empty, emptypb.Empty](srv.Client(), srv.URL+unaryProcedure, connect.WithGRPC())
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))
	var ce *connect.Error
	if !errors.As(err, &ce) {
		t.Fatalf("gRPC call returned %T %v, want a *connect.Error", err, err)
	}
	out := grpcResult{code: ce.Code(), message: ce.Message(), meta: ce.Meta().Clone()}
	for _, d := range ce.Details() {
		out.details = append(out.details, d.Type()+"="+string(d.Bytes()))
	}
	// Transport framing that differs per response, not per server.
	for _, k := range []string{"Date", "Content-Length"} {
		out.meta.Del(k)
	}
	return out
}
