package cluster

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Preflight is the deploy-time "deployability contract": before a single
// manifest is applied, it checks that the rendered bundle's external
// dependencies actually exist on the LIVE target — every Secret KEY and
// ConfigMap KEY a container references is present in the target cluster, every
// container image: is resolvable in its registry, and every NON-CORE resource
// KIND the bundle renders has a CRD installed on the cluster (so a rendered
// GRPCRoute can't fail `no matches for kind` mid-rollout). When something is
// missing it returns a single grouped error naming EVERYTHING missing at
// once, so the user fixes it all in one pass instead of discovering each
// gap one-at-a-time as pods crash (CreateContainerConfigError /
// ImagePullBackOff) — or kubectl apply errors `no matches for kind` — over a
// live rollout.
//
// The checks are injected (SecretGetter / ConfigMapGetter / ImageChecker) so
// the orchestration is unit-testable without a live cluster or registry; the
// deploy path wires the real kubectl-/docker-backed implementations (see
// KubectlSecretGetter / KubectlConfigMapGetter / DockerImageChecker).

// ManifestGVK is the GroupVersionKind of a rendered manifest document — the
// (apiVersion, kind) pair `kubectl apply` keys a resource on — paired with the
// document's name so the preflight report can point at WHICH manifest needs a
// missing CRD. ApiVersion is the raw `apiVersion:` value ("apps/v1", "v1",
// "gateway.networking.k8s.io/v1"); Kind is the raw `kind:` value.
type ManifestGVK struct {
	ApiVersion string
	Kind       string
	// Name is metadata.name of the document (best-effort, "" when absent) —
	// used only to make the missing-CRD report actionable.
	Name string
}

// group returns the API group of the GVK — the part of apiVersion before the
// "/", or "" for a core-group resource (apiVersion "v1"). Used to decide
// whether a kind is a CORE kind (group "") that every cluster serves, so the
// CRD gate never false-positives on Deployment/Service/ConfigMap/etc.
func (g ManifestGVK) group() string {
	grp, _, found := strings.Cut(g.ApiVersion, "/")
	if !found {
		// No "/" → core group (apiVersion is a bare version like "v1").
		return ""
	}
	return grp
}

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
	// ImagePullSecrets is the set of distinct Secret names referenced via a
	// pod spec's imagePullSecrets[].name. These are the credentials the
	// CLUSTER uses to pull private images — the image-verification path
	// resolves their .dockerconfigjson from the target cluster and uses it to
	// authenticate an otherwise auth-denied registry lookup, so a present
	// private image isn't false-flagged just because the LOCAL docker daemon
	// lacks creds for that registry. A subset of the names in Secrets (which
	// records every Secret reference shape); kept distinct here because only
	// imagePullSecrets carry registry credentials.
	ImagePullSecrets map[string]struct{}
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
		Secrets:          map[string]map[string]struct{}{},
		ConfigMaps:       map[string]map[string]struct{}{},
		Images:           map[string]struct{}{},
		ImagePullSecrets: map[string]struct{}{},
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

// CollectManifestGVKs walks a `---`-separated multi-doc YAML manifest stream
// and returns the GroupVersionKind of every TOP-LEVEL document — the
// (apiVersion, kind, name) `kubectl apply` will create a resource for. Only the
// document's OWN apiVersion/kind is collected (not nested template kinds): the
// cluster must serve the resource type forge actually applies, and a pod
// template's `kind` is an embedded field, never an applied object. Documents
// missing apiVersion or kind (a List wrapper, a malformed doc, a YAML comment-
// only chunk) are skipped — there's nothing to gate. The result preserves
// document order and may contain duplicate GVKs (the served-kind check
// de-dupes).
func CollectManifestGVKs(manifests string) []ManifestGVK {
	var out []ManifestGVK
	for _, doc := range splitDocs(manifests) {
		var head struct {
			ApiVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
			continue
		}
		av := strings.TrimSpace(head.ApiVersion)
		kind := strings.TrimSpace(head.Kind)
		if av == "" || kind == "" {
			continue
		}
		out = append(out, ManifestGVK{ApiVersion: av, Kind: kind, Name: strings.TrimSpace(head.Metadata.Name)})
	}
	return out
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
	// imagePullSecrets[].name — a list of {name} Secret references. Recorded
	// in BOTH Secrets (existence check) and ImagePullSecrets (the
	// registry-credential source the image-verification path authenticates
	// with).
	collectNamedListRefs(v, "imagePullSecrets", refs.Secrets)
	collectImagePullSecretNames(v, refs.ImagePullSecrets)
}

// collectImagePullSecretNames records the bare Secret name of every
// imagePullSecrets[].name entry into out. These are the cluster's registry
// pull credentials; the image-verification path resolves their
// .dockerconfigjson and uses it to authenticate registry lookups the LOCAL
// docker daemon would be denied.
func collectImagePullSecretNames(v map[string]any, out map[string]struct{}) {
	raw, ok := v["imagePullSecrets"]
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
				out[name] = struct{}{}
			}
		}
	}
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

// ServedKindChecker reports which (group, kind) resource types a target
// cluster's API server actually SERVES — its discovery surface
// (`kubectl api-resources`). The CRD preflight uses it to verify, BEFORE a
// single manifest is applied, that every NON-CORE kind the bundle renders has a
// CRD installed on the cluster. The motivating footgun: a rendered GRPCRoute
// (apiVersion gateway.networking.k8s.io/v1) applied to a cluster that never
// installed the Gateway API channel fails mid-rollout with `no matches for kind
// "GRPCRoute"` — AFTER other resources already applied. The check turns that
// deploy-time partial failure into one fail-fast block.
//
// The contract mirrors the image checker's "don't block on a blind spot, don't
// silently pass a confirmed problem" discipline:
//
//   - (served, nil) — discovery succeeded; served is the set of (group/kind)
//     the cluster serves. The gate compares the bundle's kinds against it.
//   - (_, err) — discovery itself FAILED (kubectl not configured, RBAC denial,
//     unreachable apiserver). The cluster's served set is UNKNOWN, so the gate
//     cannot assert a kind is missing — it surfaces the error and aborts the
//     preflight rather than blocking on (or silently passing) every kind.
//
// Served keys are normalized to "<group>/<kind>" with a lowercased group and
// the kind verbatim (CRD kinds are case-sensitive); a core-group kind keys as
// "/<kind>". ServesKind does the comparison so callers never reimplement the
// key shape.
type ServedKindChecker interface {
	// ServedKinds returns the set of resource types the cluster serves, keyed
	// by servedKindKey(group, kind).
	ServedKinds(ctx context.Context, kctx string) (served map[string]struct{}, err error)
}

