// Package cli — `forge cluster up` ingress install plumbing.
//
// forge is OPINIONATED on ONE Gateway API controller everywhere: Envoy
// Gateway (controllerName gateway.envoyproxy.io/gatewayclass-controller,
// GatewayClass `eg`). The local k3d bring-up installs the SAME helm chart
// the cloud envs (staging/preprod/prod) run, so a Gateway that renders
// `gatewayClassName: eg` behaves identically on a laptop and in the cloud.
// Envoy Gateway serves HTTPRoute AND GRPCRoute natively, so one controller
// covers every forge route shape — no second controller for gRPC.
//
// Three pieces, run in order after the k3d cluster is created and
// kubectl context pinned:
//
//  1. Fetch + apply the upstream Gateway API standard-channel CRDs
//     (version pinned via internal/templates/ingress/envoy/VERSION).
//     Cached under ~/.cache/forge/ingress/ so subsequent cluster-up runs
//     are offline-capable. The gateway-helm chart bundles the CRDs it
//     needs, but we apply the pinned standard channel explicitly first
//     (server-side) so the CRD surface matches the cloud install and so
//     `kubectl wait` on Established gates the GatewayClass apply.
//  2. `helm upgrade --install` the Envoy Gateway controller from the
//     pinned gateway-helm chart version into envoy-gateway-system, the
//     SAME release the cloud envs run. Envoy Gateway provisions a managed
//     Envoy proxy per Gateway with listener sockets derived dynamically
//     from Gateway.spec.listeners — no static per-listener entrypoint
//     config to template (unlike the old Traefik install). The proxy's
//     LoadBalancer Service ports follow the Gateway listeners; on k3d the
//     bundled klipper servicelb binds those node ports and the k3d
//     serverlb forwards the host ports mapped from deploy/k3d-ports.yaml.
//  3. Apply the vendored `eg` GatewayClass (idempotent).
//
// Idempotency comes from `helm upgrade --install` + `kubectl apply`
// semantics — re-running against a cluster that already has them noops.
// We block on CRD establishment between (1) and (3) so the GatewayClass
// apply doesn't race the CRD install.
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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/templates"
)

// ingressPinnedVersions reads internal/templates/ingress/envoy/VERSION
// and returns (envoyGatewayChartVersion, gatewayAPIVersion). Format:
//
//	envoy_gateway=v1.7.2
//	gateway_api=v1.5.1
func ingressPinnedVersions() (envoyGatewayVer, gatewayAPIVer string, err error) {
	b, err := templates.IngressTemplates().Get("envoy/VERSION")
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
		case "envoy_gateway":
			envoyGatewayVer = strings.TrimSpace(v)
		case "gateway_api":
			gatewayAPIVer = strings.TrimSpace(v)
		}
	}
	if envoyGatewayVer == "" || gatewayAPIVer == "" {
		return "", "", fmt.Errorf("VERSION missing one of envoy_gateway=/gateway_api= keys")
	}
	return envoyGatewayVer, gatewayAPIVer, nil
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

