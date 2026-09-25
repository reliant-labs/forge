package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Invalidate is the per-deploy CDN cache policy.
//
//   - entrypoints (DEFAULT) — invalidate only the documents that cannot be
//     content-addressed: the HTML entry points and the runtime config
//     document. Content-hashed assets never need invalidating, so this is
//     O(a few paths) per deploy regardless of site size.
//   - none — invalidate nothing. Correct for a short TTL or a promotion that
//     did not change the bytes.
//   - all — purge /*. Correct only for a build that does not hash asset
//     filenames. Invalidation is rate-limited per project, so this is a
//     deliberate choice with a real cost and never a default.
//
// +kubebuilder:validation:Enum=entrypoints;none;all
type Invalidate string

const (
	InvalidateEntrypoints Invalidate = "entrypoints"
	InvalidateNone        Invalidate = "none"
	InvalidateAll         Invalidate = "all"
)

// StaticSiteCDN is the CDN in front of the site's bucket, and what a deploy
// does to the edge cache afterwards. Neither side creates the CDN: URLMap names
// an EXISTING load-balancer URL map whose backend is the bucket.
type StaticSiteCDN struct {
	// +kubebuilder:validation:MinLength=1
	URLMap string `json:"urlMap"`

	// Invalidate is the per-deploy cache policy. Empty means entrypoints.
	// The zero value MUST NOT mean all: an unset field is exactly the case
	// where the quota-expensive choice is least likely to have been
	// considered.
	// +optional
	// +kubebuilder:default=entrypoints
	Invalidate Invalidate `json:"invalidate,omitempty"`

	// ExtraInvalidatePaths adds absolute site paths to the entrypoints set,
	// for documents the planner cannot infer (a bare /manifest.json, a
	// service worker). Ignored under none and all.
	// +optional
	ExtraInvalidatePaths []string `json:"extraInvalidatePaths,omitempty"`
}

// EffectiveInvalidate resolves an unset policy to entrypoints.
func (c StaticSiteCDN) EffectiveInvalidate() Invalidate {
	if c.Invalidate == "" {
		return InvalidateEntrypoints
	}
	return c.Invalidate
}

// StaticSiteSpec is a site served from an object-storage bucket, optionally
// behind a CDN.
//
// # WHAT A DEPLOY IS
//
// Both sides already implemented the same model, and it is kept. An
// assembled tree is archived IMMUTABLY under releases/<digest>/, and the
// live/ prefix is synced from one release. Deploy, promotion and rollback
// are therefore the same mechanical move: re-point live/ at an archive
// that already exists. None of them rebuilds, so a rollback cannot produce
// different bytes than the deploy it undoes.
//
// MERGE DECISIONS:
//
//   - The DEPLOYED spec carries control-plane's fields (the digests to serve
//     and to protect, retention, CDN). forge's BUILD inputs (public_dir,
//     bundle, cache_control) are not here, because they describe how to
//     PRODUCE a release, not what to serve. They stay on forge's KCL
//     StaticSite build block, which runs before an artifact exists and is
//     deliberately not a CR field. A CR carrying public_dir would name a
//     directory on a machine the operator never sees.
//   - The digests are SPEC, not status (control-plane's design, on merit).
//     LiveDigest is the pointer whose movement IS the deploy.
//     PreviousDigest and RetainedDigests are what retention must never
//     delete. The retention guarantee has to arrive from the release ledger
//     and not be inferred from the bucket, and a value the executor
//     computed for itself could be lost to a status wipe, after which
//     retention would no longer know what to protect.
//   - Nothing here encodes HOW bytes move. forge's `gcloud storage cp`
//     executor is to be replaced by a single storage-SDK planner. These
//     types describe only the desired pointer and the retention set, so
//     they fit either executor.
type StaticSiteSpec struct {
	// Bucket is the bucket the site's prefixes live in: gs://name or a bare
	// name. Self-hosted, the user's own bucket. Hosted, the platform assigns
	// a shared bucket and a per-customer prefix and ignores this value.
	// +optional
	// +kubebuilder:validation:MaxLength=222
	Bucket string `json:"bucket,omitempty"`

	// BasePath mounts the site under a sub-path ("/admin"). Empty is the
	// root.
	// +optional
	// +kubebuilder:validation:Pattern=`^/.*$`
	BasePath string `json:"basePath,omitempty"`

	// LiveDigest is the release the live prefix must serve. A digest, never
	// a tag: a tag is a mutable pointer, and re-pointing one would void
	// "the bytes that passed staging are the bytes in prod". Empty before
	// the first deploy.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	LiveDigest string `json:"liveDigest,omitempty"`

	// PreviousDigest is the release that was live before LiveDigest: the
	// one-step rollback target.
	// +optional
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	PreviousDigest string `json:"previousDigest,omitempty"`

	// RetainedDigests are releases that MUST survive retention (every digest
	// an environment is promoted to, plus any pin), projected from the
	// release ledger. A retention planner that cannot resolve its protected
	// set deletes NOTHING. It never reads "I know of nothing to protect" as
	// "nothing needs protecting".
	// +optional
	RetainedDigests []string `json:"retainedDigests,omitempty"`

	// KeepReleases is how many archived releases to retain, newest first.
	// Unset means DefaultKeepReleases. An explicit 0 retains everything.
	// Otherwise it must be at least MinKeepReleases, because keeping fewer
	// than the live release plus its predecessor would delete the artifact
	// a rollback needs.
	//
	// A POINTER because there are three states. control-plane's plain
	// omitempty int32 made "unset" and "retain everything" the same value,
	// so a CR that omitted the field silently disabled retention. forge's
	// KCL defaulted to 10. Both are honoured now: nil is the default, and
	// 0 is a deliberate choice.
	// +optional
	// +kubebuilder:validation:Minimum=0
	KeepReleases *int32 `json:"keepReleases,omitempty"`

	// Entrypoints are the site-absolute paths that cannot be
	// content-addressed and are invalidated on every deploy. Empty uses the
	// planner's default set.
	// +optional
	Entrypoints []string `json:"entrypoints,omitempty"`

	// CDN is the CDN in front of the bucket. Omitted means a bucket served
	// directly, with no invalidation.
	// +optional
	CDN *StaticSiteCDN `json:"cdn,omitempty"`

	// Domains are custom hostnames, in addition to the default one.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	Domains []string `json:"domains,omitempty"`
}

