// Package cli — `forge cluster up` ingress install plumbing.
//
// Three pieces, run in order after the k3d cluster is created and
// kubectl context pinned:
//
//  1. Fetch + apply the upstream Gateway API CRDs (version pinned via
//     internal/templates/ingress/traefik/VERSION). Cached under
//     ~/.cache/forge/ingress/ so subsequent cluster-up runs are
//     offline-capable.
//  2. Render the vendored Traefik controller install template
//     (internal/templates/ingress/traefik/traefik.yaml.tmpl) with
//     one --entrypoints.<name>.address arg, one containerPort, and
//     one Service port per dev Gateway listener; apply the result.
//     Traefik v3.2's kubernetesgateway provider does NOT dynamically
//     create listener sockets from Gateway.spec.listeners[*].port —
//     each port needs a matching static entrypoint declared at
//     install time. Re-run `forge cluster up` after adding or
//     removing a listener to install the new entrypoints
//     (idempotent — the rendered Deployment restarts on apply).
//  3. Apply the vendored `traefik` GatewayClass.
//
// Idempotency comes from `kubectl apply` semantics — re-running these
// steps against a cluster that already has them noops at the API
// server level. We do block on CRD establishment between (1) and (3)
// so the GatewayClass apply doesn't race the CRD install.
//
// Also: k3d config merging — `forge generate` writes
// `deploy/k3d-ports.yaml` derived from the dev env's KCL gateway
// listeners. At cluster-up time we read deploy/k3d.yaml +
// deploy/k3d-ports.yaml, merge their ports blocks in memory, and
// hand a temp file to `k3d cluster create`. Keeps deploy/k3d.yaml
// user-owned while keeping host ports in lockstep with the project's
// declared Gateway listeners.
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/templates"
)

// ingressPinnedVersions reads internal/templates/ingress/traefik/VERSION
// and returns (traefikVersion, gatewayAPIVersion). Format:
//
//	traefik=v3.2.1
//	gateway_api=v1.2.0
func ingressPinnedVersions() (traefikVer, gatewayAPIVer string, err error) {
	b, err := templates.IngressTemplates().Get("traefik/VERSION")
	if err != nil {
		return "", "", fmt.Errorf("read pinned VERSION: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(k) {
		case "traefik":
			traefikVer = strings.TrimSpace(v)
		case "gateway_api":
			gatewayAPIVer = strings.TrimSpace(v)
		}
	}
	if traefikVer == "" || gatewayAPIVer == "" {
		return "", "", fmt.Errorf("VERSION missing one of traefik=/gateway_api= keys")
	}
	return traefikVer, gatewayAPIVer, nil
}

// gatewayAPICRDsURL builds the upstream release URL for the standard
// channel CRDs. Pinned to the version from VERSION.
func gatewayAPICRDsURL(version string) string {
	return "https://github.com/kubernetes-sigs/gateway-api/releases/download/" + version + "/standard-install.yaml"
}

// ingressCacheDir is the local cache root for downloaded ingress
// assets. Falls back to a tempdir if $HOME isn't available — that's
// fine, the URL gets re-fetched on next cluster-up.
func ingressCacheDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return os.TempDir(), nil
	}
	return filepath.Join(home, ".cache", "forge", "ingress"), nil
}

// fetchGatewayAPICRDs ensures the pinned-version CRD YAML is on disk
// at the cache path and returns the path. Re-downloads when the file
// is missing; trusts the version-pinned filename for cache busting
// (a forge upgrade changes the VERSION file, the new release URL
// hashes to a different filename, the old cache file stays around).
func fetchGatewayAPICRDs(ctx context.Context, version string) (string, error) {
	dir, err := ingressCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}
	cachePath := filepath.Join(dir, "gateway-api-crds-"+version+".yaml")
	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, nil
	}

	url := gatewayAPICRDsURL(version)
	fmt.Printf("Fetching Gateway API CRDs %s from upstream...\n", version)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	httpClient := &http.Client{Timeout: 60 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, "gateway-api-crds-*.yaml.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // best-effort cleanup if rename below fails
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write cache: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), cachePath); err != nil {
		return "", fmt.Errorf("install cache: %w", err)
	}
	return cachePath, nil
}

