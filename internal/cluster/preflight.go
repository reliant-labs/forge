package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Preflight is the deploy-time "deployability contract": before a single
// manifest is applied, it checks that the rendered bundle's external
// dependencies actually exist on the LIVE target — every Secret KEY and
// ConfigMap KEY a container references is present in the target cluster, and
// every container image: is resolvable in its registry. When something is
// missing it returns a single grouped error naming EVERYTHING missing at
// once, so the user fixes it all in one pass instead of discovering each
// gap one-at-a-time as pods crash (CreateContainerConfigError /
// ImagePullBackOff) over a live rollout.
//
// The checks are injected (SecretGetter / ConfigMapGetter / ImageChecker) so
// the orchestration is unit-testable without a live cluster or registry; the
// deploy path wires the real kubectl-/docker-backed implementations (see
// KubectlSecretGetter / KubectlConfigMapGetter / DockerImageChecker).

// ManifestRefs is the set of external references a rendered manifest bundle
// depends on at schedule time: the Secret / ConfigMap (name, key) pairs its
// containers project into env or mount, and the distinct container images it
// runs.
type ManifestRefs struct {
	// Secrets maps a Secret name to the set of keys referenced from it. A
	// key of "" (present in the set) means a whole-Secret reference
	// (envFrom secretRef, a volume mount, an imagePullSecret) — verify
	// existence only.
	Secrets map[string]map[string]struct{}
	// ConfigMaps maps a ConfigMap name to the set of keys referenced from
	// it. A key of "" means a whole-ConfigMap reference (envFrom
	// configMapRef or a whole-ConfigMap volume mount) — verify existence
	// only. A missing ConfigMap key fails a pod identically to a missing
	// Secret key (CreateContainerConfigError).
	ConfigMaps map[string]map[string]struct{}
	// Images is the set of distinct container image refs in the bundle.
	Images map[string]struct{}
}