// servedKindKey normalizes a (group, kind) pair to the key shape ServedKinds
// returns and ServesKind compares against: "<lowercased-group>/<kind>". The
// group is lowercased (API groups are DNS-style, case-insensitive); the kind is
// kept verbatim (Kubernetes kinds are PascalCase and case-sensitive). A core-
// group kind (group "") keys as "/<kind>".
func servedKindKey(group, kind string) string {
	return strings.ToLower(strings.TrimSpace(group)) + "/" + strings.TrimSpace(kind)
}

// ServesKind reports whether served (from a ServedKindChecker) contains the
// (group, kind). Centralizes the key shape so call sites and the live checker
// agree.
func ServesKind(served map[string]struct{}, group, kind string) bool {
	_, ok := served[servedKindKey(group, kind)]
	return ok
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

// PullCredsResolver fetches the CLUSTER's registry pull credentials — the
// `.dockerconfigjson` of a kubernetes.io/dockerconfigjson Secret — from the
// target cluster, so the image-verification path can authenticate a registry
// lookup the LOCAL docker daemon would be denied.
//
// The deploy already collects the bundle's imagePullSecret names for the
// existence preflight (ManifestRefs.ImagePullSecrets); this resolves those
// names to the credential blob the cluster would itself use to pull. The
// returned bytes are a docker config.json `{"auths":{...}}` document (the
// decoded `.dockerconfigjson`). A resolver MAY merge several pull Secrets into
// one config (multiple registries). It returns (nil, nil) when no creds are
// available (no imagePullSecrets, or none readable) — the caller then keeps
// today's local-daemon behaviour rather than regressing. An error is a genuine
// lookup failure (kubectl misconfigured) the caller surfaces.
type PullCredsResolver interface {
	ResolveDockerConfig(ctx context.Context, kctx, namespace string, secretNames []string) (dockerConfigJSON []byte, err error)
}

// CredentialedImageChecker is an OPTIONAL capability an ImageChecker /
// ImageArchChecker may implement: given a docker config dir (a directory
// holding a config.json with the cluster's pull creds), it returns a variant
// of itself that authenticates registry lookups with those creds. The
// preflight uses it to RETRY an auth-denied lookup from the CLUSTER's
// perspective — turning a local-daemon "auth denied" (a false negative for an
// image the cluster can pull) into a TRUE existence/arch verdict. A checker
// that does not implement this is used as-is (no credentialed retry).
type CredentialedImageChecker interface {
	// WithDockerConfigDir returns an ImageChecker that runs `docker` with
	// DOCKER_CONFIG=dir, so private-registry lookups authenticate with the
	// cluster's pull creds materialised there.
	WithDockerConfigDir(dir string) ImageChecker
}

// CredentialedImageArchChecker is the ImageArchChecker analogue of
// CredentialedImageChecker.
type CredentialedImageArchChecker interface {
	WithDockerConfigDir(dir string) ImageArchChecker
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

	// ServedKinds resolves which resource types the target cluster serves, so
	// the CRD preflight can BLOCK a deploy that renders a kind (e.g. GRPCRoute)
	// whose CRD / Gateway API channel isn't installed — before the partial
	// apply that otherwise errors `no matches for kind` mid-rollout. The check
	// runs only when this and Context are both set, and only gates NON-CORE
	// kinds (core kinds like Deployment/Service are served by every cluster, so
	// gating them would false-positive on a discovery blind spot). Nil disables
	// the CRD gate (local dev / nothing to verify).
	ServedKinds ServedKindChecker

	// Images checks image existence against the registry.
	Images ImageChecker

	// ImageArch reports an image's advertised architecture(s) for the arch
	// gate. Nil disables the gate entirely (the existence check still runs).
	ImageArch ImageArchChecker

	// PullCreds resolves the CLUSTER's registry pull credentials (the
	// .dockerconfigjson of the bundle's imagePullSecrets) from the target
	// cluster. When set AND the image checker implements
	// CredentialedImageChecker AND the bundle declares imagePullSecrets, an
	// AUTH-DENIED registry lookup from the LOCAL docker daemon is RETRIED with
	// those creds, so a present private image the cluster can pull yields a TRUE
	// verdict instead of a false BLOCK. Nil (or no imagePullSecrets, or creds
	// not resolvable) leaves the existing local-daemon behaviour unchanged — the
	// auth-denied lookup still BLOCKS (fail-safe, no regression). Resolution
	// runs against opts.Context / opts.Namespace, the same target the apply
	// uses.
	PullCreds PullCredsResolver

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

	// RequiredSecrets are the env's DECLARED external Secret prerequisites
	// (forge.ExternalSecret) — out-of-band Secrets the deploy depends on but
	// forge does NOT create. UNLIKE the secretKeyRef check above (which is
	// driven by what the rendered manifests reference, all in opts.Namespace),
	// a declared prereq carries its OWN namespace (cert-manager's
	// `cloudflare-api-token` lives in the `cert-manager` namespace, not the
	// deploy namespace), so each is checked in its declared namespace. A
	// declared-required-but-absent Secret/key BLOCKS — this is the whole
	// point: it converts "render green, then ACME hangs silently" into a
	// fail-fast pre-apply block. Verified only when opts.Secrets is configured
	// (a SecretGetter against the live target). Empty => no declared prereqs.
	RequiredSecrets []RequiredSecret

	// SecretValues, when set, resolves a Secret's full .data value bytes
	// (base64-decoded) for the cross-secret BYTE-MATCH check: ExternalSecrets
	// sharing a `value_group` must carry IDENTICAL bytes under their keys. A
	// drifted copy (same group, different bytes — a half-rotated credential)
	// is caught here before it ships. Nil => the byte-match compare is skipped
	// (the KCL schema still enforces that a group's members declare the same
	// KEY SET; only the live byte equality needs cluster reads).
	SecretValues SecretValueGetter

	// SecretSupply is the env's bundle-internal Secret SUPPLY for the
	// RENDER-TIME back-propagation gate (CheckSecretSupply): the Secrets the
	// bundle PROVIDES via a forge.KubeconfigSecret mint, a forge.ExternalSecret
	// out-of-band promise, or any other generated/known Secret forge produces.
	// Rendered-stream Secrets (kind: Secret) are collected from Manifests
	// directly, so this need only carry the entity-derived supply. The gate is
	// pure (NO cluster) and ALWAYS runs — it converts a workload mounting a
	// Secret nothing declares (the silent FailedMount / 15-min ContainerCreating
	// rot) into a fail-fast render-time BLOCK. Empty => only rendered-stream
	// Secrets count as supply. See SecretSupply / CheckSecretSupply.
	SecretSupply []SecretSupply
}

// RequiredSecret is one declared external Secret prerequisite the preflight
// verifies against the live target. It mirrors the cli ExternalSecretEntity
// but stays a plain cluster-package struct so this package never depends on
// the cli entity types.
type RequiredSecret struct {
	// Name / Namespace identify the out-of-band Secret. Namespace is the
	// declared namespace (often NOT the deploy namespace).
	Name      string
	Namespace string
	// Keys are the data keys the consumer reads; each must exist.
	Keys []string
	// ValueGroup ties this Secret to others that must carry identical bytes
	// (the cross-secret byte-match group). Empty => standalone.
	ValueGroup string
}

// SecretValueGetter resolves a Secret's decoded .data values for the
// cross-secret byte-match check. exists=false means the Secret is absent
// (already reported by the existence check); an error is a genuine lookup
// failure that aborts the preflight.
type SecretValueGetter interface {
	GetSecretValues(ctx context.Context, kctx, namespace, name string) (values map[string][]byte, exists bool, err error)
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
	// MissingCRDs is the sorted list of "<Kind> (<apiVersion>) — required by
	// <manifest-name>" entries for NON-CORE kinds the bundle renders that the
	// target cluster's API server does NOT serve (no installed CRD / Gateway
	// API channel). These BLOCK — applying such a manifest fails `no matches
	// for kind` mid-rollout, AFTER other resources already applied. Only
	// populated when a ServedKindChecker is configured (an undeclared discovery
	// surface can't assert a kind is missing).
	MissingCRDs []string
	// MissingRequiredSecretKeys maps "<namespace>/<secret>" to the sorted list
	// of declared-required keys absent on the live target (or the whole-Secret
	// marker when the Secret itself is absent), for the DECLARED external
	// Secret prerequisites (forge.ExternalSecret). These BLOCK — the deploy
	// renders a consumer (e.g. cert-manager's DNS-01 ClusterIssuer) that reads
	// the Secret out-of-band, so its absence hangs ACME/DNS silently after a
	// green apply. Only populated when a SecretGetter is configured.
	MissingRequiredSecretKeys map[string][]string
	// ByteMatchMismatches is the sorted list of advisory notes for
	// cross-secret byte-match groups whose live Secret values are NOT
	// identical across the group (a half-rotated / drifted shared credential).
	// These BLOCK — a value_group declares that the same logical secret is
	// projected to N refs, so a divergence means one consumer has the stale
	// value. Only populated when a SecretValueGetter is configured AND every
	// member Secret exists (a missing member is reported by the existence
	// check, not here).
	ByteMatchMismatches []string
	// UndeclaredSecretMounts is the RENDER-TIME back-propagation result: every
	// Secret a workload mounts/references that NOTHING in the rendered bundle
	// provides (no rendered Secret, no KubeconfigSecret, no ExternalSecret, no
	// generated/known Secret). These BLOCK — the pod would stick on
	// MountVolume.SetUp failed / CreateContainerConfigError forever, with zero
	// error at deploy time. Unlike every other field here this is a PURE,
	// no-cluster check that ALWAYS runs (incl. dry-run / local clusters) — it's
	// the complement to the live Secret preflight. See CheckSecretSupply.
	UndeclaredSecretMounts []UndeclaredSecretMount
}

// OK reports whether nothing was found missing. Inconclusive image warnings
// are advisory and do NOT make the result fail.
func (r PreflightResult) OK() bool {
	return len(r.MissingSecretKeys) == 0 &&
		len(r.MissingConfigMapKeys) == 0 &&
		len(r.MissingImages) == 0 &&
		len(r.UnverifiableImages) == 0 &&
		len(r.ArchMismatchImages) == 0 &&
		len(r.MissingCRDs) == 0 &&
		len(r.MissingRequiredSecretKeys) == 0 &&
		len(r.ByteMatchMismatches) == 0 &&
		len(r.UndeclaredSecretMounts) == 0
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
		MissingSecretKeys:         map[string][]string{},
		MissingConfigMapKeys:      map[string][]string{},
		MissingRequiredSecretKeys: map[string][]string{},
	}

	hasContext := strings.TrimSpace(opts.Context) != ""

	// RENDER-TIME secret back-propagation gate — PURE, no cluster. This is the
	// COMPLEMENT to the live Secret check below: it flags a workload mounting a
	// Secret that NOTHING in the rendered bundle provides (no rendered Secret,
	// KubeconfigSecret, or ExternalSecret), turning a silent 15-minute
	// FailedMount rot into a fail-fast block on the no-cluster path
	// (forge up --env=dev / --dry-run / host-only).
	//
	// It runs ONLY when the LIVE Secret check is NOT active (no SecretGetter or
	// no context). When a live check IS configured, that check is authoritative
	// for the deploy-namespace Secrets: a cluster-provisioned Secret (out-of-band,
	// like an ExternalSecret) is reported PRESENT, and a genuinely-absent one is
	// already reported by the live MissingSecretKeys path — so running the
	// back-prop gate too would double-report (and false-flag a legitimately
	// cluster-provided Secret the bundle deliberately doesn't render). Gating it
	// to the no-cluster path keeps the two checks non-overlapping: live check
	// when a cluster is present, render-time back-prop when it isn't.
	if !(opts.Secrets != nil && hasContext) {
		if misses := CheckSecretSupply(opts.Manifests, opts.SecretSupply); len(misses) > 0 {
			result.UndeclaredSecretMounts = misses
		}
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

	// CRD / served-kind check — ONE discovery lookup against the target
	// cluster, then every NON-CORE kind the bundle renders is verified to be in
	// the served set. A kind whose CRD isn't installed (the GRPCRoute / Gateway
	// API channel footgun) BLOCKS here instead of failing `no matches for kind`
	// mid-rollout. Core kinds (group "") are skipped — every cluster serves
	// them, so gating them would only risk a false-positive on a discovery blind
	// spot. Runs only when a checker and a target context are configured. A
	// discovery FAILURE aborts the preflight (the served set is unknown — we
	// can't assert a kind is missing) rather than blocking on every kind.
	if opts.ServedKinds != nil && hasContext {
		gvks := CollectManifestGVKs(opts.Manifests)
		wg.Add(1)
		go func() {
			defer wg.Done()
			served, derr := opts.ServedKinds.ServedKinds(ctx, opts.Context)
			if derr != nil {
				recordErr(fmt.Errorf("preflight: discover served resource kinds on %q: %w", opts.Context, derr))
				return
			}
			missing := missingCRDs(gvks, served)
			if len(missing) == 0 {
				return
			}
			mu.Lock()
			result.MissingCRDs = missing
			mu.Unlock()
		}()
	}

	// Cluster-credentialed image checker — resolve the bundle's
	// imagePullSecrets from the TARGET cluster into a temp DOCKER_CONFIG so an
	// AUTH-DENIED local-daemon lookup can be RETRIED with the credentials the
	// cluster would itself pull with. Built ONCE (not per-image). When creds
	// can't be resolved, the bundle has no imagePullSecrets, or the checker
	// isn't credentialed, credImages/credArch stay nil and the auth-denied
	// path keeps today's block-on-can't-confirm behaviour (no regression).
	credImages, credArch, credCleanup := prepareCredentialedCheckers(ctx, opts, refs)
	defer credCleanup()

	// recheckWithClusterCreds re-runs an auth-denied existence lookup with the
	// cluster's pull creds. Returns (exists, retried): retried=false when no
	// credentialed checker is available (caller keeps the auth-denied block).
	recheckExistsWithCreds := func(ref string) (exists bool, retried bool) {
		if credImages == nil {
			return false, false
		}
		ok, err := credImages.ImageExists(ctx, ref)
		if err != nil {
			// Still denied (the cluster creds also lack access — or the secret
			// was the wrong one) or now inconclusive: not a confirmed verdict,
			// so leave it to the caller's existing block/warn handling.
			return false, false
		}
		return ok, true
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
					// this. Before blocking, RETRY with the CLUSTER's pull
					// creds: the local daemon may simply lack creds the
					// cluster has, so an auth-denied here is a FALSE negative
					// for an image the cluster can pull. A credentialed retry
					// that resolves the lookup is AUTHORITATIVE (the cluster's
					// view): a present image passes, a confirmed miss blocks.
					if errors.Is(err, ErrImageCheckAuthDenied) {
						if credExists, retried := recheckExistsWithCreds(ref); retried {
							if credExists {
								return // cluster can pull it — TRUE existence verdict
							}
							mu.Lock()
							result.MissingImages = append(result.MissingImages, ref)
							mu.Unlock()
							return
						}
						// No cluster creds to retry with (or still denied with
						// them): keep the conservative block — the gate must
						// not silently pass an image it can't confirm (the
						// operator can --skip-preflight after a 5-second check).
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
					// already reported by the existence check / daemon down /
					// auth-denied on a private registry the LOCAL daemon lacks
					// creds for). Before warning, RETRY with the CLUSTER's pull
					// creds: a private image's manifest the local daemon can't
					// read is exactly the false-negative this fix targets, and
					// a credentialed read lets the arch gate actually enforce
					// on private images instead of silently skipping them.
					if errors.Is(aerr, ErrImageCheckInconclusive) {
						if credArch != nil {
							if credArchs, cerr := credArch.ImageArchitectures(ctx, ref); cerr == nil {
								archs = credArchs
								goto haveArchs
							}
						}
						mu.Lock()
						result.ArchWarnings = append(result.ArchWarnings,
							fmt.Sprintf("couldn't read architecture of image %q: %v; skipping arch gate for it", ref, aerr))
						mu.Unlock()
						return
					}
					recordErr(fmt.Errorf("preflight: read image arch %q: %w", ref, aerr))
					return
				}
			haveArchs:
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

	// Declared external Secret prerequisites (forge.ExternalSecret) — each in
	// its OWN declared namespace (often NOT opts.Namespace). One GetSecretKeys
	// per declared Secret; a missing key/Secret BLOCKS. Verified only against a
	// live target (a SecretGetter + a context), like the secretKeyRef check.
	if opts.Secrets != nil && hasContext && len(opts.RequiredSecrets) > 0 {
		for _, rs := range opts.RequiredSecrets {
			rs := rs
			wg.Add(1)
			go func() {
				defer wg.Done()
				present, exists, err := opts.Secrets.GetSecretKeys(ctx, opts.Context, rs.Namespace, rs.Name)
				if err != nil {
					recordErr(fmt.Errorf("preflight: read required Secret %s/%s: %w", rs.Namespace, rs.Name, err))
					return
				}
				want := map[string]struct{}{}
				for _, k := range rs.Keys {
					want[k] = struct{}{}
				}
				missing := missingSecretKeys(want, present, exists)
				if len(missing) == 0 {
					return
				}
				sort.Strings(missing)
				mu.Lock()
				result.MissingRequiredSecretKeys[rs.Namespace+"/"+rs.Name] = missing
				mu.Unlock()
			}()
		}
	}

	// Cross-secret BYTE-MATCH — for each value_group, read every member's live
	// values and assert byte-identity under the shared keys. A divergence (a
	// half-rotated shared credential) BLOCKS. Runs serially (groups are tiny
	// and rare); gated on a SecretValueGetter being configured.
	if opts.SecretValues != nil && hasContext && len(opts.RequiredSecrets) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mismatches := checkByteMatchGroups(ctx, opts.SecretValues, opts.Context, opts.RequiredSecrets, recordErr)
			if len(mismatches) == 0 {
				return
			}
			mu.Lock()
			result.ByteMatchMismatches = mismatches
			mu.Unlock()
		}()
	}

	wg.Wait()
	if firstErr != nil {
		return PreflightResult{}, firstErr
	}
	for _, v := range result.MissingRequiredSecretKeys {
		sort.Strings(v)
	}
	sort.Strings(result.ByteMatchMismatches)
	sort.Strings(result.MissingImages)
	sort.Strings(result.UnverifiableImages)
	sort.Strings(result.ImageWarnings)
	sort.Strings(result.ArchMismatchImages)
	sort.Strings(result.ArchWarnings)
	sort.Strings(result.MissingCRDs)
	return result, nil
}

