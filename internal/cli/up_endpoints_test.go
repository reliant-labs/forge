package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	printUpEndpoints(context.Background(), &out, "dev")
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
			printUpEndpoints(context.Background(), &out, "dev")
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
				return obsbackend.EndpointsResult{Endpoints: []obsbackend.Endpoint{{Service: "api", Name: "shop.v1.ShopService/ListWidgets", Requests: 3, P50Ms: 2}}}, nil
			}}
		},
		Now:     func() time.Time { return obsNow },
		Release: func(context.Context, string, time.Time) (*liveRelease, error) { return nil, nil },
	}
	rows, err := upEndpointsQuery(context.Background(), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "shop.v1.ShopService/ListWidgets" || rows[0].Requests != 3 {
		t.Errorf("rows = %+v", rows)
	}
	if asked.Top != upEndpointsTop {
		t.Errorf("asked for top %d, want %d", asked.Top, upEndpointsTop)
	}
}
