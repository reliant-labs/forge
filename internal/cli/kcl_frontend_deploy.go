package cli

// Frontend deploy entities — the Go side of the kcl/schema.k
// `Frontend.deploy` union (FirebaseHosting | StaticSite | K8sCluster).
//
// Split out of kcl_render.go because they are a coherent cluster with
// one job — decoding the frontend deploy discriminator and its variant
// bodies — and because kcl_render.go had grown past the public-struct
// ceiling revive enforces. Every type here is decoded from the rendered
// KCL JSON and consumed by internal/deploytarget.

import (
	"encoding/json"
	"fmt"
)

// FrontendDeployEntity carries the deploy discriminator for a frontend.
// Three variants are populated today: FirebaseHosting (Type=="firebase"),
// StaticSite (Type=="static-site") and K8sCluster (Type=="cluster"); the
// matching pointer field is non-nil exactly when its Type matches. The
// Type discriminator drives the build skip-list; the embedded variant
// blocks carry the per-target config the deploy dispatch needs. Adding
// new dispatch keys (e.g. a Vercel variant) later is a pure additive
// change — a new pointer field + a new Type string.
//
// The variants take DIFFERENT paths after the build:
//
//   - "firebase" and "static-site" ship out-of-band via
//     dispatchFrontendDeploys (build the static export, assemble it, then
//     `firebase deploy` / upload to a bucket). Neither ever appears in
//     the k8s manifest stream. They share the whole build-and-assemble
//     half (internal/deploytarget/staticstage.go) and differ only in
//     where the finished tree goes.
//   - "cluster" is projected in KCL (render.k `_project_frontend`) onto the
//     same RenderedWorkload a forge.Service produces, so it renders a real
//     Deployment + Service and rides the normal apply / rollout / prune
//     path. Nothing in the Go deploy dispatch special-cases it — by the
//     time the manifests exist it is indistinguishable from any other
//     cluster workload, which is the point.
type FrontendDeployEntity struct {
	Type string `json:"type"` // "firebase" | "static-site" | "cluster" (host/external/compose reserved for future frontend targets)

	// Firebase is populated when Type=="firebase". The Firebase Hosting
	// deploy spec — build output dir, target site/project, base-path
	// mount, and any extra static dirs to assemble into the same site.
	Firebase *FirebaseHostingDeploy `json:"-"`

	// StaticSite is populated when Type=="static-site". The bucket +
	// cache policy + CDN + retention spec.
	StaticSite *StaticSiteDeploy `json:"-"`
}

// StaticSiteDeploy mirrors the kcl/schema.k StaticSite schema. The
// forge-side StaticSiteProvider builds and assembles the frontend
// exactly as the Firebase path does, then uploads the tree to an
// immutable `releases/<digest>/` prefix and syncs `live/` from it.
type StaticSiteDeploy struct {
	Bucket       string         `json:"bucket"`
	PublicDir    string         `json:"public_dir"`
	BasePath     string         `json:"base_path,omitempty"`
	Bundle       []BundleDir    `json:"bundle,omitempty"`
	CacheControl []CacheRule    `json:"cache_control,omitempty"`
	CDN          *StaticSiteCDN `json:"cdn,omitempty"`
	// KeepReleases has NO omitempty, deliberately. 0 is a meaningful
	// value ("retain everything, never prune"), and the KCL renderer
	// projects the key unconditionally so it survives the round trip. An
	// omitempty here would drop it and the provider would read the
	// absent key back as the default 10, pruning archived releases the
	// author explicitly asked forge to keep — in a bucket, where the
	// loss is not recoverable.
	KeepReleases int `json:"keep_releases"`
}

// CacheRule is one Cache-Control header applied to the objects a glob
// matches. Order is significant: first match wins.
type CacheRule struct {
	Pattern      string `json:"pattern"`
	CacheControl string `json:"cache_control"`
}

// StaticSiteCDN is the CDN in front of a static site's bucket and the
// per-deploy invalidation policy. Absent (nil) means the bucket is
// served directly and a deploy invalidates nothing.
type StaticSiteCDN struct {
	URLMap               string   `json:"url_map"`
	Invalidate           string   `json:"invalidate,omitempty"`
	ExtraInvalidatePaths []string `json:"extra_invalidate_paths,omitempty"`
}

// FirebaseHostingDeploy mirrors the kcl/schema.k FirebaseHosting schema.
// The forge-side FirebaseProvider builds the frontend, assembles
// public_dir + Bundle dirs into a staging tree honoring BasePath, writes
// a firebase.json + .firebaserc, and runs `firebase deploy`.
type FirebaseHostingDeploy struct {
	Project   string           `json:"project"`
	Site      string           `json:"site"`
	Target    string           `json:"target,omitempty"`
	PublicDir string           `json:"public_dir"`
	BasePath  string           `json:"base_path,omitempty"`
	Bundle    []BundleDir      `json:"bundle,omitempty"`
	Rewrites  []map[string]any `json:"rewrites,omitempty"`
}

// BundleDir is one extra pre-built static directory assembled into a
// deployed site alongside the frontend's own build output. Dest empty
// means the site root. Target-neutral — the FirebaseHosting and
// StaticSite variants assemble bundles identically, which is why the
// KCL schema of the same name is shared between them.
type BundleDir struct {
	Src  string `json:"src"`
	Dest string `json:"dest,omitempty"`
}

// UnmarshalJSON dispatches the frontend deploy block by its `type`
// discriminator. An absent / null deploy leaves the zero value (Type=="").
// "firebase" and "static-site" carry typed bodies; unknown types are
// retained as the bare Type string so a forward-compatible KCL render (a
// deploy variant this binary predates) degrades to "skip build / no
// dispatch" rather than erroring the whole render.
func (d *FrontendDeployEntity) UnmarshalJSON(data []byte) error {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	d.Type = probe.Type
	switch probe.Type {
	case "firebase":
		var fb FirebaseHostingDeploy
		if err := json.Unmarshal(data, &fb); err != nil {
			return fmt.Errorf("parse firebase frontend deploy: %w", err)
		}
		d.Firebase = &fb
	case frontendDeployStaticSite:
		var ss StaticSiteDeploy
		if err := json.Unmarshal(data, &ss); err != nil {
			return fmt.Errorf("parse static-site frontend deploy: %w", err)
		}
		d.StaticSite = &ss
	}
	return nil
}

// frontendDeployStaticSite is the StaticSite discriminator value. Named
// rather than spelled inline because it appears at every dispatch site
// that decides whether a frontend ships (deploy dispatch, the
// frontend-only scope gate, the build image filter, the `up` phase
// requirements) — and a typo at any ONE of them is silent: the frontend
// simply does not deploy, with no error to notice.
const frontendDeployStaticSite = "static-site"