// checkByteMatchGroups reads the live values of every ExternalSecret that
// carries a value_group and reports one line per group whose members do NOT
// agree byte-for-byte under their shared keys. A group is checked only when
// EVERY member Secret exists (a missing member is the existence check's job,
// not a byte-match miss); a group whose members declare the same key set
// (enforced at KCL load) but differ in bytes is a half-rotated shared
// credential. Returns nil when every group is consistent (or there are no
// groups). A lookup failure is surfaced via recordErr and the group skipped.
func checkByteMatchGroups(
	ctx context.Context,
	getter SecretValueGetter,
	kctx string,
	required []RequiredSecret,
	recordErr func(error),
) []string {
	// Bucket members by value_group.
	groups := map[string][]RequiredSecret{}
	for _, rs := range required {
		if rs.ValueGroup == "" {
			continue
		}
		groups[rs.ValueGroup] = append(groups[rs.ValueGroup], rs)
	}
	var out []string
	for groupID, members := range groups {
		if len(members) < 2 {
			// A single-member group matches nothing — surfaced as an audit
			// note, not a preflight block.
			continue
		}
		// Resolve every member's values; the shared keys are the same across
		// the group (KCL guarantees the key set matches), so compare the first
		// member to each subsequent one under those keys.
		type resolved struct {
			rs     RequiredSecret
			values map[string][]byte
		}
		var got []resolved
		ok := true
		for _, rs := range members {
			values, exists, err := getter.GetSecretValues(ctx, kctx, rs.Namespace, rs.Name)
			if err != nil {
				recordErr(fmt.Errorf("preflight: read value-group Secret %s/%s: %w", rs.Namespace, rs.Name, err))
				ok = false
				break
			}
			if !exists {
				// A missing member is reported by the existence check; skip the
				// byte compare for this group (can't compare against absent).
				ok = false
				break
			}
			got = append(got, resolved{rs: rs, values: values})
		}
		if !ok || len(got) < 2 {
			continue
		}
		base := got[0]
		for _, other := range got[1:] {
			for _, key := range base.rs.Keys {
				if !bytes.Equal(base.values[key], other.values[key]) {
					out = append(out, fmt.Sprintf(
						"value-group %q: key %q differs between %s/%s and %s/%s — the same logical value is projected to both, but their live bytes don't match (a half-rotated credential)",
						groupID, key, base.rs.Namespace, base.rs.Name, other.rs.Namespace, other.rs.Name))
				}
			}
		}
	}
	return out
}

