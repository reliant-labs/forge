package cli

import (
	"os"
	"sort"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/devproxy"
)

// TestResolveProxyPort exercises the three-tier precedence: explicit
// flag > FORGE_RUN_PROXY_PORT > [defaultDevProxyPort]. The flag is
// the strongest signal; the env is the user-shell escape hatch; the
// constant is the predictable default.
func TestResolveProxyPort(t *testing.T) {
	t.Setenv("FORGE_RUN_PROXY_PORT", "")

	if got := resolveProxyPort(0); got != defaultDevProxyPort {
		t.Errorf("default: got %d, want %d", got, defaultDevProxyPort)
	}
	if got := resolveProxyPort(9090); got != 9090 {
		t.Errorf("flag wins: got %d, want 9090", got)
	}

	t.Setenv("FORGE_RUN_PROXY_PORT", "7777")
	if got := resolveProxyPort(0); got != 7777 {
		t.Errorf("env wins over default: got %d, want 7777", got)
	}
	if got := resolveProxyPort(9090); got != 9090 {
		t.Errorf("flag wins over env: got %d, want 9090", got)
	}

	t.Setenv("FORGE_RUN_PROXY_PORT", "not-a-port")
	if got := resolveProxyPort(0); got != defaultDevProxyPort {
		t.Errorf("malformed env falls back to default: got %d, want %d", got, defaultDevProxyPort)
	}

	// Negative env value treated as unset (matches strconv.Atoi
	// semantics: it parses, but the > 0 guard rejects it).
	t.Setenv("FORGE_RUN_PROXY_PORT", "-1")
	if got := resolveProxyPort(0); got != defaultDevProxyPort {
		t.Errorf("negative env falls back to default: got %d, want %d", got, defaultDevProxyPort)
	}

	_ = os.Unsetenv("FORGE_RUN_PROXY_PORT")
}

// TestBuildDevProxyBackends covers the dispatch-table composition:
//   - every frontend gets a default `<name>.localhost` entry
//   - HTTPRoute hosts add additional entries for services
//   - HTTPRoute hosts deduplicate against the frontend default
//   - HTTPRoutes for unknown services are skipped silently
//   - frontends with Port == 0 are skipped (legacy projects)
//   - root-path / no-host routes are skipped (path-prefix shape)
func TestBuildDevProxyBackends(t *testing.T) {
	frontends := []config.FrontendConfig{
		{Name: "admin", Port: 3000},
		{Name: "web", Port: 3001},
		{Name: "legacy", Port: 0}, // skipped
	}
	services := []config.ServiceConfig{
		{Name: "api", Port: 8000},
		{Name: "worker", Port: 0}, // skipped (no HTTP port)
	}
	routes := []HTTPRouteEntity{
		{Name: "api-route", Service: "api", Port: 8000, Host: "api.localhost"},
		{Name: "admin-alt", Service: "admin", Port: 3000, Host: "admin-alt.localhost"},
		{Name: "duplicate", Service: "admin", Port: 3000, Host: "admin.localhost"}, // already added
		{Name: "ghost", Service: "does-not-exist", Port: 9999, Host: "ghost.localhost"},
		{Name: "no-host", Service: "api", Port: 8000, Host: ""}, // skipped (path-prefix)
	}

	got := buildDevProxyBackends(frontends, services, routes)

	// Sort by host so the assertion is order-independent.
	sort.Slice(got, func(i, j int) bool { return got[i].Host < got[j].Host })

	want := []devproxy.Backend{
		{Host: "admin-alt.localhost", Port: 3000, Kind: "frontend", Name: "admin"},
		{Host: "admin.localhost", Port: 3000, Kind: "frontend", Name: "admin"},
		{Host: "api.localhost", Port: 8000, Kind: "service", Name: "api"},
		{Host: "web.localhost", Port: 3001, Kind: "frontend", Name: "web"},
	}

	if len(got) != len(want) {
		t.Fatalf("got %d backends, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("backend[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestBuildDevProxyBackends_FrontendlessServicesOnly confirms a
// services-only project (no frontends) gets a usable dispatch table
// from HTTPRoute hosts alone.
func TestBuildDevProxyBackends_FrontendlessServicesOnly(t *testing.T) {
	services := []config.ServiceConfig{
		{Name: "api", Port: 8000},
	}
	routes := []HTTPRouteEntity{
		{Name: "api-route", Service: "api", Port: 8000, Host: "api.localhost"},
	}
	got := buildDevProxyBackends(nil, services, routes)
	if len(got) != 1 {
		t.Fatalf("got %d backends, want 1: %+v", len(got), got)
	}
	if got[0].Host != "api.localhost" || got[0].Port != 8000 || got[0].Kind != "service" {
		t.Errorf("backend = %+v, want api.localhost:8000 kind=service", got[0])
	}
}