// MinKeepReleases is the retention floor: the live release plus its
// predecessor.
const MinKeepReleases = 2

// DefaultKeepReleases is forge's default retention depth.
const DefaultKeepReleases = 10

// StaticSiteStatus is the observed state of a StaticSite.
type StaticSiteStatus struct {
	WorkloadStatus `json:",inline"`

	// BucketPrefix is the site's root prefix within its bucket, RECORDED
	// rather than re-derived, so that a later change to the derivation rule
	// cannot silently re-point a prefix that already holds live objects.
	// +optional
	BucketPrefix string `json:"bucketPrefix,omitempty"`

	// LiveDigest is what the live prefix was CONFIRMED to serve. It lags
	// spec.liveDigest until a sync succeeds, and that gap is the drift
	// signal. PreviousDigest is its confirmed predecessor.
	// +optional
	LiveDigest string `json:"liveDigest,omitempty"`
	// +optional
	PreviousDigest string `json:"previousDigest,omitempty"`

	// ReleaseCount is how many archived releases the last retention pass
	// observed.
	// +optional
	// +kubebuilder:validation:Minimum=0
	ReleaseCount int32 `json:"releaseCount,omitempty"`

	// LastSyncedAt is nil until the live prefix has synced once.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`
}

// StaticSite is a static site served from object storage.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ss,categories=forge
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Hostname",type=string,JSONPath=`.status.hostname`
// +kubebuilder:printcolumn:name="Live",type=string,JSONPath=`.status.liveDigest`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type StaticSite struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StaticSiteSpec   `json:"spec"`
	Status StaticSiteStatus `json:"status,omitempty"`
}

// StaticSiteList contains a list of StaticSite.
//
// +kubebuilder:object:root=true
type StaticSiteList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []StaticSite `json:"items"`
}

// EffectiveKeepReleases applies the default and the rollback floor. The floor
// is applied here, on the type, so that no retention planner is one forgotten
// max() away from deleting a rollback target. 0 means "retain everything" and
// is not floored. Validate refuses 1 outright; the floor here is the
// defense-in-depth for a spec that skipped it.
func (s StaticSiteSpec) EffectiveKeepReleases() int32 {
	switch {
	case s.KeepReleases == nil:
		return DefaultKeepReleases
	case *s.KeepReleases == 0:
		return 0
	case *s.KeepReleases < MinKeepReleases:
		return MinKeepReleases
	default:
		return *s.KeepReleases
	}
}
