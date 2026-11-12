package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/applinks"
	"github.com/reliant-labs/forge/pkg/release"
)

// https universal links / App Links for a native frontend
// (docs/proposals/mobile-native-auth/universal-links.md).
//
// A kind-native application that declares associated_domains is bound to
// those hosts by two association files. Their CONTENT is facts about the
// binary (bundle id, package, signing team and certificates), so they are
// rendered per env at deploy and served from the project's static site on that
// host, as a /.well-known/ overlay: a promoted site release stays
// byte-identical, and each env serves its own fingerprints.
//
// This file owns the file side of it, kept apart from the native-app redirect
// derivation (native_hosted_identity.go, devidentity) on purpose:
//   - the https redirect itself is prepended by release.PrependAppLinkRedirect,
//     the ONE helper, called where a private-use list is derived (devidentity
//     for the host IdP; the hosted derivation calls it the same way).
//   - nativeAppLinkApps: which apps an env's association files describe.
//   - attachAppLinkFiles / hostedWellKnown: where the rendered files go.

// nativeAppIDs are the identifiers an app.json gives the binary.
type nativeAppIDs struct {
	BundleID string
	Package  string
}

// readNativeAppIDs reads ios.bundleIdentifier and android.package from the
// frontend's static app.json, the same file the callback scheme comes from.
func readNativeAppIDs(frontendDir string) (nativeAppIDs, error) {
	raw, err := os.ReadFile(filepath.Join(frontendDir, "app.json"))
	if err != nil {
		return nativeAppIDs{}, err
	}
	var a expoApp
	if err := json.Unmarshal(raw, &a); err != nil {
		return nativeAppIDs{}, fmt.Errorf("app.json is not valid JSON: %w", err)
	}
	return nativeAppIDs{BundleID: a.Expo.IOS.BundleIdentifier, Package: a.Expo.Android.Package}, nil
}

// appLinkSource is everything the association files of one env are rendered
// from, with the machine-dependent part (the debug keystore) injected so the
// rest is pure.
type appLinkSource struct {
	Env         string
	ProjectDir  string
	Entities    *KCLEntities
	DebugPrint  func(frontendDir string) (string, error)
	ReadAppIDs  func(frontendDir string) (nativeAppIDs, error)
	Diagnostics io.Writer
}

// associatedApps are the env's native applications that declare associated
// domains, with the frontend each belongs to.
func (s appLinkSource) associatedApps() []struct {
	App      release.ShapeIdentityApplication
	Frontend FrontendEntity
} {
	var out []struct {
		App      release.ShapeIdentityApplication
		Frontend FrontendEntity
	}
	if s.Entities == nil || s.Entities.Identity == nil {
		return nil
	}
	for _, a := range s.Entities.Identity.Applications {
		if a.Kind != release.IdentityAppNative || len(a.AssociatedDomains) == 0 {
			continue
		}
		for _, f := range s.Entities.Frontends {
			if f.Name == a.Frontend {
				out = append(out, struct {
					App      release.ShapeIdentityApplication
					Frontend FrontendEntity
				}{a, f})
			}
		}
	}
	return out
}

