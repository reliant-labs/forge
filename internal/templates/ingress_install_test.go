package templates_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
	"gopkg.in/yaml.v3"
)

// entrypoint matches the shape internal/cli's traefikEntrypoint
// passes to the template. We redeclare it here (with the same
// exported field names) to keep this test free of an internal/cli
// dependency.
type entrypoint struct {
	Name     string
	Port     int
	Protocol string
}

// renderTraefikInstall executes the per-project Traefik install
// template against the given entrypoints and returns the YAML bytes
// the install path would feed to kubectl. Mirrors what
// internal/cli/dev_cluster_ingress.go does at `forge dev cluster up`
// time so this test exercises the same rendering path.
func renderTraefikInstall(t *testing.T, entrypoints []entrypoint) []byte {
	t.Helper()
	out, err := templates.IngressTemplates().Render("traefik/traefik.yaml.tmpl", struct {
		Entrypoints []entrypoint
	}{Entrypoints: entrypoints})
	if err != nil {
		t.Fatalf("render traefik.yaml.tmpl: %v", err)
	}
	return out
}

// TestTraefikInstallNoExperimentalChannel guards against regressions in the
// vendored Traefik install template at internal/templates/ingress/traefik/traefik.yaml.tmpl.
//
// Traefik v3.2.1 with --providers.kubernetesgateway.experimentalchannel=true
// attempts to watch TCPRoute/TLSRoute/UDPRoute/BackendTLSPolicy CRDs that
// forge does not install (forge ships only the gateway-api v1.2.0 standard
// channel). Result: Gateway/public never reaches Programmed=True. This test
// pins the install to the standard channel and ensures the ClusterRole has
// the permissions Traefik actually needs (configmaps) but no permissions for
// experimental-only resources (udproutes).
//
// We render with zero entrypoints — the static parts of the install (RBAC,
// the controller flag set, the ping containerPort) must be intact regardless
// of the per-project listener set.
func TestTraefikInstallNoExperimentalChannel(t *testing.T) {
	raw := renderTraefikInstall(t, nil)
	docs := parseDocs(t, raw)
	if len(docs) == 0 {
		t.Fatal("expected at least one YAML document in rendered traefik install")
	}

	deployment := findResource(t, docs, "Deployment", "traefik")
	clusterRole := findResource(t, docs, "ClusterRole", "traefik")

	// 1. Deployment args must not contain experimentalchannel.
	args := containerArgs(t, deployment)
	if len(args) == 0 {
		t.Fatal("Deployment traefik: no container args found")
	}
	for _, a := range args {
		if strings.Contains(a, "experimentalchannel") {
			t.Errorf("Deployment arg %q references experimental channel; forge ships standard-channel Gateway API CRDs only", a)
		}
	}
	// Sanity: the kubernetesgateway provider must still be enabled.
	if !containsArg(args, "--providers.kubernetesgateway=true") {
		t.Errorf("Deployment args missing --providers.kubernetesgateway=true; got %v", args)
	}

	// 2. ClusterRole must grant configmaps; Traefik logs `configmaps is
	//    forbidden` without it and never finishes reconciling.
	if !clusterRoleHasResource(t, clusterRole, "", "configmaps") {
		t.Error("ClusterRole traefik: rule for core API group must include `configmaps`")
	}

	// 3. ClusterRole must NOT include experimental-only resources.
	//    udproutes is experimental in gateway-api v1.2.
	if clusterRoleHasResource(t, clusterRole, "gateway.networking.k8s.io", "udproutes") {
		t.Error("ClusterRole traefik: `udproutes` is experimental-channel only and must not appear in rules")
	}
	if clusterRoleHasResource(t, clusterRole, "gateway.networking.k8s.io", "udproutes/status") {
		t.Error("ClusterRole traefik: `udproutes/status` is experimental-channel only and must not appear in rules")
	}
}

