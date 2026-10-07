package doctor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGrafana answers the Prometheus datasource-proxy queries CheckPrometheus
// makes. `up` always has the lgtm collector's own target — the bundled image
// scrapes itself — and appSeries is what the app has exported.
func fakeGrafana(t *testing.T, appSeries string) *Environment {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/datasources/proxy/uid/prometheus/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		switch q := r.URL.Query().Get("query"); {
		case q == "up":
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"__name__":"up","job":"otelcol-contrib"},"value":[1,"1"]}]}}`))
		case strings.Contains(q, `job="shop"`):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` + appSeries + `]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	t.Cleanup(srv.Close)
	env := &Environment{ProjectName: "shop"}
	env.SetPort("lgtm", 3000, strings.TrimPrefix(srv.URL, "http://"))
	return env
}

// The false green the media capture hit: Prometheus "✓ 1 targets up" while
// the app exported nothing — the one up target is lgtm scraping its own
// collector. A check that cannot see the app's metrics must not pass.
func TestCheckPrometheus_CollectorAloneIsNotAPass(t *testing.T) {
	got := CheckPrometheus(context.Background(), fakeGrafana(t, ""))
	if got.Status == StatusPass {
		t.Fatalf("Prometheus passed with no metrics from the app: %s", got.Message)
	}
	if got.Status != StatusWarn || !strings.Contains(got.Message, "no metrics from shop") {
		t.Fatalf("want a warning naming the missing app metrics, got %s: %s", got.Status, got.Message)
	}
}

func TestCheckPrometheus_PassesOnTheAppsOwnSeries(t *testing.T) {
	got := CheckPrometheus(context.Background(), fakeGrafana(t,
		`{"metric":{"__name__":"rpc_server_call_duration_seconds_count"},"value":[1,"4"]},`+
			`{"metric":{"__name__":"go_sql_connections_open"},"value":[1,"1"]}`))
	if got.Status != StatusPass {
		t.Fatalf("want pass, got %s: %s", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "rpc_server_call_duration_seconds_count") {
		t.Fatalf("the pass should name what it found: %s", got.Message)
	}
}