// CollectManifestRefs walks a `---`-separated multi-doc YAML manifest stream
// and collects every Secret reference (secretKeyRef, envFrom secretRef,
// secret-backed volumes, projected secret sources, imagePullSecrets), every
// ConfigMap reference (configMapKeyRef, envFrom configMapRef, configMap-backed
// volumes, projected configMap sources), and every container image. It
// recurses the whole document tree rather than hard-coding pod-spec paths, so
// it picks references up uniformly across Deployments, StatefulSets,
// DaemonSets, Jobs, CronJobs, Pods, and any nested template — including init
// containers and CronJob/Job pod templates. Malformed documents are skipped
// (best-effort, mirroring the other manifest scanners in this package).
func CollectManifestRefs(manifests string) ManifestRefs {
	refs := ManifestRefs{
		Secrets:    map[string]map[string]struct{}{},
		ConfigMaps: map[string]map[string]struct{}{},
		Images:     map[string]struct{}{},
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
// Secret / ConfigMap reference and container image it finds into refs. The
// recursion is shape-driven (it matches on the well-known key names wherever
// they appear), so it covers every pod-template site uniformly.
func collectRefs(node any, refs *ManifestRefs) {
	switch v := node.(type) {
	case map[string]any:
		collectSecretRefs(v, refs)
		collectConfigMapRefs(v, refs)
		collectImageRef(v, refs)
		for _, child := range v {
			collectRefs(child, refs)
		}
	case []any:
		for _, child := range v {
			collectRefs(child, refs)
		}
	}
}

// collectSecretRefs records every Secret reference shape rooted at this map:
// a keyed projection (secretKeyRef), a whole-Secret env projection (envFrom
// secretRef), a secret-backed volume (volumes[].secret.secretName), a
// projected secret source (sources[].secret.name), and an image-pull Secret
// (imagePullSecrets[].name). The volume / projected / imagePullSecret shapes
// are existence-only (key ""): forge can't know which keys the runtime reads,
// and an imagePullSecret is consumed whole. These cover the
// `ClusterClient external=True` out-of-band kubeconfig Secret that forge does
// NOT mint — mounted via a secret volume — so the gate catches it instead of
// letting the pod crash on first schedule.
func collectSecretRefs(v map[string]any, refs *ManifestRefs) {
	// secretKeyRef: {name, key} — a single (Secret, key) projection.
	if skr, ok := mapAt(v, "secretKeyRef"); ok {
		if name := stringAt(skr, "name"); name != "" {
			addRef(refs.Secrets, name, stringAt(skr, "key"))
		}
	}
	// envFrom secretRef: {name} — projects the WHOLE Secret. Existence only.
	if sr, ok := mapAt(v, "secretRef"); ok {
		if name := stringAt(sr, "name"); name != "" {
			addRef(refs.Secrets, name, "")
		}
	}
	// volumes[].secret.secretName — a secret-backed volume mount.
	if sv, ok := mapAt(v, "secret"); ok {
		// A pod-volume secret source keys the name as `secretName`; a
		// projected source keys it as `name`. Accept either so both the
		// `volumes[].secret` and `sources[].secret` shapes are covered.
		if name := stringAt(sv, "secretName"); name != "" {
			addRef(refs.Secrets, name, "")
		}
		if name := stringAt(sv, "name"); name != "" {
			addRef(refs.Secrets, name, "")
		}
	}
	// imagePullSecrets[].name — a list of {name} Secret references.
	collectNamedListRefs(v, "imagePullSecrets", refs.Secrets)
}

// collectConfigMapRefs records every ConfigMap reference shape rooted at this
// map, mirroring collectSecretRefs: a keyed projection (configMapKeyRef), a
// whole-ConfigMap env projection (envFrom configMapRef), a configMap-backed
// volume (volumes[].configMap.name), and a projected configMap source
// (sources[].configMap.name). control-plane projects non-sensitive config to
// ConfigMaps, so a missing ConfigMap key is a real CreateContainerConfigError
// class the gate must catch.
func collectConfigMapRefs(v map[string]any, refs *ManifestRefs) {
	// configMapKeyRef: {name, key} — a single (ConfigMap, key) projection.
	if ckr, ok := mapAt(v, "configMapKeyRef"); ok {
		if name := stringAt(ckr, "name"); name != "" {
			addRef(refs.ConfigMaps, name, stringAt(ckr, "key"))
		}
	}
	// envFrom configMapRef / volumes[].configMap / sources[].configMap:
	// each is {name} — existence only. All three use the `configMap` key
	// EXCEPT envFrom, which uses `configMapRef`; handle both.
	if cmr, ok := mapAt(v, "configMapRef"); ok {
		if name := stringAt(cmr, "name"); name != "" {
			addRef(refs.ConfigMaps, name, "")
		}
	}
	if cm, ok := mapAt(v, "configMap"); ok {
		if name := stringAt(cm, "name"); name != "" {
			addRef(refs.ConfigMaps, name, "")
		}
	}
}

// collectImageRef records a container image ref. Only a STRING image is a
// container ref; objects keyed "image" elsewhere (rare) are ignored.
func collectImageRef(v map[string]any, refs *ManifestRefs) {
	if img, ok := v["image"]; ok {
		if s, ok := img.(string); ok && strings.TrimSpace(s) != "" {
			refs.Images[strings.TrimSpace(s)] = struct{}{}
		}
	}
}

// collectNamedListRefs records an existence-only (key "") reference for every
// {name: ...} entry in the list at v[key]. Used for imagePullSecrets.
func collectNamedListRefs(v map[string]any, key string, into map[string]map[string]struct{}) {
	raw, ok := v[key]
	if !ok {
		return
	}
	list, ok := raw.([]any)
	if !ok {
		return
	}
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if name := stringAt(m, "name"); name != "" {
				addRef(into, name, "")
			}
		}
	}
}