// TestTraefikInstallEntrypointsFromListeners verifies the per-listener
// projection: given two listeners (http on 18080, grpc on 19190), the
// rendered Deployment carries one --entrypoints.<name>.address=:<port>
// arg and one containerPort per listener, AND the Service exposes one
// port per listener.
//
// This is the bug the smoke test caught — Traefik v3.2's
// kubernetesgateway provider does not dynamically create listener
// sockets from Gateway.spec.listeners[*].port, so Gateway reconciles
// to ListenersNotValid ("no matching entryPoint for port 28080") and
// Traefik binds nothing on the listener ports.
func TestTraefikInstallEntrypointsFromListeners(t *testing.T) {
	raw := renderTraefikInstall(t, []entrypoint{
		{Name: "http", Port: 18080, Protocol: "HTTP"},
		{Name: "grpc", Port: 19190, Protocol: "H2C"},
	})
	docs := parseDocs(t, raw)
	deployment := findResource(t, docs, "Deployment", "traefik")
	service := findResource(t, docs, "Service", "traefik")

	// 1. Entrypoint args present.
	args := containerArgs(t, deployment)
	wantArgs := []string{
		"--entrypoints.http.address=:18080",
		"--entrypoints.grpc.address=:19190",
	}
	for _, w := range wantArgs {
		if !containsArg(args, w) {
			t.Errorf("Deployment args missing %q; got %v", w, args)
		}
	}

	// 2. Container ports present (in addition to the ping port).
	cports := containerPorts(t, deployment)
	if !containerPortPresent(cports, 18080, "http") {
		t.Errorf("Deployment containerPorts missing listener http:18080; got %v", cports)
	}
	if !containerPortPresent(cports, 19190, "grpc") {
		t.Errorf("Deployment containerPorts missing listener grpc:19190; got %v", cports)
	}
	if !containerPortPresent(cports, 8080, "ping") {
		t.Errorf("Deployment containerPorts missing baseline ping:8080; got %v", cports)
	}

	// 2b. Entrypoint containerPorts MUST set hostPort to the same value.
	//     Without hostPort, the k3d serverlb nginx forwards host:<port>
	//     -> node:<port> but nothing on the node listens there (Traefik
	//     is a ClusterIP Service that the LB can't see). Symptom: curl
	//     returns "empty reply from server" / HTTP 000. hostPort makes
	//     the Traefik pod the node-level listener for each entrypoint.
	//     This MUST hold for entrypoint ports (not the baseline ping,
	//     which is in-cluster only).
	if !containerPortHasHostPort(cports, 18080) {
		t.Errorf("Deployment containerPort http:18080 must set hostPort=18080 (k3d serverlb -> node:port -> pod requires it); got %v", cports)
	}
	if !containerPortHasHostPort(cports, 19190) {
		t.Errorf("Deployment containerPort grpc:19190 must set hostPort=19190 (k3d serverlb -> node:port -> pod requires it); got %v", cports)
	}
	// And: ping deliberately does NOT set hostPort (it's in-cluster only).
	if pingHasHostPort(cports) {
		t.Errorf("baseline ping containerPort must NOT set hostPort (in-cluster only); got %v", cports)
	}

	// 3. Service ports present (in addition to the ping port). The
	//    k3d loadbalancer's TCP forward lands here — without these
	//    entries curl returns HTTP 000.
	sports := servicePorts(t, service)
	if !servicePortPresent(sports, 18080, "http", "http") {
		t.Errorf("Service ports missing listener http:18080; got %v", sports)
	}
	if !servicePortPresent(sports, 19190, "grpc", "grpc") {
		t.Errorf("Service ports missing listener grpc:19190; got %v", sports)
	}
	if !servicePortPresent(sports, 8080, "ping", "ping") {
		t.Errorf("Service ports missing baseline ping:8080; got %v", sports)
	}
}

// TestTraefikInstallEmptyEntrypointsRendersValidYAML guards the
// no-listener case: when the project has features.ingress on but
// hasn't authored any gateways yet, the install template must still
// produce parseable YAML with the static resources intact.
func TestTraefikInstallEmptyEntrypointsRendersValidYAML(t *testing.T) {
	raw := renderTraefikInstall(t, nil)
	docs := parseDocs(t, raw)

	// Same baseline as the no-experimental-channel test: Deployment +
	// Service exist with the ping port only, and the controller arg
	// set is the static defaults.
	deployment := findResource(t, docs, "Deployment", "traefik")
	service := findResource(t, docs, "Service", "traefik")

	args := containerArgs(t, deployment)
	for _, a := range args {
		if strings.HasPrefix(a, "--entrypoints.") {
			t.Errorf("zero-entrypoint render emitted entrypoint arg %q; expected none", a)
		}
	}

	cports := containerPorts(t, deployment)
	if len(cports) != 1 {
		t.Errorf("zero-entrypoint render: expected 1 containerPort (ping), got %d: %v", len(cports), cports)
	}
	sports := servicePorts(t, service)
	if len(sports) != 1 {
		t.Errorf("zero-entrypoint render: expected 1 Service port (ping), got %d: %v", len(sports), sports)
	}
}

// parseDocs decodes a multi-doc YAML stream into a slice of maps.
func parseDocs(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("yaml parse: %v", err)
		}
		if doc != nil {
			docs = append(docs, doc)
		}
	}
	return docs
}

