package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Preflight is the deploy-time "deployability contract": before a single
// manifest is applied, it checks that the rendered bundle's external
// dependencies actually exist on the LIVE target — every Secret KEY a
// container references is present in the target cluster, and every
// container image: is resolvable in its registry. When something is
// missing it returns a single grouped error naming EVERYTHING missing at
// once, so the user fixes it all in one pass instead of discovering each
// gap one-at-a-time as pods crash (CreateContainerConfigError /
// ImagePullBackOff) over a live rollout.
//
// The checks are injected (SecretGetter / ImageChecker) so the orchestration
// is unit-testable without a live cluster or registry; the deploy path wires
// the real kubectl-/docker-backed implementations (see KubectlSecretGetter /
// DockerImageChecker).

// ManifestRefs is the set of external references a rendered manifest bundle
// depends on at schedule time: the Secret (name, key) pairs its containers
// project into env, and the distinct container images it runs.
type ManifestRefs struct {
	// Secrets maps a Secret name to the set of keys referenced from it. A
	// key of "" (present in the set) means a whole-Secret reference (envFrom
	// secretRef) — verify existence only.
	Secrets map[string]map[string]struct{}
	// Images is the set of distinct container image refs in the bundle.
	Images map[string]struct{}
}

// CollectManifestRefs walks a `---`-separated multi-doc YAML manifest stream
// and collects every Secret reference (secretKeyRef + envFrom secretRef) and
// every container image. It recurses the whole document tree rather than
// hard-coding pod-spec paths, so it picks references up uniformly across
// Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, Pods, and any
// nested template — wherever a `secretKeyRef`, `secretRef`, or `image`
// appears. Malformed documents are skipped (best-effort, mirroring the other
// manifest scanners in this package).
func CollectManifestRefs(manifests string) ManifestRefs {
	refs := ManifestRefs{
		Secrets: map[string]map[string]struct{}{},
		Images:  map[string]struct{}{},
	}
	for _, doc := range splitDocs(manifests) {
		var node any
		if err := yaml.Unmarshal([]byte(doc), &node); err != nil {
			continue
		}
		collectRefs(node, &refs)
	}
	return refs
}

// collectRefs recursively descends a decoded YAML value, recording any
// secretKeyRef / envFrom secretRef / container image it finds into refs.
func collectRefs(node any, refs *ManifestRefs) {
	switch v := node.(type) {
	case map[string]any:
		// secretKeyRef: {name, key} — a single (Secret, key) projection.
		if skr, ok := mapAt(v, "secretKeyRef"); ok {
			name := stringAt(skr, "name")
			key := stringAt(skr, "key")
			if name != "" {
				addSecretKey(refs, name, key)
			}
		}
		// envFrom secretRef: {name} — projects the WHOLE Secret. Record
		// with key "" so the check verifies existence only (we can't know
		// which keys the image reads).
		if sr, ok := mapAt(v, "secretRef"); ok {
			// Skip the secretKeyRef's nested case (handled above): a
			// secretKeyRef value is never itself keyed "secretRef".
			if name := stringAt(sr, "name"); name != "" {
				addSecretKey(refs, name, "")
			}
		}
		// Container image. Only treat a STRING image as a container ref;
		// objects keyed "image" elsewhere (rare) are ignored.
		if img, ok := v["image"]; ok {
			if s, ok := img.(string); ok && strings.TrimSpace(s) != "" {
				refs.Images[strings.TrimSpace(s)] = struct{}{}
			}
		}
		for _, child := range v {
			collectRefs(child, refs)
		}
	case []any:
		for _, child := range v {
			collectRefs(child, refs)
		}
	}
}

func addSecretKey(refs *ManifestRefs, name, key string) {
	if refs.Secrets[name] == nil {
		refs.Secrets[name] = map[string]struct{}{}
	}
	refs.Secrets[name][key] = struct{}{}
}

