package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestAuditIngress_FeatureOffSkipsCategory pins the additive-extension
// contract: when features.ingress is off (or unset for cli/library
// kinds where it defaults off), the ingress key is absent from the
// report entirely. Sub-agents that branch on `.ingress` get nil and
// know the subsystem isn't in play, without misreading "status: ok"
// as "all routes wired".
func TestAuditIngress_FeatureOffSkipsCategory(t *testing.T) {
	dir := t.TempDir()
	// Ingress is experimental — default off. A forge.yaml with no
	// `features.experimental.ingress` opt-in produces the same
	// no-ingress-category shape as the old explicit-disable case.
	yamlBody := `name: t
module_path: github.com/test/t
version: 0.0.1
forge_version: dev
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatal(err)
	}
	writeComponentsJSON(t, dir)
	report, err := buildAuditReport(dir)
	if err != nil {
		t.Fatalf("buildAuditReport: %v", err)
	}
	if _, ok := report.Categories["ingress"]; ok {
		t.Error("ingress category present despite features.experimental.ingress not opted in")
	}
}

// TestCrossCheckIngress_UnknownService asserts that routes referencing
// a non-existent forge.yaml backend produce an error-level finding and
// flip the category status to error.
func TestCrossCheckIngress_UnknownService(t *testing.T) {
	services := []config.ComponentConfig{
		{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}},
	}
	routes := []HTTPRouteEntity{
		{Name: "api-route", Service: "api", Port: 8080},
		{Name: "ghost-route", Service: "ghost", Port: 9000},
	}
	cat := crossCheckIngress(services, []string{"api"}, nil, routes, nil)
	if cat.Status != AuditStatusError {
		t.Fatalf("status = %q, want error", cat.Status)
	}
	findings, ok := cat.Details["findings"].([]string)
	if !ok {
		t.Fatalf("findings missing/wrong type: %T", cat.Details["findings"])
	}
	var sawErr bool
	for _, f := range findings {
		if strings.HasPrefix(f, "error: ") && strings.Contains(f, "ghost-route") && strings.Contains(f, "ghost") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("expected error finding for ghost-route -> ghost; got %v", findings)
	}
}

// TestCrossCheckIngress_ServiceWithoutRoute asserts that a service
// with Port>0 but no matching route produces an info-level finding and
// keeps status at ok (cluster-internal services are valid).
func TestCrossCheckIngress_ServiceWithoutRoute(t *testing.T) {
	services := []config.ComponentConfig{
		{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}},
		{Name: "internal-only", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 9000}}},
	}
	routes := []HTTPRouteEntity{
		{Name: "api-route", Service: "api", Port: 8080},
	}
	cat := crossCheckIngress(services, []string{"api", "internal-only"}, nil, routes, nil)
	if cat.Status != AuditStatusOK {
		t.Fatalf("status = %q, want ok (info findings shouldn't downgrade)", cat.Status)
	}
	findings, _ := cat.Details["findings"].([]string)
	var sawInfo bool
	for _, f := range findings {
		if strings.HasPrefix(f, "info: ") && strings.Contains(f, "internal-only") && strings.Contains(f, ":9000") {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Errorf("expected info finding for internal-only; got %v", findings)
	}
	if got, _ := cat.Details["services_without_route"].(int); got != 1 {
		t.Errorf("services_without_route = %v, want 1", cat.Details["services_without_route"])
	}
}

// TestCrossCheckIngress_PortZeroSkipped asserts that services with no
// declared port (Port==0) don't generate "no route" info lines —
// workers/operators that just consume the bus shouldn't show up here.
func TestCrossCheckIngress_PortZeroSkipped(t *testing.T) {
	services := []config.ComponentConfig{
		{Name: "worker"},
	}
	cat := crossCheckIngress(services, []string{"worker"}, nil, nil, nil)
	if cat.Status != AuditStatusOK {
		t.Errorf("status = %q, want ok", cat.Status)
	}
	if _, ok := cat.Details["findings"]; ok {
		t.Errorf("unexpected findings for port-zero service: %v", cat.Details["findings"])
	}
}

// TestCrossCheckIngress_GRPCRoutesAlsoChecked asserts the GRPCRoute
// branch is wired symmetrically with HTTPRoute — unknown svc refs in
// a GRPCRoute escalate to error too.
func TestCrossCheckIngress_GRPCRoutesAlsoChecked(t *testing.T) {
	services := []config.ComponentConfig{{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}}}
	grpc := []GRPCRouteEntity{
		{Name: "grpc-ghost", Service: "ghost", Port: 9000},
	}
	cat := crossCheckIngress(services, []string{"api"}, nil, nil, grpc)
	if cat.Status != AuditStatusError {
		t.Errorf("status = %q, want error", cat.Status)
	}
}

// TestAuditIngress_KCLRenderFailureWarn confirms that when RenderKCL
// itself fails (no kcl on PATH / no deploy/kcl/dev), the category
// degrades to warn — audit must keep running in CI environments that
// don't ship the kcl toolchain.
func TestAuditIngress_KCLRenderFailureWarn(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name: "t",
		Features: config.FeaturesConfig{
			Experimental: config.ExperimentalConfig{Ingress: true},
		},
	}
	cat := auditIngress(cfg, dir) // no deploy/kcl/dev → RenderKCL errors
	if cat.Status != AuditStatusWarn {
		t.Errorf("status = %q, want warn (no dev KCL)", cat.Status)
	}
	if !strings.Contains(cat.Summary, "could not evaluate dev KCL") {
		t.Errorf("summary should mention dev KCL failure; got %q", cat.Summary)
	}
}

// TestCrossCheckIngress_SummaryFormat pins the summary shape so
// downstream consumers (CI dashboards, doctor output) get a stable
// human-readable string format. Three slots: gateway count, route
// count (http+grpc), services-without-route count.
func TestCrossCheckIngress_SummaryFormat(t *testing.T) {
	services := []config.ComponentConfig{
		{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}},
		{Name: "internal", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 9000}}},
	}
	gws := []GatewayEntity{{Name: "edge"}}
	http := []HTTPRouteEntity{{Name: "api-route", Service: "api", Port: 8080}}
	grpc := []GRPCRouteEntity{{Name: "api-grpc", Service: "api", Port: 9090}}

	cat := crossCheckIngress(services, []string{"api", "internal"}, gws, http, grpc)
	if !strings.Contains(cat.Summary, "1 gateway") {
		t.Errorf("summary should mention 1 gateway; got %q", cat.Summary)
	}
	if !strings.Contains(cat.Summary, "2 route") {
		t.Errorf("summary should mention 2 routes (http+grpc); got %q", cat.Summary)
	}
	if !strings.Contains(cat.Summary, "1 service") {
		t.Errorf("summary should mention 1 service-without-route; got %q", cat.Summary)
	}
}

// TestCrossCheckIngress_FrontendBackend asserts a route pointing at a
// frontend by name passes the cross-check — frontends are valid K8s
// Service targets too. The "port declared but no route" info finding
// stays scoped to cfg.Components, so an SSR-only frontend doesn't get
// flagged as a gap.
func TestCrossCheckIngress_FrontendBackend(t *testing.T) {
	services := []config.ComponentConfig{}
	backends := []string{"web"}
	routes := []HTTPRouteEntity{
		{Name: "web-route", Service: "web", Port: 3000},
	}
	cat := crossCheckIngress(services, backends, nil, routes, nil)
	if cat.Status != AuditStatusOK {
		t.Fatalf("status = %q, want ok (frontend is a valid backend)", cat.Status)
	}
	if findings, ok := cat.Details["findings"].([]string); ok {
		for _, f := range findings {
			if strings.HasPrefix(f, "error: ") {
				t.Errorf("unexpected error finding: %q", f)
			}
		}
	}
}

// TestCrossCheckIngress_WebhookBackend asserts a route pointing at a
// webhook handler name (declared under a service's webhooks: block)
// passes the cross-check — at the k8s layer a webhook handler is just
// another Service in the namespace.
func TestCrossCheckIngress_WebhookBackend(t *testing.T) {
	services := []config.ComponentConfig{{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}}}
	backends := []string{"api", "stripe"}
	routes := []HTTPRouteEntity{
		{Name: "stripe-webhook", Service: "stripe", Port: 8080},
	}
	cat := crossCheckIngress(services, backends, nil, routes, nil)
	if cat.Status != AuditStatusOK {
		t.Fatalf("status = %q, want ok (webhook is a valid backend)", cat.Status)
	}
	if findings, ok := cat.Details["findings"].([]string); ok {
		for _, f := range findings {
			if strings.HasPrefix(f, "error: ") {
				t.Errorf("unexpected error finding: %q", f)
			}
		}
	}
}

// TestCrossCheckIngress_UnknownNonBackend asserts the negative case:
// a route pointing at a name that's neither service, frontend, nor
// webhook still flips the category to error.
func TestCrossCheckIngress_UnknownNonBackend(t *testing.T) {
	services := []config.ComponentConfig{{Name: "api", Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: 8080}}}}
	backends := []string{"api", "web", "stripe"}
	routes := []HTTPRouteEntity{
		{Name: "ghost-route", Service: "nobody", Port: 8080},
	}
	cat := crossCheckIngress(services, backends, nil, routes, nil)
	if cat.Status != AuditStatusError {
		t.Fatalf("status = %q, want error (unknown backend)", cat.Status)
	}
}

// TestIngressBackendNames covers the union built in auditIngress —
// services, per-service webhook handlers, and frontends all surface in
// the resulting set. Order doesn't matter, just membership.
func TestIngressBackendNames(t *testing.T) {
	cfg := &config.ProjectConfig{
		Components: []config.ComponentConfig{
			{Name: "api", Webhooks: []config.WebhookConfig{{Name: "stripe"}, {Name: "github"}}},
			{Name: "worker"},
		},
		Frontends: []config.FrontendConfig{
			{Name: "web"},
			{Name: "admin"},
		},
	}
	got := ingressBackendNames(cfg)
	want := map[string]bool{"api": true, "worker": true, "stripe": true, "github": true, "web": true, "admin": true}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for k := range want {
		if !seen[k] {
			t.Errorf("missing backend %q in %v", k, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d backends, want %d: %v", len(got), len(want), got)
	}
}
