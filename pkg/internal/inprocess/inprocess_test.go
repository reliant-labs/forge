package inprocess

import (
	"io"
	"net/http"
	"testing"
)

// The in-process hop never takes the process down: a handler that panics
// is a 500 to the forwarding client, and one that writes nothing is a 200.
func TestTransport_PanicAndEmptyHandlers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		status  int
		bodyErr bool
	}{
		{"panic", func(http.ResponseWriter, *http.Request) { panic("boom") }, http.StatusInternalServerError, true},
		{"empty", func(http.ResponseWriter, *http.Request) {}, http.StatusOK, false},
		{"streamed", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Hop", "1")
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, "a")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "b")
		}, http.StatusAccepted, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, BaseURL+"/x", nil)
			res, err := Transport{Handler: tc.handler}.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			body, err := io.ReadAll(res.Body)
			if res.StatusCode != tc.status || (err != nil) != tc.bodyErr {
				t.Fatalf("status %d (want %d), body %q, read error %v", res.StatusCode, tc.status, body, err)
			}
			if tc.name == "streamed" && (string(body) != "ab" || res.Header.Get("X-Hop") != "1") {
				t.Errorf("body %q, header %v", body, res.Header)
			}
		})
	}
}

// WithOrigin hands the outer caller's address and Host to the handler.
func TestTransport_Origin(t *testing.T) {
	outer, _ := http.NewRequest(http.MethodPost, "http://api.example/x", nil)
	outer.RemoteAddr = "203.0.113.9:4444"
	var got *http.Request
	req, _ := http.NewRequestWithContext(WithOrigin(outer.Context(), outer), http.MethodPost, BaseURL+"/x", nil)
	res, err := Transport{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r })}.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(res.Body)
	if got.RemoteAddr != outer.RemoteAddr || got.Host != "api.example" {
		t.Errorf("handler saw %s / %s", got.RemoteAddr, got.Host)
	}
}
