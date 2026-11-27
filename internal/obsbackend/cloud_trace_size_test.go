package obsbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
)

// bigTraceServer answers GetTrace with spans whose attributes make the
// document over the control-plane client's default read limit, as a long
// prod trace does (10,000 spans measured 17 MiB).
func bigTraceServer(t *testing.T, spans int) (*cloud.Client, int) {
	t.Helper()
	type span struct {
		SpanID     string            `json:"spanId"`
		Name       string            `json:"name"`
		Service    string            `json:"service"`
		StartTime  time.Time         `json:"startTime"`
		DurationNs string            `json:"durationNs"`
		Attributes map[string]string `json:"attributes"`
	}
	resp := struct {
		TraceID string `json:"traceId"`
		Spans   []span `json:"spans"`
	}{TraceID: "10e86f33868174eb8aa7576fc50dbaea"}
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	for i := range spans {
		resp.Spans = append(resp.Spans, span{
			SpanID: fmt.Sprintf("%016x", i+1), Name: "SELECT jobs", Service: "api",
			StartTime: start.Add(time.Duration(i) * time.Millisecond), DurationNs: "1000000",
			Attributes: map[string]string{"db.query.text": strings.Repeat("q", 1700)},
		})
	}
	body, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/GetTrace") {
			http.Error(w, `{"code":"unimplemented"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	ep, err := cloud.ResolveEndpoint("prod", &cloud.Declaration{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return cloud.NewClient(ep, cloud.Credential{Token: "tok", Source: cloud.SourceEnv, From: "ACME_TOKEN"}), len(body)
}

// Regression: `forge obs trace <id> prod` and `forge obs logs prod --trace
// <id>` failed on every long trace with "the control plane could not be
// reached: decode …GetTrace response: unexpected end of JSON input": the
// client cut the answer off at 8 MiB. A client set up the way the obs verbs
// set theirs up reads it whole.
func TestCloudTrace_ReadsATraceOverTheDefaultReadLimit(t *testing.T) {
	client, size := bigTraceServer(t, 5500)
	if size <= cloud.DefaultMaxResponseBytes {
		t.Fatalf("fixture is %d bytes, want over %d", size, cloud.DefaultMaxResponseBytes)
	}
	DeclareCloudScopes(client)
	c := Cloud{Env: "prod", EnvID: "env-1", OrgID: "org-1", Caller: client}
	rng := Range{From: time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)}

	res, err := c.Trace(context.Background(), TraceRequest{Narrow: Narrow{Range: rng}, TraceID: "10e86f33868174eb8aa7576fc50dbaea"})
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if len(res.Spans) != 5500 {
		t.Fatalf("spans = %d, want 5500", len(res.Spans))
	}
	ext, err := c.TraceExtent(context.Background(), TraceRequest{Narrow: Narrow{Range: rng}, TraceID: "10e86f33868174eb8aa7576fc50dbaea"})
	if err != nil {
		t.Fatalf("TraceExtent (obs logs --trace): %v", err)
	}
	if ext.Spans != 5500 {
		t.Fatalf("extent spans = %d, want 5500", ext.Spans)
	}
}

// An answer over even the declared limit is reported as too big to read,
// never as an unreachable control plane.
func TestMapCloudError_ResponseTooLargeIsNotUnreachable(t *testing.T) {
	err := MapCloudError(&cloud.ResponseTooLargeError{Procedure: procGetTrace, Limit: MaxTraceResponseBytes})
	if exitOf(err) != ExitUndetermined {
		t.Fatalf("exit %d, want %d", exitOf(err), ExitUndetermined)
	}
	if strings.Contains(err.Error(), "could not be reached") {
		t.Errorf("%q says the control plane could not be reached; it answered", err)
	}
	if !strings.Contains(err.Error(), "GetTrace") || !strings.Contains(err.Error(), "64 MiB") {
		t.Errorf("%q should name the procedure and the limit", err)
	}
	if !errors.Is(err, cloud.ErrResponseTooLarge) {
		t.Error("the cause is lost")
	}
}

// The GetTrace limit is declared wherever the scopes are.
func TestDeclareCloudScopes_DeclaresTheTraceReadLimit(t *testing.T) {
	d := &limitDeclarer{recordingDeclarer: recordingDeclarer{}, limits: map[string]int64{}}
	DeclareCloudScopes(d)
	if d.limits[procGetTrace] != MaxTraceResponseBytes {
		t.Fatalf("GetTrace limit = %d, want %d", d.limits[procGetTrace], MaxTraceResponseBytes)
	}
}

type limitDeclarer struct {
	recordingDeclarer
	limits map[string]int64
}

func (d *limitDeclarer) DeclareResponseLimit(limit int64, procedures ...string) {
	for _, p := range procedures {
		d.limits[p] = limit
	}
}