// mapAt returns m[key] as a map[string]any when present and of that type.
func mapAt(m map[string]any, key string) (map[string]any, bool) {
	raw, ok := m[key]
	if !ok {
		return nil, false
	}
	sub, ok := raw.(map[string]any)
	return sub, ok
}

// stringAt returns m[key] as a string when present and of that type.
func stringAt(m map[string]any, key string) string {
	if raw, ok := m[key]; ok {
		if s, ok := raw.(string); ok {
			return s
		}
	}
	return ""
}

// SecretGetter resolves which keys exist on a Secret in a target cluster.
// GetSecretKeys returns the set of keys present in the named Secret (the
// keys of its `.data`). exists=false means the Secret itself is absent in
// the namespace — every referenced key is then reported missing. An error
// is a genuine lookup failure (kubectl not configured, RBAC denial) and
// aborts the preflight rather than being treated as "missing".
type SecretGetter interface {
	GetSecretKeys(ctx context.Context, kctx, namespace, name string) (keys map[string]struct{}, exists bool, err error)
}

// ImageChecker reports whether an image ref is resolvable in its registry.
// An error is a genuine check failure (registry unreachable for a reason
// other than not-found) the caller may choose to surface; a clean
// (false, nil) means "definitely not present".
type ImageChecker interface {
	ImageExists(ctx context.Context, ref string) (exists bool, err error)
}

// PreflightOpts bundles everything Preflight needs: the rendered manifests
// to scan, the target cluster context + namespace the Secret checks run
// against, and the injected getter/checker.
type PreflightOpts struct {
	// Manifests is the rendered `---`-separated YAML stream about to be
	// applied. Refs are collected from it.
	Manifests string

	// Context is the DECLARED kubectl context (forge.K8sCluster.cluster)
	// the Secret checks run against — the SAME context the apply targets,
	// never the ambient one. Empty disables the Secret check (host-only /
	// compose env with nothing to verify against a cluster).
	Context string

	// Namespace is the namespace the referenced Secrets are expected to
	// live in (the deploy's target namespace).
	Namespace string

	// Secrets resolves Secret keys against the target cluster.
	Secrets SecretGetter

	// Images checks image existence against the registry.
	Images ImageChecker

	// SkipImageRef, when non-nil, returns true for image refs that should
	// NOT be checked (e.g. local k3d / registry.localhost refs in a dev
	// loop). A nil func checks every image.
	SkipImageRef func(ref string) bool
}

// PreflightResult is the structured outcome of a preflight run — the
// grouped missing-by-secret and missing-images sets. OK reports whether
// the deploy may proceed.
type PreflightResult struct {
	// MissingSecretKeys maps "<namespace>/<secret>" to the sorted list of
	// referenced keys that are absent (or the whole Secret, rendered as a
	// single "<the Secret itself>" marker when the Secret doesn't exist).
	MissingSecretKeys map[string][]string
	// MissingImages is the sorted list of image refs that don't resolve.
	MissingImages []string
}

// OK reports whether nothing was found missing.
func (r PreflightResult) OK() bool {
	return len(r.MissingSecretKeys) == 0 && len(r.MissingImages) == 0
}

// wholeSecretMarker is the placeholder listed under a Secret that doesn't
// exist at all (so every referenced key is unsatisfiable). Distinct from a
// real key name so the report reads clearly.
const wholeSecretMarker = "(the Secret does not exist)"

// Preflight runs the deployability checks and returns a grouped, actionable
// error when anything is missing — or nil to proceed. It is safe under
// --dry-run (a pure read-only check that applies nothing). When there is
// nothing to check (no Secret refs and no checkable images) it is a no-op.
//
// Checks run concurrently: one GetSecretKeys per DISTINCT Secret and one
// ImageExists per DISTINCT image, in parallel, so the happy path adds one
// round-trip's latency rather than one-per-ref.
func Preflight(ctx context.Context, opts PreflightOpts) error {
	refs := CollectManifestRefs(opts.Manifests)

	result, err := runPreflightChecks(ctx, opts, refs)
	if err != nil {
		return err
	}
	if result.OK() {
		return nil
	}
	return fmt.Errorf("%s", FormatPreflightReport(result))
}