// addRef records (name, key) into a name→keyset map (Secrets or ConfigMaps),
// lazily allocating the inner set.
func addRef(into map[string]map[string]struct{}, name, key string) {
	if into[name] == nil {
		into[name] = map[string]struct{}{}
	}
	into[name][key] = struct{}{}
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

// ConfigMapGetter resolves which keys exist on a ConfigMap in a target
// cluster, mirroring SecretGetter. exists=false means the ConfigMap is
// absent; an error is a genuine lookup failure that aborts the preflight.
type ConfigMapGetter interface {
	GetConfigMapKeys(ctx context.Context, kctx, namespace, name string) (keys map[string]struct{}, exists bool, err error)
}

// ImageChecker reports whether an image ref is resolvable in its registry.
//
// The outcomes are deliberately distinct, because a deploy GATE must not block
// on its own blind spots — but it equally must not SILENTLY PASS an image it
// could not confirm is present:
//
//   - (true, nil)   — the image is present. Proceed.
//   - (false, nil)  — the image is CONFIRMED absent (a real registry
//     not-found / MANIFEST_UNKNOWN). BLOCK the deploy.
//   - (false, err) where errors.Is(err, ErrImageCheckAuthDenied) — the
//     registry refused the lookup with an auth-class denial (denied /
//     unauthorized / 403 forbidden). The image's presence is UNKNOWN, and on
//     a private prod registry an auth-denied manifest lookup is exactly what a
//     genuinely-missing image looks like. BLOCK (cannot confirm), with an
//     actionable message — but the operator can --skip-preflight after
//     verifying. Failing open here is what produces ImagePullBackOff in prod.
//   - (false, err) where errors.Is(err, ErrImageCheckInconclusive) — the
//     check could not reach the registry AT ALL (DNS / connection refused /
//     i/o timeout / docker daemon down). That is a transport problem, not a
//     statement about the image, and the CLUSTER may still pull fine, so the
//     preflight WARNS and PROCEEDS rather than false-failing a present image.
//   - (_, err) for any other error — a genuine checker failure the caller
//     surfaces (aborts the preflight).
type ImageChecker interface {
	ImageExists(ctx context.Context, ref string) (exists bool, err error)
}

// ImageArchChecker reports the architecture(s) an image ref advertises — the
// `architecture` field of a single-platform manifest, or every platform's
// architecture in a multi-arch manifest index. A deploy GATE compares this to
// the TARGET cluster's declared node arch and BLOCKS on mismatch, turning the
// runtime `exec format error` (an amd64 node trying to run an arm64 binary —
// the 2026-06-24 cross-env incident) into a pre-apply failure.
//
// Contract mirrors ImageChecker's "don't block on a blind spot, don't pass on
// a confirmed problem" discipline:
//
//   - (archs, nil)  — the image's architectures, resolved. Compare + gate.
//   - (nil, err) where errors.Is(err, ErrImageCheckInconclusive) — the arch
//     could not be read (transport failure, image absent — already reported by
//     the existence check, daemon down). The arch is UNKNOWN, so the gate
//     WARNS and proceeds rather than false-failing. Mismatch can only be
//     asserted on a KNOWN arch.
//   - (nil, err) for anything else — a genuine checker failure surfaced by the
//     caller.
type ImageArchChecker interface {
	ImageArchitectures(ctx context.Context, ref string) (archs []string, err error)
}

// ErrImageCheckInconclusive marks an image check that could not reach the
// registry at all — a transport-class failure (DNS, connection refused, i/o
// timeout, docker daemon down). That says nothing about the image: the CLUSTER
// may still pull it fine, so the preflight treats it as a non-blocking warning
// ("couldn't verify image X: <reason>; proceeding") rather than a confirmed
// miss. Wrap it with %w to carry the underlying reason.
var ErrImageCheckInconclusive = errors.New("image existence could not be verified")

// ErrImageCheckAuthDenied marks an image check the registry refused with an
// auth-class denial (denied / unauthorized / 403 forbidden). Unlike a
// transport error, this DID reach the registry — it just wouldn't answer
// whether the manifest exists. On a private registry (ghcr.io private
// packages, GCP Artifact Registry) a genuinely-MISSING image returns exactly
// this denial, so treating it as inconclusive would let the gate silently pass
// a missing image and ImagePullBackOff in prod. The preflight therefore BLOCKS
// on it (cannot confirm the image is present) with a message naming both
// causes — image not pushed, OR the deploy host lacks pull creds — and the
// --skip-preflight escape. Wrap it with %w to carry the underlying reason.
var ErrImageCheckAuthDenied = errors.New("image existence could not be confirmed (registry denied the lookup)")

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

	// ConfigMaps resolves ConfigMap keys against the target cluster. Like
	// Secrets, the check runs only when this and Context are both set.
	ConfigMaps ConfigMapGetter

	// Images checks image existence against the registry.
	Images ImageChecker

	// ImageArch reports an image's advertised architecture(s) for the arch
	// gate. Nil disables the gate entirely (the existence check still runs).
	ImageArch ImageArchChecker

	// TargetArch is the DECLARED node architecture of the target cluster
	// (GOARCH form: "amd64" / "arm64"), resolved from the env's KCL
	// `deploy.Cluster.platform`. When set AND ImageArch is configured, each
	// checked image's architectures are compared to it and a mismatch BLOCKS
	// the deploy (the exec-format-error gate). EMPTY means the env hasn't
	// declared a platform yet — the gate is INERT (WARN-don't-block) so envs
	// that predate the platform field (incl. the local e2e path) are never
	// false-failed.
	TargetArch string

	// SkipImageRef, when non-nil, returns true for image refs that should
	// NOT be checked (e.g. local k3d / registry.localhost refs in a dev
	// loop). A nil func checks every image (both existence AND arch).
	SkipImageRef func(ref string) bool
}