// kubeContextArgs returns the `--context <kctx>` prefix for a kubectl
// invocation, or an empty slice when kctx is "". An empty context means
// "use whatever context the caller pinned via pinKubectlContext" — the
// dev `forge cluster up` path. The declared-cluster path
// (reconcileDeclaredClusters) passes an explicit `k3d-<name>` context so
// the install targets THAT cluster regardless of the active context.
func kubeContextArgs(kctx string) []string {
	if kctx == "" {
		return nil
	}
	return []string{"--context", kctx}
}

// kubectlApplyBytes runs `kubectl apply -f -` with the given YAML
// piped in via stdin. When kctx is non-empty it targets that context
// explicitly (`--context <kctx>`); kctx == "" inherits the current
// pinned context (caller pins it via pinKubectlContext first).
func kubectlApplyBytes(ctx context.Context, kctx string, yamlBytes []byte) error {
	args := append(kubeContextArgs(kctx), "apply", "-f", "-")
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = strings.NewReader(string(yamlBytes))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
}

// kubectlApplyFile runs `kubectl apply -f <path>`. kctx targets a
// specific context when non-empty; "" uses the pinned context.
func kubectlApplyFile(ctx context.Context, kctx, path string) error {
	args := append(kubeContextArgs(kctx), "apply", "-f", path)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply -f %s: %w", path, err)
	}
	return nil
}

// waitForCRDs blocks until the named Gateway API CRDs report
// Established=True, or times out. Run between CRD apply and
// GatewayClass apply so the latter doesn't race the controller's
// CRD reconciler. kctx targets a specific context when non-empty.
func waitForCRDs(ctx context.Context, kctx string, crds []string, timeout time.Duration) error {
	args := append(kubeContextArgs(kctx), "wait", "--for=condition=Established", "--timeout="+timeout.String())
	for _, c := range crds {
		args = append(args, "crd/"+c)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// traefikEntrypoint is one Gateway listener projected into the shape
// the Traefik install template consumes. Name is the listener's KCL
// name (e.g. "http", "grpc") — used as the entrypoint label and as
// the containerPort/Service-port name. Traefik's kubernetesgateway
// provider matches listeners to entrypoints by port number, not name;
// the name is just a label for log diagnosis. Protocol is carried
// alongside in case future Traefik versions key off it, but the
// current entrypoint-arg shape doesn't use it.
type traefikEntrypoint struct {
	Name     string
	Port     int
	Protocol string
}

// collectTraefikEntrypoints renders the given env's KCL and projects
// its Gateway listeners into the entrypoint shape the Traefik template
// expects. Dedupes by port — a port can have only one entrypoint, so
// the first listener (sorted by port, then gateway, then name) wins
// on collisions.
//
// Gateways are sourced from the rendered `manifests` stream (every
// `kind: Gateway` object), NOT the typed `entities.Gateways` echo.
// That's deliberate: an env may render its Gateways as RAW manifests
// (e.g. a multi-cluster env that stamps each Gateway with an owning-app
// label to pin it to one cluster, leaving the bundle-level `gateways`
// list empty) — those don't appear in the typed echo but DO appear in
// the manifest stream. Reading the stream covers both shapes.
//
// env is the env being brought up (`forge cluster up` passes "dev"; the
// declared-cluster path passes the active `forge up --env=<env>`). A
// missing KCL dir or a render with no gateways returns (nil, nil): a
// normal state for a project with features.ingress on but no routes yet
// (Traefik installs with the default `ping` entrypoint; listeners are
// added on a later re-run).
func collectTraefikEntrypoints(ctx context.Context, projectDir, env string) ([]traefikEntrypoint, error) {
	if projectDir == "" {
		return nil, nil
	}
	if env == "" {
		env = "dev"
	}
	envKCL := filepath.Join(projectDir, "deploy", "kcl", env)
	if _, err := os.Stat(envKCL); err != nil {
		return nil, nil //nolint:nilerr // missing env KCL is a normal first-scaffold state
	}
	rawJSON, err := renderKCLRaw(ctx, projectDir, env)
	if err != nil {
		return nil, err
	}

	type candidate struct {
		gw    string
		name  string
		port  int
		proto string
	}
	var raw []candidate
	for _, gw := range gatewayListenersFromRender(rawJSON) {
		for _, l := range gw.listeners {
			raw = append(raw, candidate{gw: gw.name, name: l.name, port: l.port, proto: l.proto})
		}
	}
	// Stable sort: port, then gateway, then listener. Deterministic
	// output across re-runs and matches the k3d-ports generator's
	// dedupe order so the two stays in lockstep.
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].port != raw[j].port {
			return raw[i].port < raw[j].port
		}
		if raw[i].gw != raw[j].gw {
			return raw[i].gw < raw[j].gw
		}
		return raw[i].name < raw[j].name
	})

	seenPort := map[int]bool{}
	seenName := map[string]bool{}
	var out []traefikEntrypoint
	for _, c := range raw {
		if seenPort[c.port] {
			continue
		}
		seenPort[c.port] = true
		// Disambiguate name collisions across gateways. Two gateways
		// may both name a listener `http`; Traefik requires
		// entrypoint names to be unique. Suffix `-2`, `-3`, … on
		// collision. Port-based ordering ensures the lowest port
		// keeps the bare name.
		name := c.name
		for i := 2; seenName[name]; i++ {
			name = fmt.Sprintf("%s-%d", c.name, i)
		}
		seenName[name] = true
		out = append(out, traefikEntrypoint{Name: name, Port: c.port, Protocol: c.proto})
	}
	return out, nil
}

