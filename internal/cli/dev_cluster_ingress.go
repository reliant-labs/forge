// Package cli — `forge dev cluster up` ingress install plumbing.
//
// Three pieces, run in order after the k3d cluster is created and
// kubectl context pinned:
//
//  1. Fetch + apply the upstream Gateway API CRDs (version pinned via
//     internal/templates/ingress/traefik/VERSION). Cached under
//     ~/.cache/forge/ingress/ so subsequent cluster-up runs are
//     offline-capable.
//  2. Apply the vendored Traefik controller install
//     (internal/templates/ingress/traefik/traefik.yaml).
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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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

// kubectlApplyBytes runs `kubectl apply -f -` with the given YAML
// piped in via stdin. Inherits the current kubectl context (caller is
// responsible for pinning it via pinKubectlContext first).
func kubectlApplyBytes(ctx context.Context, yamlBytes []byte) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(string(yamlBytes))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl apply: %w", err)
	}
	return nil
}

// kubectlApplyFile runs `kubectl apply -f <path>` against the
// currently pinned context.
func kubectlApplyFile(ctx context.Context, path string) error {
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", path)
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
// CRD reconciler.
func waitForCRDs(ctx context.Context, crds []string, timeout time.Duration) error {
	args := []string{"wait", "--for=condition=Established", "--timeout=" + timeout.String()}
	for _, c := range crds {
		args = append(args, "crd/"+c)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// installIngressBundle is the post-cluster-up wiring entrypoint.
// Called from runDevClusterUp when features.ingress is on.
//
// Order matters:
//  1. Apply Gateway API CRDs (fetched if not cached).
//  2. Wait for CRDs Established.
//  3. Apply the Traefik controller install.
//  4. Apply the `traefik` GatewayClass (depends on CRDs being live).
//
// Failure anywhere short-circuits — the cluster is up but ingress
// isn't, so subsequent `forge deploy dev` will fail apply on the
// project's Gateway resources. The error message is what the user
// acts on; we don't try to clean up the partial install.
func installIngressBundle(ctx context.Context) error {
	_, gatewayAPIVer, err := ingressPinnedVersions()
	if err != nil {
		return err
	}

	crdPath, err := fetchGatewayAPICRDs(ctx, gatewayAPIVer)
	if err != nil {
		return err
	}
	fmt.Println("Applying Gateway API CRDs...")
	if err := kubectlApplyFile(ctx, crdPath); err != nil {
		return err
	}

	// The names below are the Gateway API standard-channel CRDs we
	// actually consume. We skip ReferenceGrant + the experimental-
	// channel TCPRoute/TLSRoute/UDPRoute because forge doesn't render
	// them in v1; including them in the wait list would only delay
	// happy-path cluster-up if upstream renames or splits them.
	fmt.Println("Waiting for Gateway API CRDs to be Established...")
	if err := waitForCRDs(ctx, []string{
		"gatewayclasses.gateway.networking.k8s.io",
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"grpcroutes.gateway.networking.k8s.io",
	}, 60*time.Second); err != nil {
		return fmt.Errorf("wait for Gateway API CRDs: %w", err)
	}

	traefikYAML, err := templates.IngressTemplates().Get("traefik/traefik.yaml")
	if err != nil {
		return fmt.Errorf("load vendored Traefik install: %w", err)
	}
	fmt.Println("Installing Traefik controller (traefik-system namespace)...")
	if err := kubectlApplyBytes(ctx, traefikYAML); err != nil {
		return err
	}

	gcYAML, err := templates.IngressTemplates().Get("traefik/gatewayclass.yaml")
	if err != nil {
		return fmt.Errorf("load vendored GatewayClass: %w", err)
	}
	fmt.Println("Applying traefik GatewayClass...")
	if err := kubectlApplyBytes(ctx, gcYAML); err != nil {
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
	path       string
	temporary  bool // true when we own cleanup
}

// mergeK3dConfig reads deploy/k3d.yaml + deploy/k3d-ports.yaml from
// the project root, splices the ports[] array from the fragment into
// the user config, and returns a path to a temp file holding the
// merged YAML. When the fragment is missing the user config is
// passed through unchanged. Caller invokes Close() to clean up.
//
// Merge policy: the fragment's ports[] entries APPEND to whatever
// the user declared in deploy/k3d.yaml. Duplicates on the host port
// number cause k3d to reject the config at create time — that's an
// audit signal that the user's hand-edits and the project's ingress
// disagree.
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
// YAML's `ports:` list (if present) gets the fragment's entries
// appended. Other top-level keys pass through unchanged so we don't
// silently drop registries, agent counts, etc.
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
	userDoc["ports"] = append(userPorts, fragPorts...)
	out, err := yaml.Marshal(userDoc)
	if err != nil {
		return nil, err
	}
	return out, nil
}