// runPreflightChecks performs the secret + image lookups concurrently and
// assembles the PreflightResult. Split out from Preflight so tests can
// assert on the structured result directly.
func runPreflightChecks(ctx context.Context, opts PreflightOpts, refs ManifestRefs) (PreflightResult, error) {
	result := PreflightResult{MissingSecretKeys: map[string][]string{}}

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		firstErr error
	)
	recordErr := func(e error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		mu.Unlock()
	}

	// Secret checks — one lookup per distinct Secret, only when a getter
	// and a target context are configured.
	if opts.Secrets != nil && strings.TrimSpace(opts.Context) != "" {
		for name, keys := range refs.Secrets {
			name, keys := name, keys
			wg.Add(1)
			go func() {
				defer wg.Done()
				present, exists, err := opts.Secrets.GetSecretKeys(ctx, opts.Context, opts.Namespace, name)
				if err != nil {
					recordErr(fmt.Errorf("preflight: read Secret %s/%s: %w", opts.Namespace, name, err))
					return
				}
				missing := missingSecretKeys(keys, present, exists)
				if len(missing) == 0 {
					return
				}
				sort.Strings(missing)
				mu.Lock()
				result.MissingSecretKeys[opts.Namespace+"/"+name] = missing
				mu.Unlock()
			}()
		}
	}

	// Image checks — one lookup per distinct image, skipping refs the
	// caller marks (local registries) when an image checker is configured.
	if opts.Images != nil {
		for ref := range refs.Images {
			ref := ref
			if opts.SkipImageRef != nil && opts.SkipImageRef(ref) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				exists, err := opts.Images.ImageExists(ctx, ref)
				if err != nil {
					recordErr(fmt.Errorf("preflight: check image %q: %w", ref, err))
					return
				}
				if exists {
					return
				}
				mu.Lock()
				result.MissingImages = append(result.MissingImages, ref)
				mu.Unlock()
			}()
		}
	}

	wg.Wait()
	if firstErr != nil {
		return PreflightResult{}, firstErr
	}
	sort.Strings(result.MissingImages)
	return result, nil
}

