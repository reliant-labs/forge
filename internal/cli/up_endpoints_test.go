package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/obsbackend"
)

func stubUpEndpoints(t *testing.T, q func(context.Context, string) ([]obsEndpointDoc, error)) {
	t.Helper()
	prev := upEndpointsQuery
	upEndpointsQuery = q
	t.Cleanup(func() { upEndpointsQuery = prev })
}

func TestPrintUpEndpointsTable(t *testing.T) {
	stubUpEndpoints(t, func(_ context.Context, env string) ([]obsEndpointDoc, error) {
		if env != "dev" {
			t.Errorf("queried env %q, want dev", env)
		}
		return []obsEndpointDoc{
			{Service: "api", Name: "shop.v1.ShopService/ListWidgets", Requests: 42, ErrorRatio: 0.0238, P50Ms: 3.2, P95Ms: 18, P99Ms: 1250},
			{Service: "api", Name: "shop.v1.ShopService/CreateWidget", Requests: 7, P50Ms: 9.5, P95Ms: 12, P99Ms: 14},
		}, nil
	})
	var out bytes.Buffer
	printUpEndpoints(context.Background(), &out, "dev", upHealthProbes{})
	got := out.String()
	for _, want := range []string{
		"Per-RPC latency and errors over the last hour, no instrumentation code (`" + Name() + " obs endpoints dev` for more):",
		"ENDPOINT", "REQUESTS", "ERRORS", "P50", "P95", "P99",
		"shop.v1.ShopService/ListWidgets", "42", "2.4%", "3.2ms", "18ms", "1.25s",
		"shop.v1.ShopService/CreateWidget",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if n := strings.Count(strings.TrimSpace(got), "\n") + 1; n != 4 {
		t.Errorf("table is %d lines, want heading + header + 2 rows:\n%s", n, got)
	}
}

// Nothing served yet, no telemetry backend, or a query past its budget: one
// line naming the command and what it shows, and never an error.
func TestPrintUpEndpointsFallsBackToOneLine(t *testing.T) {
	prevBudget := upEndpointsBudget
	upEndpointsBudget = 50 * time.Millisecond
	t.Cleanup(func() { upEndpointsBudget = prevBudget })
	for name, q := range map[string]func(context.Context, string) ([]obsEndpointDoc, error){
		"no requests yet": func(context.Context, string) ([]obsEndpointDoc, error) { return nil, nil },
		"no backend": func(context.Context, string) ([]obsEndpointDoc, error) {
			return nil, errors.New("clickstack is not running")
		},
		"slow query": func(ctx context.Context, _ string) ([]obsEndpointDoc, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			stubUpEndpoints(t, q)
			var out bytes.Buffer
			start := time.Now()
			printUpEndpoints(context.Background(), &out, "dev", upHealthProbes{})
			if time.Since(start) > 2*time.Second {
				t.Errorf("took %s, past the budget", time.Since(start))
			}
			got := strings.TrimSpace(out.String())
			if strings.Count(got, "\n") != 0 || !strings.Contains(got, "`"+Name()+" obs endpoints dev` shows p50/p95/p99 and the error rate per RPC") {
				t.Errorf("want the one line, got:\n%s", got)
			}
		})
	}
}