// kubectlApplyServerSideFile runs `kubectl apply --server-side
// --force-conflicts -f <path>`. The Gateway API CRD bundle carries
// large last-applied-configuration annotations that overflow the 256KB
// client-side apply limit on some CRDs; server-side apply (the same mode
// the cloud install uses) sidesteps that and resolves field-manager
// conflicts deterministically. kctx targets a specific context when
// non-empty; "" uses the pinned context.
func kubectlApplyServerSideFile(ctx context.Context, kctx, path string) error {
	args := append(kubeContextArgs(kctx), "apply", "--server-side", "--force-conflicts", "-f", path)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply --server-side -f %s: %w", path, err)
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

// envoyGatewayChartRef is the OCI ref of the gateway-helm chart — the
// SAME chart the cloud envs install. The pinned version comes from
// internal/templates/ingress/envoy/VERSION (envoy_gateway=).
const envoyGatewayChartRef = "oci://docker.io/envoyproxy/gateway-helm"

// envoyGatewayReleaseName / envoyGatewayNamespace are the helm release
// name and namespace the cloud envs use, kept identical for the local
// install so `helm list -n envoy-gateway-system` reads the same here.
const (
	envoyGatewayReleaseName = "eg"
	envoyGatewayNamespace   = "envoy-gateway-system"
)

// helmInstallEnvoyGateway runs `helm upgrade --install` for the pinned
// gateway-helm chart into envoy-gateway-system, targeting the given
// kubectl context when non-empty. Idempotent: `--install` creates the
// release on a cold cluster and upgrades-in-place on a warm one. `--wait`
// blocks until the controller Deployment is Available so the GatewayClass
// apply (and the subsequent deploy phase) doesn't race a controller that
// isn't reconciling yet.
func helmInstallEnvoyGateway(ctx context.Context, kctx, chartVersion string) error {
	args := []string{
		"upgrade", "--install", envoyGatewayReleaseName, envoyGatewayChartRef,
		"--version", chartVersion,
		"--namespace", envoyGatewayNamespace,
		"--create-namespace",
		"--wait", "--timeout", "180s",
	}
	if kctx != "" {
		args = append(args, "--kube-context", kctx)
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm upgrade --install %s %s: %w", envoyGatewayReleaseName, envoyGatewayChartRef, err)
	}
	return nil
}

// certManagerChartRepo / certManagerChartName / certManagerReleaseName /
// certManagerNamespace are the helm coordinates for cert-manager — the
// jetstack chart the cloud envs run (staging already has v1.20.1). Kept
// as named constants so the cloud cluster-setup install is byte-aligned
// with what an operator would type by hand (the install this REPLACES).
const (
	certManagerChartRepoURL  = "https://charts.jetstack.io"
	certManagerChartRepoName = "jetstack"
	certManagerChartName     = "jetstack/cert-manager"
	certManagerReleaseName   = "cert-manager"
	certManagerNamespace     = "cert-manager"
)

// certManagerChartVersion is the pinned cert-manager chart version. Kept
// in lockstep with what staging runs (v1.20.1) so cloud clusters forge
// bootstraps match the cluster the project was validated against. A
// single source so a future bump is one edit.
const certManagerChartVersion = "v1.20.1"

// helmRepoAddUpdate registers (idempotently) a helm chart repo and
// refreshes the local index. `helm repo add` is a no-op when the repo is
// already registered with the same URL; we tolerate its non-zero exit on a
// re-add by always following with `helm repo update`, which is the
// load-bearing step (it makes the chart resolvable). Stdout/stderr stream
// so the user sees the fetch.
func helmRepoAddUpdate(ctx context.Context, name, url string) error {
	add := exec.CommandContext(ctx, "helm", "repo", "add", name, url)
	add.Stdout = os.Stdout
	add.Stderr = os.Stderr
	// A duplicate add (same name) exits non-zero with "already exists";
	// that's fine — the subsequent update is what matters. Only a genuine
	// failure (e.g. network) surfaces via the update below.
	_ = add.Run()
	upd := exec.CommandContext(ctx, "helm", "repo", "update", name)
	upd.Stdout = os.Stdout
	upd.Stderr = os.Stderr
	if err := upd.Run(); err != nil {
		return fmt.Errorf("helm repo update %s: %w", name, err)
	}
	return nil
}

// helmInstallCertManager runs `helm upgrade --install` for the pinned
// jetstack/cert-manager chart into the cert-manager namespace, targeting
// the given kubectl context when non-empty. crds.enabled=true installs the
// CRDs with the chart (matching staging). Idempotent: `--install` creates
// on a cold cluster and upgrades-in-place on a warm one; `--wait` blocks
// until the controller Deployments are Available so the ClusterIssuer apply
// that follows doesn't race the webhook coming up.
func helmInstallCertManager(ctx context.Context, kctx, chartVersion string) error {
	args := []string{
		"upgrade", "--install", certManagerReleaseName, certManagerChartName,
		"--version", chartVersion,
		"--namespace", certManagerNamespace,
		"--create-namespace",
		"--set", "crds.enabled=true",
		"--wait", "--timeout", "300s",
	}
	if kctx != "" {
		args = append(args, "--kube-context", kctx)
	}
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("helm upgrade --install %s %s: %w", certManagerReleaseName, certManagerChartName, err)
	}
	return nil
}