// missingSecretKeys returns the referenced keys not satisfied by the
// target Secret. When the Secret doesn't exist, the whole-Secret marker is
// returned (every key is unsatisfiable). A referenced key of "" (whole-
// Secret envFrom reference) is satisfied as long as the Secret exists.
func missingSecretKeys(referenced, present map[string]struct{}, exists bool) []string {
	if !exists {
		return []string{wholeSecretMarker}
	}
	var missing []string
	for key := range referenced {
		if key == "" {
			// Whole-Secret reference — satisfied by the Secret existing.
			continue
		}
		if _, ok := present[key]; !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

// FormatPreflightReport renders the grouped, actionable failure report. The
// shape is deliberately scannable: one block per missing Secret, then the
// missing-images block, then the remediation footer.
func FormatPreflightReport(r PreflightResult) string {
	var b strings.Builder
	b.WriteString("deploy preflight failed — the live target is missing dependencies the rendered manifests require:\n")

	if len(r.MissingSecretKeys) > 0 {
		b.WriteString("\n")
		names := make([]string, 0, len(r.MissingSecretKeys))
		for k := range r.MissingSecretKeys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			b.WriteString(fmt.Sprintf("  Secret %s missing keys: [%s]\n",
				name, strings.Join(r.MissingSecretKeys[name], ", ")))
		}
	}

	if len(r.MissingImages) > 0 {
		b.WriteString("\n  Images not found:\n")
		for _, img := range r.MissingImages {
			b.WriteString(fmt.Sprintf("    - %s\n", img))
		}
	}

	b.WriteString("\nNothing was applied. Fix the gaps, then re-run the deploy:\n")
	if len(r.MissingSecretKeys) > 0 {
		b.WriteString("  - provision the missing Secret key(s) in the target cluster/namespace\n")
	}
	if len(r.MissingImages) > 0 {
		b.WriteString("  - build + push the missing image(s) to the registry\n")
	}
	b.WriteString("  - or pass --skip-preflight to bypass this check (you accept the risk of a crash-on-apply).")
	return b.String()
}

// KubectlSecretGetter is the live SecretGetter: it reads a Secret's `.data`
// keys from the target cluster via `kubectl --context <ctx> get secret
// <name> -n <ns> -o json`, threading the DECLARED context per command (the
// same per-command --context discipline the rest of the apply path uses, so
// the check never trusts the ambient context). A not-found Secret returns
// exists=false rather than an error so the preflight reports it cleanly.
type KubectlSecretGetter struct{}

// GetSecretKeys returns the keys of the named Secret's `.data` in namespace.
func (KubectlSecretGetter) GetSecretKeys(ctx context.Context, kctx, namespace, name string) (map[string]struct{}, bool, error) {
	args := []string{"get", "secret", name, "-o", "json"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// `kubectl get` exits non-zero AND prints "NotFound" when the
		// Secret is absent — that's a clean "missing", not a lookup
		// failure. Anything else (RBAC denial, unreachable apiserver,
		// kubectl misconfig) is a real error that aborts the preflight.
		if strings.Contains(string(out), "NotFound") || strings.Contains(string(out), "not found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("kubectl get secret: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var parsed struct {
		Data map[string]string `json:"data"`
	}
	if jerr := json.Unmarshal(out, &parsed); jerr != nil {
		return nil, false, fmt.Errorf("parse secret json: %w", jerr)
	}
	keys := make(map[string]struct{}, len(parsed.Data))
	for k := range parsed.Data {
		keys[k] = struct{}{}
	}
	return keys, true, nil
}

// DockerImageChecker is the live ImageChecker: it resolves an image ref via
// `docker manifest inspect <ref>` (a cheap registry HEAD, no pull). Local /
// HTTP registries get --insecure. A clean non-zero exit means "not present"
// (exists=false, nil err); forge can't distinguish absent-manifest from
// other manifest-API failures, so any failure is reported as not-found
// rather than aborting — the conservative choice for a check whose miss is
// already actionable ("build + push the image").
type DockerImageChecker struct{}

// ImageExists reports whether `docker manifest inspect` resolves ref.
func (DockerImageChecker) ImageExists(ctx context.Context, ref string) (bool, error) {
	args := []string{"manifest", "inspect"}
	if registryRefIsInsecure(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	if cmd.Run() == nil {
		return true, nil
	}
	return false, nil
}

// registryRefIsInsecure reports whether an image ref points at a registry
// that should be treated as plain HTTP (the dev-cluster k3d registries
// forge stands up). Mirrors the cli package's isInsecureRegistry — kept
// here so the cluster package's checker is self-contained.
func registryRefIsInsecure(ref string) bool {
	host, _, _ := strings.Cut(ref, "/")
	hostOnly, _, _ := strings.Cut(host, ":")
	switch hostOnly {
	case "localhost", "127.0.0.1", "registry.localhost":
		return true
	}
	return false
}

// LocalImageRef reports whether an image ref targets a LOCAL registry
// (k3d / registry.localhost / localhost:<port> / 127.0.0.1). The deploy
// path uses it as the default SkipImageRef so a local dev loop isn't failed
// by an image that only exists in the in-cluster registry the manifest
// checker can't reach the same way. Best-effort: a missing local image
// still surfaces as an ImagePullBackOff, but local dev iterates fast and a
// hard preflight failure there is more friction than value.
func LocalImageRef(ref string) bool {
	return registryRefIsInsecure(ref)
}