// The real query is `obs endpoints <env> --json` run in-process: its rows
// come back decoded, busiest first, at most upEndpointsTop.
func TestUpEndpointsQueryRunsObsEndpoints(t *testing.T) {
	var asked obsbackend.EndpointsRequest
	old := obsDepsOverride
	t.Cleanup(func() { obsDepsOverride = old })
	obsDepsOverride = &obsDeps{
		Facts: func(context.Context, io.Writer, string) (obsbackend.Facts, error) { return devFacts(), nil },
		Local: func(string) obsbackend.Backend {
			return &obsbackend.MockBackend{EndpointsFunc: func(_ context.Context, req obsbackend.EndpointsRequest) (obsbackend.EndpointsResult, error) {
				asked = req
				return obsbackend.EndpointsResult{Endpoints: []obsbackend.Endpoint{
					{Service: "api", Name: "shop.v1.ShopService/ListWidgets", Requests: 3, P50Ms: 2},
					{Service: "api", Name: "grpc.health.v1.Health/Check", Requests: 1, P50Ms: 0.4},
				}}, nil
			}}
		},
		Now:     func() time.Time { return obsNow },
		Release: func(context.Context, string, time.Time) (*liveRelease, error) { return nil, nil },
	}
	rows, err := upEndpointsQuery(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "shop.v1.ShopService/ListWidgets" || rows[0].Requests != 3 || rows[0].Probe {
		t.Errorf("rows = %+v", rows)
	}
	if len(rows) == 2 && !rows[1].Probe {
		t.Errorf("the health rpc's row is not marked a probe: %+v", rows[1])
	}
	if asked.Top != upEndpointsTop {
		t.Errorf("asked for top %d, want %d", asked.Top, upEndpointsTop)
	}
}

// The first env up of a fresh stack: the health rpc env up sent is the one
// row, labelled as env up's own and as left out of release health. The table
// waits for its span to be stored, within upEndpointsSettle of the probe.
func TestPrintUpEndpointsWaitsForTheHealthProbe(t *testing.T) {
	prevPoll, prevSettle := upEndpointsPoll, upEndpointsSettle
	upEndpointsPoll, upEndpointsSettle = 10*time.Millisecond, 2*time.Second
	t.Cleanup(func() { upEndpointsPoll, upEndpointsSettle = prevPoll, prevSettle })
	calls := 0
	stubUpEndpoints(t, func(context.Context, string) ([]obsEndpointDoc, error) {
		calls++
		if calls < 3 { // not stored yet
			return nil, nil
		}
		return []obsEndpointDoc{{Service: "api", Name: "grpc.health.v1.Health/Check", Requests: 1, P50Ms: 0.4, P95Ms: 0.4, P99Ms: 0.4, Probe: true}}, nil
	})
	var out bytes.Buffer
	printUpEndpoints(context.Background(), &out, "dev", upHealthProbes{at: time.Now(), answered: 1})
	got := out.String()
	for _, want := range []string{
		"grpc.health.v1.Health/Check  (health check sent by env up; release health leaves it out)",
		"Your own RPCs join the table as they serve requests.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table lacks %q:\n%s", want, got)
		}
	}
	if calls != 3 {
		t.Errorf("queried %d times, want until the row appeared (3)", calls)
	}

	// Never stored (a slow pipeline): the line, by the settle deadline.
	upEndpointsSettle = 50 * time.Millisecond
	stubUpEndpoints(t, func(context.Context, string) ([]obsEndpointDoc, error) { return nil, nil })
	out.Reset()
	start := time.Now()
	printUpEndpoints(context.Background(), &out, "dev", upHealthProbes{at: time.Now(), answered: 1})
	if time.Since(start) > time.Second || !strings.Contains(out.String(), "once it has served requests") {
		t.Errorf("after %s: %s", time.Since(start), out.String())
	}
}

// env up calls the health rpc once on every host service with a port, and
// counts the ones that answered; frontends and portless workers get none.
func TestSendUpHealthProbes(t *testing.T) {
	var asked []string
	prev := upHealthProbe
	upHealthProbe = func(_ context.Context, base string) error {
		asked = append(asked, base)
		if strings.Contains(base, "9999") {
			return errors.New("404: an older forge/pkg serves no health rpc")
		}
		return nil
	}
	t.Cleanup(func() { upHealthProbe = prev })
	p := sendUpHealthProbes(context.Background(), []upServiceRow{
		{Name: "api", Kind: "host", URL: "http://localhost:8080", Port: 8080},
		{Name: "old", Kind: "host", Port: 9999},
		{Name: "worker", Kind: "host"},
		{Name: "web", Kind: "frontend", URL: "http://localhost:3000", Port: 3000},
	})
	if strings.Join(asked, ",") != "http://localhost:8080,http://127.0.0.1:9999" {
		t.Errorf("probed %v", asked)
	}
	if p.answered != 1 || p.at.IsZero() {
		t.Errorf("probes = %+v, want 1 answered", p)
	}
}

// The real probe is a Connect unary POST of {} to the health rpc.
func TestUpHealthProbeIsAConnectCall(t *testing.T) {
	var method, path, ctype, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		method, path, ctype, body = r.Method, r.URL.Path, r.Header.Get("Content-Type"), string(b)
		_, _ = w.Write([]byte(`{"status":"SERVING"}`))
	}))
	defer srv.Close()
	if err := upHealthProbe(context.Background(), srv.URL+"/"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/grpc.health.v1.Health/Check" || ctype != "application/json" || body != "{}" {
		t.Errorf("sent %s %s (%s) %q", method, path, ctype, body)
	}
}