// PreflightResult is the structured outcome of a preflight run — the
// grouped missing-by-secret / missing-by-configmap / missing-images sets,
// plus non-blocking image warnings. OK reports whether the deploy may
// proceed (warnings do NOT block).
type PreflightResult struct {
	// MissingSecretKeys maps "<namespace>/<secret>" to the sorted list of
	// referenced keys that are absent (or the whole Secret, rendered as a
	// single marker when the Secret doesn't exist).
	MissingSecretKeys map[string][]string
	// MissingConfigMapKeys maps "<namespace>/<configmap>" to the sorted
	// list of referenced keys that are absent (or the whole-ConfigMap
	// marker when the ConfigMap doesn't exist).
	MissingConfigMapKeys map[string][]string
	// MissingImages is the sorted list of image refs CONFIRMED absent.
	MissingImages []string
	// UnverifiableImages is the sorted list of "<ref>: <reason>" entries for
	// images whose registry lookup was AUTH-DENIED (denied / unauthorized /
	// 403). Their presence is unknown and a deploy gate must not silently
	// pass an image it cannot confirm — these BLOCK, with a message naming
	// both possible causes (image not pushed, OR the deploy host lacks pull
	// creds for the registry).
	UnverifiableImages []string
	// ImageWarnings is the sorted list of "could not verify" notes for
	// images whose existence check was inconclusive — a TRANSPORT failure
	// (DNS, connection refused, i/o timeout, docker daemon down) that says
	// nothing about the image. These do NOT block the deploy — they are
	// printed so the user knows the gate couldn't vouch for those images.
	ImageWarnings []string
	// ArchMismatchImages is the sorted list of "<ref>: image is <archs>,
	// cluster nodes are <target>" entries for images whose advertised
	// architecture does NOT include the target cluster's declared arch. These
	// BLOCK — running an arm64 image on amd64 nodes is the exec-format-error
	// crash, caught here before a single pod schedules. Only populated when a
	// target arch is DECLARED (an undeclared target arch can't assert a
	// mismatch).
	ArchMismatchImages []string
	// ArchWarnings is the sorted list of advisory notes for images whose arch
	// could not be read (transport failure / inconclusive). These do NOT
	// block — a mismatch can only be asserted on a known arch.
	ArchWarnings []string
}

