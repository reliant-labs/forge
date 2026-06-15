package cli

import (
	"os"
	"sort"
	"strings"
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
	none := map[int]string{}

	mustResolve := func(flagPort int, declared map[int]string) int {
		t.Helper()
		got, err := resolveProxyPort(flagPort, declared)
		if err != nil {
			t.Fatalf("resolveProxyPort(%d) error: %v", flagPort, err)
		}
		return got
	}

	if got := mustResolve(0, none); got != defaultDevProxyPort {
		t.Errorf("default: got %d, want %d", got, defaultDevProxyPort)
	}
	if got := mustResolve(9090, none); got != 9090 {
		t.Errorf("flag wins: got %d, want 9090", got)
	}

	t.Setenv("FORGE_RUN_PROXY_PORT", "7777")
	if got := mustResolve(0, none); got != 7777 {
		t.Errorf("env wins over default: got %d, want 7777", got)
	}
	if got := mustResolve(9090, none); got != 9090 {
		t.Errorf("flag wins over env: got %d, want 9090", got)
	}

	t.Setenv("FORGE_RUN_PROXY_PORT", "not-a-port")
	if got := mustResolve(0, none); got != defaultDevProxyPort {
		t.Errorf("malformed env falls back to default: got %d, want %d", got, defaultDevProxyPort)
	}

	// Negative env value treated as unset (matches strconv.Atoi
	// semantics: it parses, but the > 0 guard rejects it).
	t.Setenv("FORGE_RUN_PROXY_PORT", "-1")
	if got := mustResolve(0, none); got != defaultDevProxyPort {
		t.Errorf("negative env falls back to default: got %d, want %d", got, defaultDevProxyPort)
	}

	_ = os.Unsetenv("FORGE_RUN_PROXY_PORT")
}

// The proxy must never share a port with a component it fronts. The
// first scaffolded service defaults to 8080 — the same number as
// defaultDevProxyPort — and the split-brain that follows (proxy on
// 127.0.0.1:8080, service wildcard-bound on the same port, Chrome's
// localhost resolving to ::1 and hitting the raw API) broke every
// advertised URL in journey fr-5b2121e48f. Default: silently skip past
// declared ports. Explicit flag/env: fail loudly — the user asked for
// a guaranteed split-brain.
func TestResolveProxyPort_AvoidsDeclaredComponentPorts(t *testing.T) {
	t.Setenv("FORGE_RUN_PROXY_PORT", "")
	declared := map[int]string{
		8080: "service api",
		8081: "service worker",
	}

	got, err := resolveProxyPort(0, declared)
	if err != nil {
		t.Fatalf("default with declared ports: %v", err)
	}
	if got != 8082 {
		t.Errorf("default skips declared ports: got %d, want 8082", got)
	}

	if _, err := resolveProxyPort(8080, declared); err == nil {
		t.Error("explicit --proxy-port onto a service port must error")
	} else if !strings.Contains(err.Error(), "service api") {
		t.Errorf("error must name the conflicting component: %v", err)
	}

	t.Setenv("FORGE_RUN_PROXY_PORT", "8081")
	if _, err := resolveProxyPort(0, declared); err == nil {
		t.Error("explicit FORGE_RUN_PROXY_PORT onto a service port must error")
	} else if !strings.Contains(err.Error(), "service worker") {
		t.Errorf("error must name the conflicting component: %v", err)
	}
	_ = os.Unsetenv("FORGE_RUN_PROXY_PORT")
}

// declaredComponentPorts feeds the proxy's overlap avoidance: every
// port a service or frontend will bind, mapped to a human name for the
// conflict error.
func TestDeclaredComponentPorts(t *testing.T) {
	t.Parallel()
	got := declaredComponentPorts(
		[]config.ComponentConfig{
			{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}},
			{Name: "grpc"}, // portless: skipped
		},
		[]config.FrontendConfig{
			{Name: "web", Port: 3000},
			{Name: "legacy", Port: 0}, // portless: skipped
		},
	)
	want := map[int]string{
		8080: "service api",
		3000: "frontend web",
	}
	if len(got) != len(want) {
		t.Fatalf("declaredComponentPorts = %v, want %v", got, want)
	}
	for port, name := range want {
		if got[port] != name {
			t.Errorf("port %d = %q, want %q", port, got[port], name)
		}
	}
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
	services := []config.ComponentConfig{
		{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8000}}},
		{Name: "worker"}, // skipped (no HTTP port)
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
	services := []config.ComponentConfig{
		{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8000}}},
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
