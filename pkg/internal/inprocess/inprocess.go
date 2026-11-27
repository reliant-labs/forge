// Package inprocess is an http.RoundTripper that answers each request by
// running an http.Handler in this process — the bridge serverkit's REST
// transcoder (serverkit/rest.go) and mcpkit's tool calls use to relay a
// request into the listener's own mux, through its interceptor chain, with
// no socket in between.
package inprocess

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// BaseURL is the base URL of a client over Transport. Nothing dials it:
// Transport serves every request it is handed.
const BaseURL = "http://serverkit.rest.in-process"

type originKey struct{}

// WithOrigin carries the outer request into a relayed call, so the handler
// sees the real caller's address, TLS state and Host rather than the
// in-process hop's.
func WithOrigin(ctx context.Context, origin *http.Request) context.Context {
	return context.WithValue(ctx, originKey{}, origin)
}

// Transport answers each request by running Handler in this process,
// streaming its response body back as it is written.
type Transport struct{ Handler http.Handler }

// RoundTrip implements http.RoundTripper. A handler that panics is a 500 (or
// a truncated body, when headers already went), never a crashed process; a
// handler that writes nothing answered 200 with no body.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if origin, ok := req.Context().Value(originKey{}).(*http.Request); ok {
		req.RemoteAddr, req.TLS, req.Host = origin.RemoteAddr, origin.TLS, origin.Host
	}
	req.RequestURI = req.URL.RequestURI()
	if req.Body == nil {
		req.Body = http.NoBody
	}
	pr, pw := io.Pipe()
	w := &pipeResponseWriter{header: http.Header{}, body: pw, sent: make(chan struct{})}
	go func() {
		var failed error
		defer func() {
			if p := recover(); p != nil {
				failed = fmt.Errorf("serverkit: in-process handler for %s panicked: %v", req.URL.Path, p)
				w.WriteHeader(http.StatusInternalServerError) // unless headers already went
			}
			w.WriteHeader(http.StatusOK) // a handler that wrote nothing answered 200 with no body
			_ = pw.CloseWithError(failed)
		}()
		t.Handler.ServeHTTP(w, req)
	}()
	<-w.sent
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.sentHeader,
		Body:          pr,
		ContentLength: -1,
		Request:       req,
	}, nil
}

// pipeResponseWriter is the handler's side of Transport. Headers are fixed
// at the first WriteHeader (or Write), as on a real connection.
type pipeResponseWriter struct {
	header     http.Header
	body       *io.PipeWriter
	once       sync.Once
	sent       chan struct{}
	status     int
	sentHeader http.Header
}

func (w *pipeResponseWriter) Header() http.Header { return w.header }

func (w *pipeResponseWriter) WriteHeader(status int) {
	w.once.Do(func() {
		w.status, w.sentHeader = status, w.header.Clone()
		close(w.sent)
	})
}

func (w *pipeResponseWriter) Write(b []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.body.Write(b)
}

// Flush is a no-op: every Write already reaches the reader.
func (w *pipeResponseWriter) Flush() {}