// appLinkFiles renders the env's association files, grouped by the associated
// domain set they serve (one site per set). It returns an error for a
// declaration that cannot produce a valid file, never a half-empty one:
//
//   - an iOS app needs apple_team_id and a bundle id, an Android app needs a
//     package and at least one fingerprint for this env; an app with NEITHER
//     is refused (the domains would claim an association nothing binds);
//   - the "debug" fingerprint is resolved from the keystore the build uses.
func (s appLinkSource) appLinkFiles(ctx context.Context) (map[string]applinks.Files, error) {
	byDomains := map[string][]applinks.App{}
	for _, pair := range s.associatedApps() {
		a, f := pair.App, pair.Frontend
		if err := release.ValidateAssociatedDomains(a.AssociatedDomains, a.RedirectURIs); err != nil {
			return nil, fmt.Errorf("native application %s: %w", a.Name, err)
		}
		dir := f.Path
		if dir != "" && !filepath.IsAbs(dir) {
			dir = filepath.Join(s.ProjectDir, dir)
		}
		ids, err := s.ReadAppIDs(dir)
		if err != nil {
			return nil, fmt.Errorf("frontend %s: cannot read its app.json to name the app in its association files: %w", f.Name, err)
		}
		app := applinks.App{Frontend: f.Name}
		if f.AppleTeamID != "" && ids.BundleID != "" {
			app.TeamID, app.BundleID = f.AppleTeamID, ids.BundleID
		}
		if fps := f.AndroidCertFingerprints[s.Env]; len(fps) > 0 && ids.Package != "" {
			app.Package = ids.Package
			for _, fp := range fps {
				if applinks.IsDebugFingerprint(fp) {
					got, err := s.DebugPrint(dir)
					if err != nil {
						return nil, fmt.Errorf("frontend %s: android_cert_fingerprints[%q] is \"debug\": %w. "+
							"Run `expo prebuild` once so android/app/debug.keystore exists, or list the SHA-256 fingerprint yourself", f.Name, s.Env, err)
					}
					fp = got
				}
				app.Fingerprints = append(app.Fingerprints, fp)
			}
		}
		if !app.IOS() && !app.Android() {
			return nil, fmt.Errorf("frontend %s: native application %s declares associated_domains %v, but nothing binds the app to them in env %q. "+
				"Set apple_team_id (iOS, with ios.bundleIdentifier in app.json) and/or android_cert_fingerprints[%q] (Android, with android.package)",
				f.Name, a.Name, a.AssociatedDomains, s.Env, s.Env)
		}
		key := appLinkDomainSetKey(a.AssociatedDomains)
		byDomains[key] = append(byDomains[key], app)
	}
	out := map[string]applinks.Files{}
	for key, apps := range byDomains {
		applinks.SortApps(apps)
		files, err := applinks.Render(apps)
		if err != nil {
			return nil, err
		}
		out[key] = files
	}
	return out, nil
}

// associatedSiteFor picks the static-site frontend that serves the app's
// domains' /.well-known/: the app's declared associated_site, else the env's
// only non-native frontend that publishes a static site. An ambiguous or
// absent site is an error naming the fix, because a guess would publish
// another app's host a file it never asked for.
func associatedSiteFor(e *KCLEntities, app FrontendEntity) (FrontendEntity, error) {
	if app.AssociatedSite != "" {
		for _, f := range e.Frontends {
			if f.Name == app.AssociatedSite {
				if isNativeFrontend(f.Type) {
					return FrontendEntity{}, fmt.Errorf("frontend %s: associated_site %q is a native frontend; name the static site that serves the domains", app.Name, f.Name)
				}
				return f, nil
			}
		}
		return FrontendEntity{}, fmt.Errorf("frontend %s: associated_site %q is not a frontend of this env", app.Name, app.AssociatedSite)
	}
	var sites []FrontendEntity
	for _, f := range e.Frontends {
		if isNativeFrontend(f.Type) {
			continue
		}
		switch f.Runtime.Type {
		case FrontendRuntimeHosted, FrontendRuntimeBucket, FrontendRuntimeFirebase:
			sites = append(sites, f)
		}
	}
	switch len(sites) {
	case 1:
		return sites[0], nil
	case 0:
		return FrontendEntity{}, fmt.Errorf("frontend %s: no static-site frontend in this env serves its associated domains. "+
			"Add a site on the domain (forge.OnHosted / OnBucket / OnFirebase) or serve the files yourself from `forge env app-links`", app.Name)
	default:
		names := make([]string, len(sites))
		for i, f := range sites {
			names[i] = f.Name
		}
		sort.Strings(names)
		return FrontendEntity{}, fmt.Errorf("frontend %s: %d static sites could serve its associated domains (%s); set associated_site on the native frontend", app.Name, len(sites), strings.Join(names, ", "))
	}
}