// resourceExists reports whether the named cluster resource exists in the
// given kubectl context. Used for the idempotency skip checks (a present
// GatewayClass / cert-manager Deployment means the stack is already
// installed). A non-zero exit (NotFound) returns false with no error; a
// genuine kubectl failure (e.g. no such context) returns the error so the
// caller fails loudly rather than silently re-installing.
func resourceExists(ctx context.Context, kctx string, getArgs ...string) (bool, error) {
	args := append(kubeContextArgs(kctx), "get")
	args = append(args, getArgs...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	// Suppress NotFound noise on stdout/stderr — we only care about exit code.
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// kubectl get of an absent resource exits 1 with "NotFound";
			// treat as "doesn't exist". A bad context / unreachable API
			// server also exits non-zero, but those surface downstream when
			// the install itself runs against the same context.
			return false, nil
		}
		return false, fmt.Errorf("kubectl get %s: %w", strings.Join(getArgs, " "), err)
	}
	return true, nil
}

// installIngressBundle is the post-cluster-up wiring entrypoint.
// Called from runDevClusterUp when features.ingress is on.
//
// Order matters:
//  1. Apply the pinned Gateway API standard-channel CRDs (fetched if not
//     cached, applied server-side).
//  2. Wait for CRDs Established.
//  3. `helm upgrade --install` the Envoy Gateway controller (the SAME
//     gateway-helm chart/version the cloud envs run). Envoy Gateway
//     derives each managed proxy's listener sockets from the Gateway's
//     spec.listeners dynamically — no per-listener static config to
//     template, so the install is env-independent.
//  4. Apply the vendored `eg` GatewayClass (depends on CRDs being live).
//
// projectDir/env are retained for signature stability with the
// declared-cluster caller; the Envoy install needs neither (the
// listener-derived host ports come from deploy/k3d-ports.yaml at cluster
// CREATE time, not from the controller install).
//
// Failure anywhere short-circuits — the cluster is up but ingress
// isn't, so subsequent `forge deploy <env>` will fail apply on the
// project's Gateway resources. The error message is what the user
// acts on; we don't try to clean up the partial install.
// kctx pins the kubectl context the install targets. "" means "use the
// caller-pinned current context" (the dev `forge cluster up` path, which
// pins via pinKubectlContext first). The declared-cluster path passes an
// explicit `k3d-<name>` so the install lands on THAT cluster regardless
// of the active context.
func installIngressBundle(ctx context.Context, kctx, projectDir, env string) error {
	_ = projectDir // retained for caller signature stability (see doc above)
	_ = env
	envoyVer, gatewayAPIVer, err := ingressPinnedVersions()
	if err != nil {
		return err
	}

	crdPath, err := fetchGatewayAPICRDs(ctx, gatewayAPIVer)
	if err != nil {
		return err
	}
	fmt.Println("Applying Gateway API CRDs...")
	if err := kubectlApplyServerSideFile(ctx, kctx, crdPath); err != nil {
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

	fmt.Printf("Installing Envoy Gateway %s (helm release %q, namespace %s)...\n",
		envoyVer, envoyGatewayReleaseName, envoyGatewayNamespace)
	if err := helmInstallEnvoyGateway(ctx, kctx, envoyVer); err != nil {
		return err
	}

	gcYAML, err := templates.IngressTemplates().Get("envoy/gatewayclass.yaml")
	if err != nil {
		return fmt.Errorf("load vendored GatewayClass: %w", err)
	}
	fmt.Println("Applying eg GatewayClass...")
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