// missingCRDs returns one report line per DISTINCT non-core (group, kind) the
// bundle renders that the cluster does NOT serve. Core-group kinds (group "")
// are skipped — every cluster serves Deployment/Service/ConfigMap/etc, so
// gating them would only produce a false-positive on a discovery blind spot.
// The first manifest name seen for a missing kind is named in the line so the
// report points the author at WHAT to install the CRD for. Output order is the
// caller's responsibility (the result is sorted before formatting).
func missingCRDs(gvks []ManifestGVK, served map[string]struct{}) []string {
	// De-dupe by (group, kind): a bundle renders many GRPCRoutes but the CRD is
	// either installed or not — report it once. Keep the first name seen so the
	// message stays actionable without listing every offending document.
	type miss struct {
		apiVersion string
		kind       string
		name       string
	}
	seen := map[string]struct{}{}
	var misses []miss
	for _, g := range gvks {
		group := g.group()
		if group == "" {
			// Core kind — served by every cluster. Don't gate it.
			continue
		}
		key := servedKindKey(group, g.Kind)
		if _, dup := seen[key]; dup {
			continue
		}
		if ServesKind(served, group, g.Kind) {
			continue
		}
		seen[key] = struct{}{}
		misses = append(misses, miss{apiVersion: g.ApiVersion, kind: g.Kind, name: g.Name})
	}
	if len(misses) == 0 {
		return nil
	}
	out := make([]string, 0, len(misses))
	for _, m := range misses {
		req := m.name
		if req == "" {
			req = "(unnamed manifest)"
		}
		out = append(out, fmt.Sprintf("%s (%s) — required by %s", m.kind, m.apiVersion, req))
	}
	return out
}