// findResource returns the YAML doc whose kind+metadata.name match.
func findResource(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		k, _ := d["kind"].(string)
		if k != kind {
			continue
		}
		md, _ := d["metadata"].(map[string]any)
		n, _ := md["name"].(string)
		if n == name {
			return d
		}
	}
	t.Fatalf("could not find %s/%s in traefik.yaml", kind, name)
	return nil
}

// containerArgs pulls the args list from spec.template.spec.containers[0].
func containerArgs(t *testing.T, deployment map[string]any) []string {
	t.Helper()
	c0 := firstContainer(t, deployment)
	rawArgs, _ := c0["args"].([]any)
	args := make([]string, 0, len(rawArgs))
	for _, a := range rawArgs {
		if s, ok := a.(string); ok {
			args = append(args, s)
		}
	}
	return args
}

// containerPortEntry is a denormalized container ports[] entry.
type containerPortEntry struct {
	Name     string
	Port     int
	HostPort int // 0 when unset
}

// containerPorts pulls the ports list from spec.template.spec.containers[0].
func containerPorts(t *testing.T, deployment map[string]any) []containerPortEntry {
	t.Helper()
	c0 := firstContainer(t, deployment)
	raw, _ := c0["ports"].([]any)
	out := make([]containerPortEntry, 0, len(raw))
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		port, _ := m["containerPort"].(int)
		// hostPort is optional; absent means 0.
		hostPort, _ := m["hostPort"].(int)
		out = append(out, containerPortEntry{Name: name, Port: port, HostPort: hostPort})
	}
	return out
}

func containerPortPresent(ports []containerPortEntry, port int, name string) bool {
	for _, p := range ports {
		if p.Port == port && p.Name == name {
			return true
		}
	}
	return false
}

// containerPortHasHostPort reports whether the entry for `port` declares
// the same value as its hostPort. Required for k3d serverlb -> node
// -> Traefik pod path (see the comment in the install template).
func containerPortHasHostPort(ports []containerPortEntry, port int) bool {
	for _, p := range ports {
		if p.Port == port {
			return p.HostPort == port
		}
	}
	return false
}

// pingHasHostPort reports whether the ping containerPort accidentally
// got a hostPort. ping is in-cluster only — exposing it on the host is
// not the intent and would collide with the user's host port choices.
func pingHasHostPort(ports []containerPortEntry) bool {
	for _, p := range ports {
		if p.Name == "ping" && p.HostPort != 0 {
			return true
		}
	}
	return false
}

// servicePortEntry is a denormalized service spec.ports[] entry.
type servicePortEntry struct {
	Name       string
	Port       int
	TargetPort string
}

// servicePorts pulls spec.ports from a Service doc.
func servicePorts(t *testing.T, service map[string]any) []servicePortEntry {
	t.Helper()
	spec, _ := service["spec"].(map[string]any)
	raw, _ := spec["ports"].([]any)
	out := make([]servicePortEntry, 0, len(raw))
	for _, p := range raw {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		port, _ := m["port"].(int)
		// targetPort can be int or string in k8s; the template only
		// emits the string form (matching a named containerPort).
		tp, _ := m["targetPort"].(string)
		out = append(out, servicePortEntry{Name: name, Port: port, TargetPort: tp})
	}
	return out
}

func servicePortPresent(ports []servicePortEntry, port int, name, targetPort string) bool {
	for _, p := range ports {
		if p.Port == port && p.Name == name && p.TargetPort == targetPort {
			return true
		}
	}
	return false
}

func firstContainer(t *testing.T, deployment map[string]any) map[string]any {
	t.Helper()
	spec, _ := deployment["spec"].(map[string]any)
	tmpl, _ := spec["template"].(map[string]any)
	tspec, _ := tmpl["spec"].(map[string]any)
	containers, _ := tspec["containers"].([]any)
	if len(containers) == 0 {
		t.Fatal("Deployment has no containers")
	}
	c0, _ := containers[0].(map[string]any)
	return c0
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// clusterRoleHasResource reports whether any rule in the ClusterRole matches
// the given apiGroup and resource.
func clusterRoleHasResource(t *testing.T, cr map[string]any, apiGroup, resource string) bool {
	t.Helper()
	rules, _ := cr["rules"].([]any)
	for _, r := range rules {
		rule, _ := r.(map[string]any)
		groups, _ := rule["apiGroups"].([]any)
		if !containsAny(groups, apiGroup) {
			continue
		}
		resources, _ := rule["resources"].([]any)
		if containsAny(resources, resource) {
			return true
		}
	}
	return false
}

func containsAny(xs []any, want string) bool {
	for _, x := range xs {
		if s, ok := x.(string); ok && s == want {
			return true
		}
	}
	return false
}