// renderGateway is one Gateway object recovered from the rendered
// manifest stream — just the bits collectTraefikEntrypoints needs.
type renderGateway struct {
	name      string
	listeners []renderGatewayListener
}

type renderGatewayListener struct {
	name  string
	port  int
	proto string
}

// gatewayListenersFromRender extracts every Gateway's listeners from a
// forge KCL render's JSON, from BOTH shapes a render can carry:
//
//   - the typed `gateways` ENTITY echo (`output.gateways[*]` or top-level
//     `gateways[*]`), where each gateway is `{name, listeners:[{name,
//     port, protocol}]}`. This is the bundle-level `gateways` field —
//     what the dev `forge cluster up` path consumes.
//   - the rendered `manifests` STREAM (`output.manifests[*]` or top-level
//     `manifests[*]`), matching `kind: Gateway` in any
//     gateway.networking.k8s.io apiVersion. This covers an env that
//     renders its Gateways as RAW manifests (e.g. a multi-cluster env that
//     stamps each with an owning-app label to pin it to one cluster,
//     leaving the bundle-level `gateways` list empty) — those never appear
//     in the typed echo but DO appear here.
//
// Reading both means the entrypoint collection works whether an env wires
// its Gateway through the typed field or as a raw manifest. Best-effort:
// malformed JSON or a render with no Gateways yields nil. A Gateway seen
// in more than one shape (by name) is emitted once.
func gatewayListenersFromRender(data []byte) []renderGateway {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	type listenerJSON struct {
		Name     string `json:"name"`
		Port     int    `json:"port"`
		Protocol string `json:"protocol"`
	}
	// Typed entity echo: gateways[*].listeners[*].
	type gatewayEntityJSON struct {
		Name      string         `json:"name"`
		Listeners []listenerJSON `json:"listeners"`
	}
	// Raw k8s manifest: kind Gateway, spec.listeners[*].
	type gatewayManifestJSON struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Listeners []listenerJSON `json:"listeners"`
		} `json:"spec"`
	}
	var doc struct {
		Gateways  []gatewayEntityJSON `json:"gateways"`
		Manifests []json.RawMessage   `json:"manifests"`
		Output    struct {
			Gateways  []gatewayEntityJSON `json:"gateways"`
			Manifests []json.RawMessage   `json:"manifests"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}

	var out []renderGateway
	seen := map[string]bool{} // dedupe a Gateway that appears in more than one shape

	add := func(name string, listeners []listenerJSON) {
		if seen[name] {
			return
		}
		seen[name] = true
		rg := renderGateway{name: name}
		for _, l := range listeners {
			rg.listeners = append(rg.listeners, renderGatewayListener{name: l.Name, port: l.Port, proto: l.Protocol})
		}
		out = append(out, rg)
	}

	// Typed entity echo first (the bundle-level `gateways` field).
	for _, ge := range append(append([]gatewayEntityJSON{}, doc.Gateways...), doc.Output.Gateways...) {
		add(ge.Name, ge.Listeners)
	}
	// Then the raw manifest stream (kind: Gateway).
	for _, stream := range [][]json.RawMessage{doc.Manifests, doc.Output.Manifests} {
		for _, rawM := range stream {
			var g gatewayManifestJSON
			if err := json.Unmarshal(rawM, &g); err != nil {
				continue
			}
			if g.Kind != "Gateway" || !strings.HasPrefix(g.APIVersion, "gateway.networking.k8s.io") {
				continue
			}
			add(g.Metadata.Name, g.Spec.Listeners)
		}
	}
	return out
}

// renderTraefikInstall executes the vendored Traefik install template
// against the given entrypoints and returns the rendered YAML bytes.
// Empty entrypoints are valid — the bundle still installs (Traefik
// runs with the default `ping` entrypoint) and the user can re-run
// `forge cluster up` after adding listeners.
func renderTraefikInstall(entrypoints []traefikEntrypoint) ([]byte, error) {
	return templates.IngressTemplates().Render("traefik/traefik.yaml.tmpl", struct {
		Entrypoints []traefikEntrypoint
	}{Entrypoints: entrypoints})
}

// installIngressBundle is the post-cluster-up wiring entrypoint.
// Called from runDevClusterUp when features.ingress is on.
//
// Order matters:
//  1. Apply Gateway API CRDs (fetched if not cached).
//  2. Wait for CRDs Established.
//  3. Render the Traefik install with one entrypoint per dev Gateway
//     listener and apply it.
//  4. Apply the `traefik` GatewayClass (depends on CRDs being live).
//
// projectDir is used to evaluate the dev env's KCL gateways for step
// 3 — same data the k3d-ports generator uses. Adding or removing a
// listener requires re-running `forge cluster up` to install the
// new entrypoints (idempotent — re-applying the rendered Deployment
// restarts the pod with the new args).
//
// Failure anywhere short-circuits — the cluster is up but ingress
// isn't, so subsequent `forge deploy dev` will fail apply on the
// project's Gateway resources. The error message is what the user
// acts on; we don't try to clean up the partial install.
// kctx pins the kubectl context the install targets. "" means "use the
// caller-pinned current context" (the dev `forge cluster up` path, which
// pins via pinKubectlContext first). The declared-cluster path passes an
// explicit `k3d-<name>` so the install lands on THAT cluster regardless
// of the active context. env is the env whose Gateway listeners drive
// the Traefik entrypoints (collectTraefikEntrypoints).
func installIngressBundle(ctx context.Context, kctx, projectDir, env string) error {
	_, gatewayAPIVer, err := ingressPinnedVersions()
	if err != nil {
		return err
	}

	crdPath, err := fetchGatewayAPICRDs(ctx, gatewayAPIVer)
	if err != nil {
		return err
	}
	fmt.Println("Applying Gateway API CRDs...")
	if err := kubectlApplyFile(ctx, kctx, crdPath); err != nil {
		return err
	}

	// The names below are the Gateway API standard-channel CRDs we
	// actually consume. We skip ReferenceGrant + the experimental-
	// channel TCPRoute/TLSRoute/UDPRoute because forge doesn't render
	// them in v1; including them in the wait list would only delay
	// happy-path cluster-up if upstream renames or splits them.
	fmt.Println("Waiting for Gateway API CRDs to be Established...")
	if err := waitForCRDs(ctx, kctx, []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"grpcroutes.gateway.networking.k8s.io",
	}, 60*time.Second); err != nil {
		return fmt.Errorf("wait for Gateway API CRDs: %w", err)
	}

	entrypoints, err := collectTraefikEntrypoints(ctx, projectDir, env)
	if err != nil {
		// Intentional soft warning: KCL render failures here are
		// non-fatal — the project may not have this env's KCL yet, or kcl
		// may not be installed locally. Install the bundle with no
		// extra entrypoints and let the user re-run once their KCL is
		// ready. There's no --strict equivalent because the install is a
		// developer-loop helper, not a CI gate.
		fmt.Fprintf(os.Stderr, "Warning: could not evaluate %s gateways for Traefik entrypoints; installing without listener-derived entrypoints: %v\n", env, err)
		entrypoints = nil
	}
	if len(entrypoints) > 0 {
		labels := make([]string, len(entrypoints))
		for i, e := range entrypoints {
			labels[i] = fmt.Sprintf("%s:%d", e.Name, e.Port)
		}
		fmt.Printf("Installing Traefik controller (traefik-system namespace) with entrypoints: %s\n", strings.Join(labels, ", "))
	} else {
		fmt.Println("Installing Traefik controller (traefik-system namespace)...")
	}
	traefikYAML, err := renderTraefikInstall(entrypoints)
	if err != nil {
		return fmt.Errorf("render vendored Traefik install: %w", err)
	}
	if err := kubectlApplyBytes(ctx, kctx, traefikYAML); err != nil {
		return err
	}

	gcYAML, err := templates.IngressTemplates().Get("traefik/gatewayclass.yaml")
	if err != nil {
		return fmt.Errorf("load vendored GatewayClass: %w", err)
	}
	fmt.Println("Applying traefik GatewayClass...")
	if err := kubectlApplyBytes(ctx, kctx, gcYAML); err != nil {
		return err
	}

	fmt.Println("Ingress install complete.")
	return nil
}

// k3dConfigPath holds the path to the (possibly merged) k3d config
// passed to `k3d cluster create`. Callers that don't need the merge
// pass the raw configPath; the merge path writes a tempfile and
// returns its path.
type k3dConfigPath struct {
	path      string
	temporary bool // true when we own cleanup
}

// mergeK3dConfig reads deploy/k3d.yaml + deploy/k3d-ports.yaml from
// the project root, splices the ports[] array from the fragment into
// the user config, and returns a path to a temp file holding the
// merged YAML. When the fragment is missing the user config is
// passed through unchanged. Caller invokes Close() to clean up.
//
// Merge policy: fragment entries WIN over scaffolded entries on the
// same host port — the fragment is derived from the current KCL
// truth, the scaffolded deploy/k3d.yaml is a one-shot from `forge
// new`. Entries that don't parse as the canonical `<host>:<cluster>`
// shorthand are passed through unchanged (best-effort: a warning is
// printed and both entries survive into the merged config — k3d may
// then reject the config, but the warning gives the user a starting
// point).
func mergeK3dConfig(userPath string, ingressOn bool) (k3dConfigPath, func(), error) {
	cleanup := func() {}
	if !ingressOn {
		return k3dConfigPath{path: userPath}, cleanup, nil
	}
	projectDir := filepath.Dir(userPath) // userPath is typically deploy/k3d.yaml; sibling is deploy/k3d-ports.yaml
	fragPath := filepath.Join(projectDir, "k3d-ports.yaml")
	if _, err := os.Stat(fragPath); errors.Is(err, os.ErrNotExist) {
		return k3dConfigPath{path: userPath}, cleanup, nil
	}

	userBytes, err := os.ReadFile(userPath)
	if err != nil {
		return k3dConfigPath{}, cleanup, fmt.Errorf("read %s: %w", userPath, err)
	}
	fragBytes, err := os.ReadFile(fragPath)
	if err != nil {
		return k3dConfigPath{}, cleanup, fmt.Errorf("read %s: %w", fragPath, err)
	}

	merged, err := spliceK3dPorts(userBytes, fragBytes)
	if err != nil {
		return k3dConfigPath{}, cleanup, fmt.Errorf("merge k3d config: %w", err)
	}

	tmp, err := os.CreateTemp("", "forge-k3d-config-*.yaml")
	if err != nil {
		return k3dConfigPath{}, cleanup, err
	}
	if _, err := tmp.Write(merged); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return k3dConfigPath{}, cleanup, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return k3dConfigPath{}, cleanup, err
	}
	cleanup = func() { _ = os.Remove(tmp.Name()) }
	return k3dConfigPath{path: tmp.Name(), temporary: true}, cleanup, nil
}

// spliceK3dPorts is the pure YAML-merging half — exposed for tests
// so the merge policy is unit-testable without temp files. The user
// YAML's `ports:` list (if present) is merged with the fragment's
// entries: fragment entries WIN on host-port collisions. Other
// top-level keys pass through unchanged so we don't silently drop
// registries, agent counts, etc.
//
// Host-port extraction handles the canonical `port: <host>:<cluster>`
// shorthand. Entries we can't parse (alternative forms, missing port
// key) are passed through verbatim with a warning — k3d may then
// reject the config, but the warning gives the user a starting point.
func spliceK3dPorts(userYAML, fragmentYAML []byte) ([]byte, error) {
	var userDoc map[string]any
	if err := yaml.Unmarshal(userYAML, &userDoc); err != nil {
		return nil, fmt.Errorf("parse user k3d.yaml: %w", err)
	}
	if userDoc == nil {
		userDoc = map[string]any{}
	}
	var frag map[string]any
	if err := yaml.Unmarshal(fragmentYAML, &frag); err != nil {
		return nil, fmt.Errorf("parse k3d-ports.yaml: %w", err)
	}
	fragPorts, _ := frag["ports"].([]any)
	if len(fragPorts) == 0 {
		// Fragment has no ports — nothing to splice; return user
		// YAML verbatim so we don't disturb formatting/comments.
		return userYAML, nil
	}
	userPorts, _ := userDoc["ports"].([]any)

	// Collect host ports claimed by the fragment. Entries from the
	// user list that share a host port get dropped (fragment wins).
	// Entries we can't classify pass through unchanged — caller logs
	// a warning so the user can investigate if k3d then rejects.
	fragHosts := map[int]bool{}
	for _, e := range fragPorts {
		if host, ok := k3dPortHost(e); ok {
			fragHosts[host] = true
		}
	}

	merged := make([]any, 0, len(userPorts)+len(fragPorts))
	for _, e := range userPorts {
		host, ok := k3dPortHost(e)
		if !ok {
			// Unrecognized shape — keep it, warn the user. We
			// don't crash because the alternative forms (structured
			// `port:` int + hostIP/protocol/nodeFilters siblings) are
			// legitimate k3d config; we just can't dedupe them.
			fmt.Fprintf(os.Stderr, "warning: deploy/k3d.yaml ports[] entry not in canonical <host>:<cluster> shorthand; passing through without dedupe: %v\n", e)
			merged = append(merged, e)
			continue
		}
		if fragHosts[host] {
			// Fragment wins on this host port — drop the user entry.
			continue
		}
		merged = append(merged, e)
	}
	merged = append(merged, fragPorts...)
	userDoc["ports"] = merged

	out, err := yaml.Marshal(userDoc)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// k3dPortHost extracts the host-port integer from a k3d ports[] entry
// in the canonical `port: <host>:<cluster>` shorthand. Returns
// (port, true) on success; (0, false) for anything else (alternative
// structured forms, missing key, bare `port: <int>`). Callers treat
// the false case as "don't dedupe this entry".
func k3dPortHost(entry any) (int, bool) {
	m, ok := entry.(map[string]any)
	if !ok {
		return 0, false
	}
	raw, ok := m["port"].(string)
	if !ok {
		return 0, false
	}
	hostStr, _, ok := strings.Cut(raw, ":")
	if !ok {
		// Bare `port: 18080` (no cluster side) — possible in k3d but
		// not the shape we emit. Bail out of the dedupe; caller will
		// pass through with a warning.
		return 0, false
	}
	host, err := strconv.Atoi(strings.TrimSpace(hostStr))
	if err != nil {
		return 0, false
	}
	return host, true
}
