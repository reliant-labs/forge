package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

// What a native frontend needs to know about its https universal links, and
// how forge tells it (docs/proposals/mobile-native-auth/universal-links.md).
//
// Two consumers, two channels:
//
//   - The APP (runtime) reads the sign-in redirects. The first redirect is
//     EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI as before, now https when the
//     application declares associated_domains; the private-use entry that
//     follows it is EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI. Both belong
//     to the one OIDC unit (see withNativeOIDCEnvNote), because the pair is
//     what was registered at the IdP.
//   - The Expo config plugin (plugins/with-forge-links.js, prebuild time)
//     reads FORGE_ASSOCIATED_DOMAINS and FORGE_APP_LINK_MODE. Prebuild is a
//     command the developer runs, not a child of `forge env up`, so the same
//     two values are also written to the frontend's .forge-app-links.json,
//     which the plugin falls back to. Environment variables win.
const (
	oidcFallbackRedirectURIEnv   = "EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI"
	oidcFallbackPostLogoutURIEnv = "EXPO_PUBLIC_FORGE_OIDC_FALLBACK_POST_LOGOUT_REDIRECT_URI"

	appLinkDomainsEnv = "FORGE_ASSOCIATED_DOMAINS"
	appLinkModeEnv    = "FORGE_APP_LINK_MODE"
	// appLinkModeDeveloper makes the iOS entitlements `?mode=developer`, which
	// fetches the association file directly from the host instead of Apple's
	// CDN. forge sets it only for a local env; a release build ignores it, and
	// forge must never emit it for one.
	appLinkModeDeveloper = "developer"

	appLinkConfigFile = ".forge-app-links.json"
)

// allVars is every OIDC variable of the unit: the five of the client, then the
// fallback redirects when the first redirect is an https link.
func (n nativeOIDC) allVars() [][2]string {
	return append(n.vars(),
		[2]string{oidcFallbackRedirectURIEnv, n.FallbackRedirectURI},
		[2]string{oidcFallbackPostLogoutURIEnv, n.FallbackPostLogoutRedirectURI},
	)
}

// setAppLinkFallback fills the fallbacks: when the FIRST redirect is an https
// link, the first private-use redirect after it (and the post-logout URI beside
// it). A client on a platform that cannot use the link signs in with these.
// Nothing is set when the first redirect is already private-use: there is
// nothing to fall back from.
func (n *nativeOIDC) setAppLinkFallback(redirects, postLogouts []string) {
	if len(redirects) == 0 || !strings.HasPrefix(redirects[0], "https://") {
		return
	}
	n.FallbackRedirectURI = firstPrivateUse(redirects)
	if n.FallbackRedirectURI != "" {
		n.FallbackPostLogoutRedirectURI = firstPrivateUse(postLogouts)
	}
}

func firstPrivateUse(uris []string) string {
	for _, u := range uris {
		if scheme, _, ok := strings.Cut(u, ":"); ok && release.IsNativePrivateScheme(scheme) {
			return u
		}
	}
	return ""
}

// appLinkConfig is what the Expo config plugin reads.
type appLinkConfig struct {
	Domains []string
	// Developer is set for a local env only.
	Developer bool
}

// nativeAppLinkConfig is the plugin configuration of a native frontend: the
// associated domains of its native application. Empty (no domains) for a
// frontend that declares none, which is every project today.
func nativeAppLinkConfig(e *KCLEntities, fe FrontendEntity) appLinkConfig {
	if e == nil || !isNativeFrontend(fe.Type) {
		return appLinkConfig{}
	}
	app, ok := nativeApplicationFor(e.Identity, fe.Name)
	if !ok || len(app.AssociatedDomains) == 0 {
		return appLinkConfig{}
	}
	return appLinkConfig{Domains: slices.Clone(app.AssociatedDomains), Developer: e.Lifecycle == "local"}
}

func (c appLinkConfig) vars() [][2]string {
	if len(c.Domains) == 0 {
		return nil
	}
	out := [][2]string{{appLinkDomainsEnv, strings.Join(c.Domains, ",")}}
	if c.Developer {
		out = append(out, [2]string{appLinkModeEnv, appLinkModeDeveloper})
	}
	return out
}

// withAppLinkEnv adds the plugin's variables to a frontend launch env, never
// replacing one the developer already set.
func withAppLinkEnv(env []string, c appLinkConfig) []string {
	for _, kv := range c.vars() {
		if envLookupLastOr(env, kv[0]) == "" {
			env = withForcedEnv(env, kv[0], kv[1])
		}
	}
	return env
}

// writeAppLinkConfig writes (or removes) <frontend>/.forge-app-links.json, the
// file the config plugin falls back to when prebuild runs outside forge. With
// no domains it REMOVES a stale file: an association nobody declares any more
// must not survive into the next prebuild. It touches the disk only when the
// bytes change, and never a frontend that has no domains and no file.
func writeAppLinkConfig(frontendDir string, c appLinkConfig) error {
	path := filepath.Join(frontendDir, appLinkConfigFile)
	if len(c.Domains) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		return nil
	}
	doc := map[string]string{}
	for _, kv := range c.vars() {
		doc[kv[0]] = kv[1]
	}
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if old, err := os.ReadFile(path); err == nil && string(old) == string(body) {
		return nil
	}
	if err := os.MkdirAll(frontendDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o644)
}

// frontendDirOf is a native frontend's directory on disk.
func frontendDirOf(projectDir string, fe FrontendEntity) string {
	if filepath.IsAbs(fe.Path) {
		return fe.Path
	}
	return filepath.Join(projectDir, fe.Path)
}