// prepareCredentialedCheckers resolves the bundle's imagePullSecrets from the
// TARGET cluster into a temp DOCKER_CONFIG dir and returns credentialed
// variants of the existence + arch checkers that authenticate with them. It is
// the seam that makes the image-verification path use the CLUSTER's pull creds
// rather than the local docker daemon's: an auth-denied local lookup can then
// be retried from the cluster's perspective for a TRUE verdict.
//
// It returns (nil, nil, no-op cleanup) — disabling the credentialed retry —
// when ANY precondition is unmet, so the caller transparently keeps today's
// local-daemon behaviour and never regresses:
//   - no PullCreds resolver, or no target context, or
//   - the bundle declares no imagePullSecrets, or
//   - the configured checker doesn't implement the credentialed capability, or
//   - the creds can't be resolved / materialised.
//
// The returned cleanup removes the temp dir; callers MUST defer it.
func prepareCredentialedCheckers(ctx context.Context, opts PreflightOpts, refs ManifestRefs) (ImageChecker, ImageArchChecker, func()) {
	noop := func() {}
	if opts.PullCreds == nil || strings.TrimSpace(opts.Context) == "" || len(refs.ImagePullSecrets) == 0 {
		return nil, nil, noop
	}
	// At least one of the checkers must be credentialed for the retry to mean
	// anything.
	credImagesCap, imgOK := opts.Images.(CredentialedImageChecker)
	credArchCap, archOK := opts.ImageArch.(CredentialedImageArchChecker)
	if !imgOK && !archOK {
		return nil, nil, noop
	}

	names := make([]string, 0, len(refs.ImagePullSecrets))
	for n := range refs.ImagePullSecrets {
		names = append(names, n)
	}
	sort.Strings(names)

	dockerConfig, err := opts.PullCreds.ResolveDockerConfig(ctx, opts.Context, opts.Namespace, names)
	if err != nil || len(dockerConfig) == 0 {
		// No usable creds (none of the imagePullSecrets resolved to a
		// dockerconfigjson, or a lookup failed). Fall back — don't regress.
		return nil, nil, noop
	}

	dir, err := os.MkdirTemp("", "forge-preflight-dockercfg-")
	if err != nil {
		return nil, nil, noop
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	// docker reads $DOCKER_CONFIG/config.json. The .dockerconfigjson blob IS a
	// docker config.json ({"auths":{...}}), so it drops straight in. 0600: it
	// carries registry credentials.
	if werr := os.WriteFile(filepath.Join(dir, "config.json"), dockerConfig, 0o600); werr != nil {
		cleanup()
		return nil, nil, noop
	}

	var credImages ImageChecker
	if imgOK {
		credImages = credImagesCap.WithDockerConfigDir(dir)
	}
	var credArch ImageArchChecker
	if archOK {
		credArch = credArchCap.WithDockerConfigDir(dir)
	}
	return credImages, credArch, cleanup
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

	if len(r.MissingCRDs) > 0 {
		b.WriteString("\n  Resource kinds the cluster does NOT serve (missing CRD / Gateway API channel):\n")
		for _, c := range r.MissingCRDs {
			b.WriteString(fmt.Sprintf("    - %s\n", c))
		}
	}

	if len(r.MissingRequiredSecretKeys) > 0 {
		b.WriteString("\n  DECLARED external Secret prerequisites missing (forge.ExternalSecret — provision out-of-band):\n")
		writeMissingKeyBlock(&b, "Secret", r.MissingRequiredSecretKeys)
	}

	if len(r.ByteMatchMismatches) > 0 {
		b.WriteString("\n  Cross-secret byte-match mismatches (a value_group's members carry different bytes):\n")
		for _, m := range r.ByteMatchMismatches {
			b.WriteString(fmt.Sprintf("    - %s\n", m))
		}
	}

	if len(r.UndeclaredSecretMounts) > 0 {
		b.WriteString("\n  Secrets a workload mounts/references but NOTHING in the bundle provides (would FailedMount at schedule time):\n")
		for _, m := range r.UndeclaredSecretMounts {
			who := "a workload"
			if len(m.Workloads) > 0 {
				who = quoteJoin(m.Workloads)
			}
			b.WriteString(fmt.Sprintf("    - Secret %q mounted by %s — no rendered Secret, KubeconfigSecret, or ExternalSecret declares it\n", m.Secret, who))
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
	if len(r.MissingCRDs) > 0 {
		b.WriteString("  - install the CRD for the kind(s) above on the target cluster, or the\n")
		b.WriteString("      Gateway API channel that provides them\n")
		b.WriteString("      (e.g. kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/.../experimental-install.yaml).\n")
	}
	if len(r.MissingRequiredSecretKeys) > 0 {
		b.WriteString("  - provision the DECLARED external Secret(s) above out-of-band in their\n")
		b.WriteString("      namespace (e.g. the cert-manager `cloudflare-api-token`); forge does\n")
		b.WriteString("      not create them, and their absence hangs the consumer (ACME/DNS) silently.\n")
	}
	if len(r.ByteMatchMismatches) > 0 {
		b.WriteString("  - re-sync the value-group Secret(s) above so every ref carries identical bytes\n")
		b.WriteString("      (a divergence means a half-rotated shared credential).\n")
	}
	if len(r.UndeclaredSecretMounts) > 0 {
		b.WriteString("  - declare a forge.KubeconfigSecret (to MINT it) or forge.ExternalSecret (to\n")
		b.WriteString("      promise it out-of-band), render it via a secret_provider, or remove the\n")
		b.WriteString("      mount/reference if the workload doesn't need it.\n")
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

// KubectlSecretValueGetter is the live SecretValueGetter: it reads a
// Secret's `.data` and base64-decodes each value, for the cross-secret
// byte-match check. Same per-command --context discipline and not-found →
// exists=false handling as KubectlSecretGetter.
type KubectlSecretValueGetter struct{}

// GetSecretValues returns the decoded `.data` values of the named Secret.
func (KubectlSecretValueGetter) GetSecretValues(ctx context.Context, kctx, namespace, name string) (map[string][]byte, bool, error) {
	args := []string{"get", "secret", name, "-o", "json"}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	cmd := kubectlCmd(ctx, kctx, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
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
	values := make(map[string][]byte, len(parsed.Data))
	for k, v := range parsed.Data {
		dec, derr := base64.StdEncoding.DecodeString(v)
		if derr != nil {
			return nil, false, fmt.Errorf("decode secret key %q: %w", k, derr)
		}
		values[k] = dec
	}
	return values, true, nil
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

// KubectlServedKinds is the live ServedKindChecker: it enumerates the resource
// types the target cluster's API server serves via `kubectl --context <ctx>
// api-resources --no-headers -o wide` and keys each by servedKindKey(group,
// kind). This is the cluster's discovery surface — a kind absent from it has no
// installed CRD (the GRPCRoute / Gateway API channel footgun), so the preflight
// can block before the apply that would otherwise fail `no matches for kind`.
//
// The DECLARED context is threaded per command (the same --context discipline
// the rest of the apply path uses). A non-zero exit is a genuine discovery
// failure (kubectl not configured, RBAC denial, unreachable apiserver) returned
// as an error so the gate aborts rather than asserting a kind is missing
// against an unknown served set.
type KubectlServedKinds struct{}

// ServedKinds implements ServedKindChecker. It parses `kubectl api-resources`
// output, whose columns are NAME [SHORTNAMES] APIVERSION NAMESPACED KIND. The
// APIVERSION column is the group/version ("apps/v1", "gateway.networking.k8s.io/
// v1") or a bare version ("v1") for core; the KIND column is the last field.
// We split the group off APIVERSION and key on (group, kind).
func (KubectlServedKinds) ServedKinds(ctx context.Context, kctx string) (map[string]struct{}, error) {
	// --no-headers keeps parsing simple; -o wide guarantees the APIVERSION
	// column is present (some kubectl versions omit it without -o wide).
	cmd := kubectlCmd(ctx, kctx, "api-resources", "--no-headers", "-o", "wide")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl api-resources: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return parseServedKinds(string(out)), nil
}

// parseServedKinds turns `kubectl api-resources --no-headers -o wide` output
// into the served-kind key set. Each non-blank line is whitespace-split; the
// APIVERSION column (a group/version or bare version) and KIND column are
// located positionally. kubectl's columns are NAME [SHORTNAMES] APIVERSION
// NAMESPACED KIND VERBS [CATEGORIES] — variable because SHORTNAMES/CATEGORIES
// are optional. We anchor on the APIVERSION token (the field containing a "/"
// for grouped resources, or a bare version like "v1" for core) and read KIND as
// the token two positions after it (APIVERSION, NAMESPACED, KIND). A line we
// can't confidently parse is skipped — discovery over-reporting a served kind
// would only relax the gate, and the apply itself remains the backstop.
func parseServedKinds(out string) map[string]struct{} {
	served := map[string]struct{}{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// Find the APIVERSION column: the first field that looks like an API
		// group/version. A grouped resource has a "/" ("apps/v1"); a core
		// resource has a bare version ("v1", "v2beta1"). NAME (and SHORTNAMES)
		// never contain a "/", and NAME is a plural lowercase resource name —
		// so the first "/"-bearing token, or a bare version token, is the
		// APIVERSION. KIND is two tokens after it (APIVERSION NAMESPACED KIND).
		avIdx := -1
		for i, f := range fields {
			if i == 0 {
				continue // NAME is never the apiVersion
			}
			if strings.Contains(f, "/") || looksLikeBareVersion(f) {
				avIdx = i
				break
			}
		}
		if avIdx < 0 || avIdx+2 >= len(fields) {
			continue
		}
		apiVersion := fields[avIdx]
		kind := fields[avIdx+2]
		group, _, _ := strings.Cut(apiVersion, "/")
		if !strings.Contains(apiVersion, "/") {
			group = "" // bare version → core group
		}
		served[servedKindKey(group, kind)] = struct{}{}
	}
	return served
}

// looksLikeBareVersion reports whether s is a Kubernetes API version token with
// no group (the core-group APIVERSION column, e.g. "v1", "v2", "v1beta1",
// "v2beta3"). Used to locate the APIVERSION column for core resources, whose
// token carries no "/". Matches `v<digits>` optionally followed by an
// alpha/beta qualifier.
func looksLikeBareVersion(s string) bool {
	if len(s) < 2 || s[0] != 'v' {
		return false
	}
	rest := s[1:]
	// Must start with at least one digit.
	i := 0
	for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
		i++
	}
	if i == 0 {
		return false
	}
	// Remainder (if any) is an alpha/beta qualifier like "beta1" / "alpha2".
	suffix := rest[i:]
	return suffix == "" || strings.HasPrefix(suffix, "alpha") || strings.HasPrefix(suffix, "beta")
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
//
// DockerConfigDir, when set, points `docker` at a DOCKER_CONFIG dir holding the
// CLUSTER's pull credentials (its imagePullSecrets' .dockerconfigjson), so a
// private-registry lookup the LOCAL daemon's creds would be denied succeeds
// from the cluster's perspective. The preflight builds a credentialed copy via
// WithDockerConfigDir to RETRY an auth-denied lookup, turning a false negative
// into a TRUE existence verdict. Empty = use the ambient docker config.
type DockerImageChecker struct {
	// DockerConfigDir overrides DOCKER_CONFIG for the docker invocation. Empty
	// means inherit the process environment (ambient docker login).
	DockerConfigDir string
}

// WithDockerConfigDir returns a copy of the checker that runs docker with
// DOCKER_CONFIG=dir — the cluster's pull creds. Implements
// CredentialedImageChecker so the preflight can retry an auth-denied lookup
// with the cluster's credentials.
func (c DockerImageChecker) WithDockerConfigDir(dir string) ImageChecker {
	c.DockerConfigDir = dir
	return c
}

// ImageExists reports whether `docker manifest inspect` resolves ref. See the
// type doc for the confirmed-miss vs inconclusive distinction.
func (c DockerImageChecker) ImageExists(ctx context.Context, ref string) (bool, error) {
	args := []string{"manifest", "inspect"}
	if registryRefIsInsecure(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	withDockerConfig(cmd, c.DockerConfigDir)
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
//
// DockerConfigDir mirrors DockerImageChecker: set it (via WithDockerConfigDir)
// to read the manifest with the CLUSTER's pull creds when the local daemon
// lacks access to a private registry.
type DockerImageArchChecker struct {
	// DockerConfigDir overrides DOCKER_CONFIG for the docker invocation. Empty
	// inherits the process environment.
	DockerConfigDir string
}

// WithDockerConfigDir returns a copy that runs docker with DOCKER_CONFIG=dir.
// Implements CredentialedImageArchChecker.
func (c DockerImageArchChecker) WithDockerConfigDir(dir string) ImageArchChecker {
	c.DockerConfigDir = dir
	return c
}

// ImageArchitectures returns the distinct architectures ref advertises. See
// the type doc for the two manifest shapes handled.
func (c DockerImageArchChecker) ImageArchitectures(ctx context.Context, ref string) ([]string, error) {
	args := []string{"manifest", "inspect"}
	if registryRefIsInsecure(ref) {
		args = append(args, "--insecure")
	}
	args = append(args, ref)
	cmd := exec.CommandContext(ctx, "docker", args...)
	withDockerConfig(cmd, c.DockerConfigDir)
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

// withDockerConfig points a docker *exec.Cmd at a DOCKER_CONFIG dir when dir
// is non-empty, so the lookup authenticates with the credentials materialised
// there (the cluster's pull creds) instead of the ambient docker login. A
// no-op for an empty dir (inherit the process environment). It seeds Env from
// os.Environ() once so the docker binary still finds PATH/HOME etc.
func withDockerConfig(cmd *exec.Cmd, dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	env := os.Environ()
	// Drop any inherited DOCKER_CONFIG so ours is authoritative, then set it.
	filtered := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "DOCKER_CONFIG=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	cmd.Env = append(filtered, "DOCKER_CONFIG="+dir)
}

// KubectlPullCredsResolver is the live PullCredsResolver: it reads the
// `.dockerconfigjson` of each named imagePullSecret from the TARGET cluster
// (via `kubectl --context <ctx> get secret <name> -n <ns> -o json`) and MERGES
// their `auths` maps into a single docker config.json document. That document
// is what the image-verification path materialises into a temp DOCKER_CONFIG so
// `docker manifest inspect` authenticates with the cluster's pull creds.
//
// A secret that is absent, isn't a dockerconfigjson Secret, or has an
// unparseable blob is SKIPPED (best-effort) rather than failing the resolve —
// the goal is to recover creds when we can, never to turn a credential gap into
// a hard preflight error (that would regress envs that worked before). Only a
// genuine kubectl failure (misconfig / unreachable apiserver) surfaces as an
// error. Returns (nil, nil) when no secret yielded any auths.
type KubectlPullCredsResolver struct{}

// ResolveDockerConfig implements PullCredsResolver.
func (KubectlPullCredsResolver) ResolveDockerConfig(ctx context.Context, kctx, namespace string, secretNames []string) ([]byte, error) {
	merged := map[string]json.RawMessage{}
	for _, name := range secretNames {
		args := []string{"get", "secret", name, "-o", "json"}
		if namespace != "" {
			args = append(args, "-n", namespace)
		}
		cmd := kubectlCmd(ctx, kctx, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			// A not-found pull secret is a SKIP (the existence preflight already
			// reports a genuinely-missing imagePullSecret); any other non-zero
			// exit is a real kubectl failure worth surfacing.
			if strings.Contains(string(out), "NotFound") || strings.Contains(string(out), "not found") {
				continue
			}
			return nil, fmt.Errorf("kubectl get secret %s/%s: %v: %s", namespace, name, err, strings.TrimSpace(string(out)))
		}
		auths := dockerConfigAuthsFromSecret(out)
		for reg, entry := range auths {
			if _, exists := merged[reg]; !exists {
				merged[reg] = entry
			}
		}
	}
	if len(merged) == 0 {
		return nil, nil
	}
	doc := map[string]any{"auths": merged}
	return json.Marshal(doc)
}

// dockerConfigAuthsFromSecret extracts the `auths` map from a
// kubernetes.io/dockerconfigjson Secret's JSON (`kubectl get secret -o json`).
// The Secret stores the docker config under data[".dockerconfigjson"],
// base64-encoded (kubectl -o json reports `.data` already... no — kubectl
// reports `.data` values base64-encoded). Returns an empty map for any Secret
// that isn't a parseable dockerconfigjson (best-effort: a non-cred Secret named
// as a pull secret simply contributes nothing).
func dockerConfigAuthsFromSecret(secretJSON []byte) map[string]json.RawMessage {
	var parsed struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(secretJSON, &parsed); err != nil {
		return nil
	}
	b64, ok := parsed.Data[".dockerconfigjson"]
	if !ok || strings.TrimSpace(b64) == "" {
		return nil
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil
	}
	var cfg struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	if err := json.Unmarshal(decoded, &cfg); err != nil {
		return nil
	}
	return cfg.Auths
}