// attachAppLinkFiles renders the env's association files and attaches them to
// the static-site frontend that serves their host, as that frontend's
// WellKnown overlay. It is a no-op for an env with no associated domains, so
// every other deploy is byte-identical.
//
// Two native apps on one host are merged into one file (the AASA lists both
// app ids); two sites cannot serve one domain set, so a second site is refused.
func attachAppLinkFiles(ctx context.Context, s appLinkSource) error {
	if len(s.associatedApps()) == 0 {
		return nil
	}
	files, err := s.appLinkFiles(ctx)
	if err != nil {
		return err
	}
	// A site serves ONE overlay on every hostname bound to it. Therefore
	// every app on that site must claim the same domain set; combining subsets
	// would silently associate an app with hosts it never declared. Check
	// host ownership separately: overlapping sets on different sites are also
	// impossible. Domain order remains untouched on the application (it picks
	// callback precedence); only the file-group key is canonicalized.
	siteOf := map[string]string{}
	domainsOfSite := map[string]string{}
	siteOfHost := map[string]string{}
	for _, pair := range s.associatedApps() {
		site, err := associatedSiteFor(s.Entities, pair.Frontend)
		if err != nil {
			return err
		}
		key := appLinkDomainSetKey(pair.App.AssociatedDomains)
		for _, host := range pair.App.AssociatedDomains {
			if prev, ok := siteOfHost[host]; ok && prev != site.Name {
				return fmt.Errorf("associated host %s is served by two sites (%s and %s); name one with associated_site", host, prev, site.Name)
			}
			siteOfHost[host] = site.Name
		}
		if prev, ok := domainsOfSite[site.Name]; ok && prev != key {
			return fmt.Errorf("site %s has incompatible associated domain sets (%s and %s): one site serves the same association files on every host; use separate sites or declare the same domain set", site.Name, prev, key)
		}
		domainsOfSite[site.Name] = key
		siteOf[key] = site.Name
	}
	for _, key := range slices.Sorted(maps.Keys(siteOf)) {
		site := siteOf[key]
		for i := range s.Entities.Frontends {
			if s.Entities.Frontends[i].Name != site {
				continue
			}
			fe := &s.Entities.Frontends[i]
			if fe.WellKnown == nil {
				fe.WellKnown = map[string]string{}
			}
			for rel, body := range files[key] {
				fe.WellKnown[strings.TrimPrefix(rel, applinksWellKnownPrefix)] = string(body)
			}
		}
	}
	return nil
}

// Canonicalize only file grouping, never the ordered callback declaration.
func appLinkDomainSetKey(domains []string) string {
	set := slices.Clone(domains)
	slices.Sort(set)
	return strings.Join(slices.Compact(set), ",")
}

const applinksWellKnownPrefix = ".well-known/"

// defaultAppLinkSource wires the real keystore and app.json readers.
func defaultAppLinkSource(env, projectDir string, e *KCLEntities, w io.Writer) appLinkSource {
	return appLinkSource{
		Env: env, ProjectDir: projectDir, Entities: e, Diagnostics: w,
		ReadAppIDs: readNativeAppIDs,
		DebugPrint: func(frontendDir string) (string, error) {
			return applinks.DebugKeystoreFingerprint(context.Background(), applinks.ExecRunner,
				filepath.Join(frontendDir, "android", "app", "debug.keystore"))
		},
	}
}

// checkAppLinks is the render-time half: the declaration problems that need
// no keystore, no filesystem and no site, refused where every path that could ship the
// env passes through, so the author still has the KCL in front of them. It
// does NOT read the debug keystore (that is the deploy's, where the files are
// written), so `forge env shape` and a render stay machine-independent.
func checkAppLinks(env string, e *KCLEntities) error {
	src := appLinkSource{
		Env: env, Entities: e,
		ReadAppIDs: func(string) (nativeAppIDs, error) {
			// The names do not affect whether a declaration binds.
			return nativeAppIDs{BundleID: "x.y", Package: "x.y"}, nil
		},
		DebugPrint: func(string) (string, error) {
			return strings.Repeat("00:", 31) + "00", nil
		},
	}
	if len(src.associatedApps()) == 0 {
		return nil
	}
	// The serving site is NOT checked here: a dev env serves nothing itself
	// (the loop's host is a hosted dev site declared with the deploy), so
	// requiring one would refuse `forge env up`. attachAppLinkFiles requires
	// it at deploy, where the files are written.
	_, err := src.appLinkFiles(context.Background())
	return err
}