// OK reports whether nothing was found missing. Inconclusive image warnings
// are advisory and do NOT make the result fail.
func (r PreflightResult) OK() bool {
	return len(r.MissingSecretKeys) == 0 &&
		len(r.MissingConfigMapKeys) == 0 &&
		len(r.MissingImages) == 0 &&
		len(r.UnverifiableImages) == 0 &&
		len(r.ArchMismatchImages) == 0
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
	// Inconclusive image checks are advisory — surface them whether the
	// run blocks or proceeds, so the user knows the gate couldn't vouch
	// for those images (and isn't surprised by a later ImagePullBackOff).
	for _, w := range result.ImageWarnings {
		fmt.Printf("preflight: %s\n", w)
	}
	// Arch checks that couldn't read an image's architecture are advisory
	// too (an undeclared target arch, or an unreadable manifest) — surface
	// them so a later exec-format-error isn't a surprise.
	for _, w := range result.ArchWarnings {
		fmt.Printf("preflight: %s\n", w)
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
	result := PreflightResult{
		MissingSecretKeys:    map[string][]string{},
		MissingConfigMapKeys: map[string][]string{},
	}

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

	hasContext := strings.TrimSpace(opts.Context) != ""

	// Secret checks — one lookup per distinct Secret, only when a getter
	// and a target context are configured.
	if opts.Secrets != nil && hasContext {
		checkKeyedResources(ctx, &wg, refs.Secrets, "Secret", opts.Namespace,
			func(ctx context.Context, name string) (map[string]struct{}, bool, error) {
				return opts.Secrets.GetSecretKeys(ctx, opts.Context, opts.Namespace, name)
			},
			result.MissingSecretKeys, &mu, recordErr)
	}

	// ConfigMap checks — symmetric with Secrets. A missing ConfigMap key
	// fails a pod identically (CreateContainerConfigError).
	if opts.ConfigMaps != nil && hasContext {
		checkKeyedResources(ctx, &wg, refs.ConfigMaps, "ConfigMap", opts.Namespace,
			func(ctx context.Context, name string) (map[string]struct{}, bool, error) {
				return opts.ConfigMaps.GetConfigMapKeys(ctx, opts.Context, opts.Namespace, name)
			},
			result.MissingConfigMapKeys, &mu, recordErr)
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
					// An auth-denied lookup did reach the registry but
					// wouldn't say whether the image is present — and on a
					// private registry a MISSING image looks exactly like
					// this. The gate must not silently pass an image it
					// can't confirm, so this BLOCKS (the operator can
					// --skip-preflight after a 5-second check).
					if errors.Is(err, ErrImageCheckAuthDenied) {
						mu.Lock()
						result.UnverifiableImages = append(result.UnverifiableImages,
							fmt.Sprintf("%s (%v)", ref, err))
						mu.Unlock()
						return
					}
					// A transport-class inconclusive check (DNS / connection
					// refused / timeout for a registry the CLUSTER may still
					// pull) says nothing about the image — warn and proceed.
					// Any other checker error is a genuine failure that
					// aborts the preflight.
					if errors.Is(err, ErrImageCheckInconclusive) {
						mu.Lock()
						result.ImageWarnings = append(result.ImageWarnings,
							fmt.Sprintf("couldn't verify image %q: %v; proceeding", ref, err))
						mu.Unlock()
						return
					}
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

	// Arch gate — one manifest-inspect per distinct image, comparing the
	// image's advertised architecture(s) to the TARGET cluster's declared
	// arch. Only runs when BOTH an arch checker and a declared target arch
	// are configured: an undeclared target arch can't assert a mismatch
	// (WARN-don't-block), so the gate is inert for envs that predate the
	// platform field (incl. the local e2e path). Skips the same local-
	// registry refs as the existence check.
	target := strings.TrimSpace(opts.TargetArch)
	if opts.ImageArch != nil && target != "" {
		for ref := range refs.Images {
			ref := ref
			if opts.SkipImageRef != nil && opts.SkipImageRef(ref) {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				archs, aerr := opts.ImageArch.ImageArchitectures(ctx, ref)
				if aerr != nil {
					// Couldn't read the arch (transport / image-absent —
					// already reported by the existence check / daemon down).
					// A mismatch can only be asserted on a KNOWN arch, so warn
					// and proceed rather than false-failing.
					if errors.Is(aerr, ErrImageCheckInconclusive) {
						mu.Lock()
						result.ArchWarnings = append(result.ArchWarnings,
							fmt.Sprintf("couldn't read architecture of image %q: %v; skipping arch gate for it", ref, aerr))
						mu.Unlock()
						return
					}
					recordErr(fmt.Errorf("preflight: read image arch %q: %w", ref, aerr))
					return
				}
				if archMatchesTarget(archs, target) {
					return
				}
				mu.Lock()
				result.ArchMismatchImages = append(result.ArchMismatchImages,
					fmt.Sprintf("%s: image is %s, cluster nodes are %s — rebuild for the target platform (exec-format-error class)",
						ref, archList(archs), target))
				mu.Unlock()
			}()
		}
	}

	wg.Wait()
	if firstErr != nil {
		return PreflightResult{}, firstErr
	}
	sort.Strings(result.MissingImages)
	sort.Strings(result.UnverifiableImages)
	sort.Strings(result.ImageWarnings)
	sort.Strings(result.ArchMismatchImages)
	sort.Strings(result.ArchWarnings)
	return result, nil
}

// archMatchesTarget reports whether the target cluster arch is among the
// image's advertised architectures. A multi-arch index that includes the
// target (e.g. ["amd64","arm64"] for an amd64 target) MATCHES — the kubelet
// selects the right manifest. An empty arch set is treated as "unknown" →
// matches (don't block on a blind spot; the warn path handles unreadable
// arch). Comparison is case-insensitive and trims whitespace.
func archMatchesTarget(archs []string, target string) bool {
	if len(archs) == 0 {
		return true
	}
	t := strings.ToLower(strings.TrimSpace(target))
	for _, a := range archs {
		if strings.ToLower(strings.TrimSpace(a)) == t {
			return true
		}
	}
	return false
}

// archList renders an image's architecture set for a report message:
// "arm64", or "[amd64 arm64]" for a multi-arch image, "unknown" when empty.
func archList(archs []string) string {
	switch len(archs) {
	case 0:
		return "unknown"
	case 1:
		return archs[0]
	default:
		return "[" + strings.Join(archs, " ") + "]"
	}
}

// checkKeyedResources fans out one existence/key lookup per distinct named
// resource (Secret or ConfigMap) and records the missing keys into out under
// "<namespace>/<name>". getKeys returns (presentKeys, exists, err); a lookup
// error aborts the preflight via recordErr. Shared by the Secret and
// ConfigMap checks so both behave identically.
func checkKeyedResources(
	ctx context.Context,
	wg *sync.WaitGroup,
	refs map[string]map[string]struct{},
	kind, namespace string,
	getKeys func(ctx context.Context, name string) (map[string]struct{}, bool, error),
	out map[string][]string,
	mu *sync.Mutex,
	recordErr func(error),
) {
	for name, keys := range refs {
		name, keys := name, keys
		wg.Add(1)
		go func() {
			defer wg.Done()
			present, exists, err := getKeys(ctx, name)
			if err != nil {
				recordErr(fmt.Errorf("preflight: read %s %s/%s: %w", kind, namespace, name, err))
				return
			}
			missing := missingSecretKeys(keys, present, exists)
			if len(missing) == 0 {
				return
			}
			sort.Strings(missing)
			mu.Lock()
			out[namespace+"/"+name] = missing
			mu.Unlock()
		}()
	}
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

	writeMissingKeyBlock(&b, "Secret", r.MissingSecretKeys)
	writeMissingKeyBlock(&b, "ConfigMap", r.MissingConfigMapKeys)

	if len(r.MissingImages) > 0 {
		b.WriteString("\n  Images not found:\n")
		for _, img := range r.MissingImages {
			b.WriteString(fmt.Sprintf("    - %s\n", img))
		}
	}

	if len(r.UnverifiableImages) > 0 {
		b.WriteString("\n  Images that could not be confirmed (registry denied the lookup):\n")
		for _, img := range r.UnverifiableImages {
			b.WriteString(fmt.Sprintf("    - %s\n", img))
		}
	}

	if len(r.ArchMismatchImages) > 0 {
		b.WriteString("\n  Images built for the WRONG architecture (would crash with exec format error):\n")
		for _, img := range r.ArchMismatchImages {
			b.WriteString(fmt.Sprintf("    - %s\n", img))
		}
	}

	b.WriteString("\nNothing was applied. Fix the gaps, then re-run the deploy:\n")
	if len(r.MissingSecretKeys) > 0 {
		b.WriteString("  - provision the missing Secret key(s) in the target cluster/namespace\n")
	}
	if len(r.MissingConfigMapKeys) > 0 {
		b.WriteString("  - provision the missing ConfigMap key(s) in the target cluster/namespace\n")
	}
	if len(r.MissingImages) > 0 {
		b.WriteString("  - build + push the missing image(s) to the registry\n")
	}
	if len(r.UnverifiableImages) > 0 {
		b.WriteString("  - the registry refused to confirm the image(s) above — this means ONE of:\n")
		b.WriteString("      (a) the image was never pushed (build + push it), OR\n")
		b.WriteString("      (b) the deploy host lacks pull credentials for that registry\n")
		b.WriteString("          (e.g. `docker login ghcr.io`, `gcloud auth configure-docker`).\n")
		b.WriteString("    Verify the image really exists, then re-run; only --skip-preflight once you've confirmed it.\n")
	}
	if len(r.ArchMismatchImages) > 0 {
		b.WriteString("  - rebuild the wrong-arch image(s) for the target cluster's node architecture\n")
		b.WriteString("      (forge build --target-arch <arch>, or set deploy.target_arch / the env's K8sCluster.platform).\n")
	}
	b.WriteString("  - or pass --skip-preflight to bypass this check (you accept the risk of a crash-on-apply).")
	return b.String()
}

// writeMissingKeyBlock renders one "<kind> <ns/name> missing keys: [...]"
// line per entry, in deterministic name order. No-op for an empty map.
func writeMissingKeyBlock(b *strings.Builder, kind string, missing map[string][]string) {
	if len(missing) == 0 {
		return
	}
	b.WriteString("\n")
	names := make([]string, 0, len(missing))
	for k := range missing {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, name := range names {
		b.WriteString(fmt.Sprintf("  %s %s missing keys: [%s]\n",
			kind, name, strings.Join(missing[name], ", ")))
	}
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
	return kubectlDataKeys(ctx, kctx, namespace, "secret", name)
}

// KubectlConfigMapGetter is the live ConfigMapGetter: it reads a ConfigMap's
// `.data` keys from the target cluster via `kubectl --context <ctx> get
// configmap <name> -n <ns> -o json`, mirroring KubectlSecretGetter (same
// declared-context discipline, same not-found → exists=false handling).
type KubectlConfigMapGetter struct{}

// GetConfigMapKeys returns the keys of the named ConfigMap's `.data`.
func (KubectlConfigMapGetter) GetConfigMapKeys(ctx context.Context, kctx, namespace, name string) (map[string]struct{}, bool, error) {
	return kubectlDataKeys(ctx, kctx, namespace, "configmap", name)
}

// kubectlDataKeys reads the `.data` keys of a `kubectl get <kind> <name>`
// object in namespace, threading the DECLARED context per command. A
// not-found object returns exists=false (a clean "missing", not a lookup
// failure); any other non-zero exit (RBAC denial, unreachable apiserver,
// kubectl misconfig) is a real error that aborts the preflight. Shared by the
// Secret and ConfigMap getters — both project `.data` keys identically.
func kubectlDataKeys(ctx context.Context, kctx, namespace, kind, name string) (map[string]struct{}, bool, error) {
	args := []string{"get", kind, name, "-o", "json"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "NotFound") || strings.Contains(string(out), "not found") {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("kubectl get %s: %v: %s", kind, err, strings.TrimSpace(string(out)))
	}
	var parsed struct {
		Data map[string]string `json:"data"`
	}
	if jerr := json.Unmarshal(out, &parsed); jerr != nil {
		return nil, false, fmt.Errorf("parse %s json: %w", kind, jerr)
	}
	keys := make(map[string]struct{}, len(parsed.Data))
	for k := range parsed.Data {
		keys[k] = struct{}{}
	}
	return keys, true, nil
}

// DockerImageChecker is the live ImageChecker: it resolves an image ref via
// `docker manifest inspect <ref>` (a cheap registry HEAD, no pull) against the
// LOCAL docker daemon. Local / HTTP registries get --insecure.
//
// As a deploy GATE it must distinguish two very different non-zero exits:
//
//   - A CONFIRMED miss — the registry answered "no such manifest"
//     (MANIFEST_UNKNOWN / "manifest unknown" / "not found" / a 404). The
//     image genuinely isn't there → (false, nil), BLOCK.
//   - An AUTH-DENIED lookup — the registry refused to answer ("denied",
//     "unauthorized", "403 forbidden", "access to the resource is denied").
//     This DID reach the registry but left the image's presence UNKNOWN — and
//     on a PRIVATE registry (ghcr.io private packages, GCP Artifact Registry)
//     a genuinely-MISSING image returns exactly this denial. Passing it as
//     inconclusive would fail OPEN and ImagePullBackOff in prod, so →
//     (false, ErrImageCheckAuthDenied), BLOCK (cannot confirm) — overridable
//     via --skip-preflight once the operator has verified the image.
//   - An INCONCLUSIVE failure — the LOCAL daemon couldn't reach the registry
//     at all: DNS/TLS failure, connection refused, i/o timeout, docker not
//     running. That is a transport problem, not a statement about the image;
//     the CLUSTER may still pull it fine, so blocking here would false-fail a
//     present image. → (false, ErrImageCheckInconclusive), WARN and PROCEED.
//
// The distinction is made by scanning combined stdout+stderr: not-found
// markers first (most specific), then auth-denied markers; anything left is
// treated as inconclusive transport noise.
type DockerImageChecker struct{}

// ImageExists reports whether `docker manifest inspect` resolves ref. See the
// type doc for the confirmed-miss vs inconclusive distinction.
func (DockerImageChecker) ImageExists(ctx context.Context, ref string) (bool, error) {
	args := []string{"manifest", "inspect"}
	if registryRefIsInsecure(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	if imageOutputIsConfirmedMiss(string(out)) {
		return false, nil
	}
	reason := strings.TrimSpace(string(out))
	if reason == "" {
		reason = err.Error()
	}
	// An auth-class denial reached the registry but won't confirm the image.
	// On a private registry a MISSING image is indistinguishable from this,
	// so a deploy gate must BLOCK rather than fail open. Checked before the
	// inconclusive fallback so a denial is never mistaken for transport noise.
	if imageOutputIsAuthDenied(string(out)) {
		return false, fmt.Errorf("%w: %s", ErrImageCheckAuthDenied, reason)
	}
	// Couldn't even reach the registry — transport noise. Surface the
	// daemon's own message as the inconclusive reason so the warning is
	// actionable.
	return false, fmt.Errorf("%w: %s", ErrImageCheckInconclusive, reason)
}

// DockerImageArchChecker is the live ImageArchChecker: it reads an image's
// advertised architecture(s) via `docker manifest inspect <ref>` against the
// registry. It parses BOTH shapes the command can return:
//
//   - a manifest LIST / OCI index — `.manifests[].platform.architecture`,
//     one entry per platform (a multi-arch image). "unknown" attestation
//     entries (buildx provenance/SBOM) are dropped — they aren't runnable.
//   - a single image manifest — the top-level `.architecture` field.
//
// A lookup that can't reach the registry (transport failure, image absent,
// daemon down) is returned as ErrImageCheckInconclusive so the gate WARNS
// rather than blocking — a mismatch can only be asserted on a known arch.
type DockerImageArchChecker struct{}

// ImageArchitectures returns the distinct architectures ref advertises. See
// the type doc for the two manifest shapes handled.
func (DockerImageArchChecker) ImageArchitectures(ctx context.Context, ref string) ([]string, error) {
	args := []string{"manifest", "inspect"}
	if registryRefIsInsecure(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		reason := strings.TrimSpace(string(out))
		if reason == "" {
			reason = err.Error()
		}
		// Any non-zero exit leaves the arch UNKNOWN — existence is the
		// existence check's job; here we only fail open so the gate warns.
		return nil, fmt.Errorf("%w: %s", ErrImageCheckInconclusive, reason)
	}
	return parseManifestArchitectures(out)
}

// parseManifestArchitectures extracts the distinct architectures from a
// `docker manifest inspect` JSON blob — a manifest list (`.manifests[].
// platform.architecture`) or a single image (`.architecture`). "unknown"
// platforms (buildx attestation entries) are dropped. Order is stable
// (sorted) so the report message is deterministic.
func parseManifestArchitectures(raw []byte) ([]string, error) {
	var parsed struct {
		Architecture string `json:"architecture"`
		Manifests    []struct {
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if jerr := json.Unmarshal(raw, &parsed); jerr != nil {
		return nil, fmt.Errorf("%w: parse manifest json: %v", ErrImageCheckInconclusive, jerr)
	}
	seen := map[string]struct{}{}
	var archs []string
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" || a == "unknown" {
			return
		}
		if _, ok := seen[a]; !ok {
			seen[a] = struct{}{}
			archs = append(archs, a)
		}
	}
	for _, m := range parsed.Manifests {
		add(m.Platform.Architecture)
	}
	if len(archs) == 0 {
		add(parsed.Architecture)
	}
	sort.Strings(archs)
	return archs, nil
}

// imageOutputIsConfirmedMiss reports whether `docker manifest inspect`'s
// combined output indicates the image is genuinely ABSENT (a registry
// not-found), as opposed to a transport/auth/network failure that leaves
// existence unknown. Matching is case-insensitive on the registry/CLI markers
// for a missing manifest or repository.
func imageOutputIsConfirmedMiss(out string) bool {
	l := strings.ToLower(out)
	for _, marker := range []string{
		"manifest unknown",
		"manifest_unknown",
		"not found",
		"no such manifest",
		"name unknown",
		"name_unknown",
		"repository name not known",
	} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
}

// imageOutputIsAuthDenied reports whether `docker manifest inspect`'s combined
// output is an AUTH-CLASS denial from the registry — the registry was reached
// but refused to confirm the manifest. On a PRIVATE registry a genuinely-
// missing image surfaces as exactly this denial (ghcr.io private packages, GCP
// Artifact Registry `us-docker.pkg.dev`, Docker Hub private repos), so the
// gate treats it as "cannot confirm → block" rather than failing open.
//
// Matching is case-insensitive. These markers are auth/authorization denials,
// distinct from the transport failures (DNS, connection refused, timeout,
// daemon down) that remain inconclusive.
func imageOutputIsAuthDenied(out string) bool {
	l := strings.ToLower(out)
	for _, marker := range []string{
		"denied",        // "denied: requested access to the resource is denied"
		"unauthorized",  // "unauthorized: authentication required"
		"forbidden",     // "403 Forbidden"
		"403",           // bare 403 status
		"access denied", // some registries phrase it this way
		"requested access to the resource is denied",
		"authentication required",
		"no basic auth credentials", // local daemon has no creds for a private registry
	} {
		if strings.Contains(l, marker) {
			return true
		}
	}
	return false
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
