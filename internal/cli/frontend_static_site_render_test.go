package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// staticSiteRenderProject writes a throwaway project whose staging env
// declares one Frontend with the supplied `deploy = forge.StaticSite
// {...}` body, renders it against the in-tree forge KCL module, and
// returns the parsed FrontendEntity.
//
// The whole chain is what matters here — the StaticSite schema,
// _render_static_site emitting it, and FrontendDeployEntity.UnmarshalJSON
// dispatching on the discriminator. A break at any link produces a nil
// StaticSite block, which downstream reads as "this frontend does not
// ship" and skips SILENTLY. Needs CGO for the KCL plugin.
func staticSiteRenderProject(t *testing.T, deployBody string) FrontendEntity {
	t.Helper()

	forgeKcl, err := filepath.Abs("../../kcl")
	if err != nil {
		t.Fatal(err)
	}
	// FATAL, not skip: kcl/schema.k is a repo-relative path present on
	// every checkout, so its absence means the layout moved — a broken
	// test, not an inapplicable one. Skipping would silently delete this
	// test's coverage on the day that happens, which is precisely when
	// the round trip most needs checking.
	if _, err := os.Stat(filepath.Join(forgeKcl, "schema.k")); err != nil {
		t.Fatalf("forge kcl module not found at %s: %v", forgeKcl, err)
	}

	root := t.TempDir()
	kclParent := filepath.Join(root, "deploy", "kcl")
	stagingDir := filepath.Join(kclParent, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mod := "[package]\nname = \"t\"\nedition = \"v0.11.4\"\n\n[dependencies]\nforge = { path = " +
		strconv.Quote(forgeKcl) + " }\n"
	if err := os.WriteFile(filepath.Join(kclParent, "kcl.mod"), []byte(mod), 0o644); err != nil {
		t.Fatal(err)
	}

	main := `import forge
_bundle = forge.Bundle {
    frontends = [forge.Frontend {
        name = "admin-web"
        type = "nextjs"
        path = "admin-web"
        env_vars = [forge.EnvVar { name = "NEXT_PUBLIC_API_URL", value = "https://api.staging.example.com" }]
        deploy = ` + deployBody + `
    }]
}
output = forge.render(_bundle)
`
	if err := os.WriteFile(filepath.Join(stagingDir, "main.k"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := renderKCLRaw(context.Background(), root, "staging")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	ents, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out)
	}
	if len(ents.Frontends) != 1 {
		t.Fatalf("want 1 frontend, got %d: %s", len(ents.Frontends), out)
	}
	return ents.Frontends[0]
}

// TestFrontendStaticSiteDeployRoundTrip renders a full StaticSite deploy
// block and asserts every field survives KCL → JSON → FrontendEntity →
// the deploytarget spec the provider consumes.
func TestFrontendStaticSiteDeployRoundTrip(t *testing.T) {
	fe := staticSiteRenderProject(t, `forge.StaticSite {
            bucket = "gs://reliant-staging-web"
            public_dir = "out"
            base_path = "/admin"
            bundle = [forge.BundleDir { src = "../reliant-web/dist", dest = "" }]
            cache_control = [
                forge.CacheRule { pattern = "_next/static/**", cache_control = "public, max-age=31536000, immutable" }
                forge.CacheRule { pattern = "**", cache_control = "public, max-age=0, must-revalidate" }
            ]
            cdn = forge.StaticSiteCDN {
                url_map = "reliant-staging-lb"
                extra_invalidate_paths = ["/manifest.json"]
            }
            keep_releases = 5
        }`)

	if fe.Deploy == nil {
		t.Fatal("frontend deploy not parsed")
	}
	if fe.Deploy.Type != "static-site" {
		t.Fatalf("deploy type = %q, want static-site", fe.Deploy.Type)
	}
	ss := fe.Deploy.StaticSite
	if ss == nil {
		t.Fatal("static-site deploy block nil")
	}
	if ss.Bucket != "gs://reliant-staging-web" {
		t.Errorf("bucket = %q", ss.Bucket)
	}
	if ss.PublicDir != "out" {
		t.Errorf("public_dir = %q", ss.PublicDir)
	}
	if ss.BasePath != "/admin" {
		t.Errorf("base_path = %q", ss.BasePath)
	}
	if len(ss.Bundle) != 1 || ss.Bundle[0].Src != "../reliant-web/dist" {
		t.Errorf("bundle = %+v", ss.Bundle)
	}
	// Cache rules must keep DECLARATION ORDER — first match wins at
	// upload time, so a reordered list would tag immutable hashed assets
	// with the catch-all's must-revalidate.
	if len(ss.CacheControl) != 2 {
		t.Fatalf("cache_control = %+v", ss.CacheControl)
	}
	if ss.CacheControl[0].Pattern != "_next/static/**" {
		t.Errorf("cache rule order lost: %+v", ss.CacheControl)
	}
	if ss.CDN == nil || ss.CDN.URLMap != "reliant-staging-lb" {
		t.Fatalf("cdn = %+v", ss.CDN)
	}
	if ss.CDN.Invalidate != "entrypoints" {
		t.Errorf("invalidate should default to entrypoints, got %q", ss.CDN.Invalidate)
	}
	if len(ss.CDN.ExtraInvalidatePaths) != 1 || ss.CDN.ExtraInvalidatePaths[0] != "/manifest.json" {
		t.Errorf("extra_invalidate_paths = %+v", ss.CDN.ExtraInvalidatePaths)
	}
	if ss.KeepReleases != 5 {
		t.Errorf("keep_releases = %d", ss.KeepReleases)
	}

	// The CLI→provider mapping forwards every field intact.
	got := frontendToStaticSite(fe)
	if got.Spec.Bucket != ss.Bucket || got.Spec.BasePath != "/admin" {
		t.Errorf("frontendToStaticSite spec = %+v", got.Spec)
	}
	if got.BuildEnv["NEXT_PUBLIC_API_URL"] != "https://api.staging.example.com" {
		t.Errorf("frontendToStaticSite build env = %+v", got.BuildEnv)
	}
	if len(got.Spec.Bundle) != 1 || got.Spec.Bundle[0].Src != "../reliant-web/dist" {
		t.Errorf("frontendToStaticSite bundle = %+v", got.Spec.Bundle)
	}
	if len(got.Spec.CacheControl) != 2 || got.Spec.CacheControl[0].Pattern != "_next/static/**" {
		t.Errorf("frontendToStaticSite cache rules = %+v", got.Spec.CacheControl)
	}
	if got.Spec.CDN == nil || got.Spec.CDN.URLMap != "reliant-staging-lb" {
		t.Errorf("frontendToStaticSite cdn = %+v", got.Spec.CDN)
	}
	if got.Spec.KeepReleases != 5 {
		t.Errorf("frontendToStaticSite keep_releases = %d", got.Spec.KeepReleases)
	}
}

// TestFrontendStaticSiteKeepReleasesZeroSurvives is the round-trip half
// of the retention guard.
//
// keep_releases = 0 means "retain everything, never prune". Zero is also
// Go's zero value and JSON's omitempty trigger, so it is exactly the
// value most likely to be silently dropped somewhere along KCL → JSON →
// struct — and if it is, the provider reads the absent key back as the
// default 10 and starts deleting archived releases the author explicitly
// asked forge to keep. In a bucket, that loss is not recoverable.
//
// The KCL renderer projects the key unconditionally and the Go field
// carries no omitempty; this pins both.
func TestFrontendStaticSiteKeepReleasesZeroSurvives(t *testing.T) {
	fe := staticSiteRenderProject(t, `forge.StaticSite {
            bucket = "reliant-preview-web"
            public_dir = "dist"
            keep_releases = 0
        }`)

	ss := fe.Deploy.StaticSite
	if ss == nil {
		t.Fatal("static-site deploy block nil")
	}
	if ss.KeepReleases != 0 {
		t.Errorf("keep_releases = %d, want 0 (retain everything)", ss.KeepReleases)
	}
	if got := frontendToStaticSite(fe); got.Spec.KeepReleases != 0 {
		t.Errorf("keep_releases lost in the provider mapping: %d", got.Spec.KeepReleases)
	}

	// A bare bucket name (no gs:// scheme) round-trips verbatim; the
	// provider normalizes it.
	if ss.Bucket != "reliant-preview-web" {
		t.Errorf("bucket = %q", ss.Bucket)
	}
	// No CDN declared → nil → the deploy invalidates nothing.
	if ss.CDN != nil {
		t.Errorf("cdn should be nil when undeclared, got %+v", ss.CDN)
	}
}

// TestStaticSiteCountsAsShippableFrontend guards the registration trap
// that has no error path.
//
// hasShippableFrontend gates --frontends-only and the frontend-only
// cluster-skip. A StaticSite frontend missing from it does not fail —
// `--frontends-only` refuses with "declares no shippable frontend", and
// a frontend-only env drives an empty cluster.Apply instead of shipping.
// Both look like unrelated bugs.
func TestStaticSiteCountsAsShippableFrontend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		deploy *FrontendDeployEntity
		want   bool
	}{
		{"static-site ships", &FrontendDeployEntity{Type: "static-site"}, true},
		{"firebase ships", &FrontendDeployEntity{Type: "firebase"}, true},
		// A cluster frontend renders a real Deployment and rides the k8s
		// apply path, so an env containing one is NOT frontend-only.
		{"cluster does not", &FrontendDeployEntity{Type: "cluster"}, false},
		{"no deploy block does not", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &KCLEntities{Frontends: []FrontendEntity{{Name: "web", Deploy: tc.deploy}}}
			if got := hasShippableFrontend(e); got != tc.want {
				t.Errorf("hasShippableFrontend = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestStaticSiteRollbackGroupsBuilt confirms the rollback path picks up
// static-site frontends and leaves Firebase ones alone.
//
// Firebase must be excluded: FirebaseProvider.Rollback returns
// ErrProviderNotImplemented, so including a Firebase frontend would turn
// a mixed env's rollback into a hard failure over a target that has a
// perfectly good native recovery path (`firebase hosting:rollback`).
func TestStaticSiteRollbackGroupsBuilt(t *testing.T) {
	e := &KCLEntities{Frontends: []FrontendEntity{
		{Name: "site", Deploy: &FrontendDeployEntity{
			Type:       "static-site",
			StaticSite: &StaticSiteDeploy{Bucket: "gs://b", PublicDir: "dist"},
		}},
		{Name: "fb", Deploy: &FrontendDeployEntity{
			Type:     "firebase",
			Firebase: &FirebaseHostingDeploy{Project: "p", Site: "s", PublicDir: "out"},
		}},
		{Name: "devonly"},
	}}

	groups := staticSiteRollbackGroups(e, "prod", false)
	if len(groups) != 1 {
		t.Fatalf("want 1 rollback group, got %d", len(groups))
	}
	if groups[0].ProviderID != "static-site" {
		t.Errorf("provider = %q", groups[0].ProviderID)
	}
	if len(groups[0].StaticSites) != 1 || groups[0].StaticSites[0].Name != "site" {
		t.Errorf("only the static-site frontend belongs here, got %+v", groups[0].StaticSites)
	}

	// No static-site frontends => no group at all, so a Firebase-only or
	// backend-only env's rollback is completely unchanged.
	none := &KCLEntities{Frontends: []FrontendEntity{{Name: "fb", Deploy: &FrontendDeployEntity{Type: "firebase"}}}}
	if got := staticSiteRollbackGroups(none, "prod", false); len(got) != 0 {
		t.Errorf("no static sites should mean no rollback group, got %+v", got)
	}
}
