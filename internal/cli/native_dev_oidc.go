package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/pkg/identity"
	"github.com/reliant-labs/forge/pkg/release"
)

// A native (Expo) frontend signs in as an OAuth public client straight at the
// env's identity provider (authorization code + PKCE; no broker, no secret).
// `forge env up` converges that client (the kind-native identity application,
// derived for every `rn` frontend of a host-run identity) and hands the app
// what it needs through the env vars below. The names are the contract with
// the app's auth module; they are read as static `process.env.X`, so Metro
// inlines them.
//
// Precedence, the same as EXPO_PUBLIC_API_URL and the dev-log origin (see
// native_dev_origin.go): a value the developer exported, the KCL env_vars, or
// the frontend's .env.<env> wins over what forge injects, per variable.
//
// The issuer is always the IdP's own origin (http://localhost:<port>), and
// --device-host does NOT change it. Zitadel resolves which instance a request
// is for from the Host header, matched against the instance's ExternalDomain,
// which the dev IdP pins to "localhost" (internal/hostinfra/zitadel.go), and it
// mints tokens whose `iss` is that origin; the services validate that exact
// `iss`. An issuer on a LAN address or 10.0.2.2 would be a different instance
// lookup and a different `iss`, so forge does not advertise one. An Android
// emulator reaches the IdP with `adb reverse tcp:<idp port> tcp:<idp port>`;
// a physical device is a follow-up of this slice.
const (
	oidcIssuerEnv        = "EXPO_PUBLIC_FORGE_OIDC_ISSUER"
	oidcClientIDEnv      = "EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID"
	oidcScopesEnv        = "EXPO_PUBLIC_FORGE_OIDC_SCOPES"
	oidcRedirectURIEnv   = "EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI"
	oidcPostLogoutURIEnv = "EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI"
	offlineAccessScope   = "offline_access"
)

// nativeOIDC is the sign-in configuration of one native frontend.
type nativeOIDC struct {
	Issuer, ClientID, Scopes, RedirectURI, PostLogoutRedirectURI string
}

func (n nativeOIDC) vars() [][2]string {
	return [][2]string{
		{oidcIssuerEnv, n.Issuer},
		{oidcClientIDEnv, n.ClientID},
		{oidcScopesEnv, n.Scopes},
		{oidcRedirectURIEnv, n.RedirectURI},
		{oidcPostLogoutURIEnv, n.PostLogoutRedirectURI},
	}
}

// nativeApplicationFor is the kind-native application of a frontend.
func nativeApplicationFor(id *release.ShapeIdentity, frontend string) (release.ShapeIdentityApplication, bool) {
	if id == nil {
		return release.ShapeIdentityApplication{}, false
	}
	for _, a := range id.Applications {
		if a.Kind == release.IdentityAppNative && a.Frontend == frontend {
			return a, true
		}
	}
	return release.ShapeIdentityApplication{}, false
}

// readNativeOIDC resolves what to inject into fe from the identity's published
// outputs (.forge/identity/<name>.json, written by convergeHostIdentity). It
// returns nil when the env has no converged managed identity with a native
// application for fe: nothing is injected and the app keeps its unwired
// provider.
func readNativeOIDC(projectDir string, e *KCLEntities, fe FrontendEntity) *nativeOIDC {
	id := hostIdentity(e)
	if id == nil || !isNativeFrontend(fe.Type) {
		return nil
	}
	app, ok := nativeApplicationFor(id, fe.Name)
	if !ok {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(projectDir, ".forge", "identity", id.Name+".json"))
	if err != nil {
		return nil
	}
	var out identity.Output
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return nativeOIDCFromOutput(out, app.Name)
}

// nativeOIDCFromOutput is the pure half: the published output and the name of
// the native application. Nil when the app is not (yet) registered.
//
// Revocation and end-session endpoints are deliberately absent: the app reads
// them from the issuer's OIDC discovery document, which is their source of
// truth, so they are not duplicated into Output or into env vars.
func nativeOIDCFromOutput(out identity.Output, appName string) *nativeOIDC {
	app, ok := out.Apps[appName]
	if !ok || app.ClientID == "" || out.Issuer == "" || len(app.RedirectURIs) == 0 {
		return nil
	}
	// openid profile email offline_access first (offline_access is what makes
	// the IdP issue a refresh token), then whatever else the instance says a
	// token needs: the project audience scope, without which every RPC 401s,
	// and the roles scope.
	scopes := []string{"openid", "profile", "email", offlineAccessScope}
	for _, s := range out.Scopes {
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	n := &nativeOIDC{
		Issuer:      out.Issuer,
		ClientID:    app.ClientID,
		Scopes:      strings.Join(scopes, " "),
		RedirectURI: app.RedirectURIs[0],
	}
	if len(app.PostLogoutURIs) > 0 {
		n.PostLogoutRedirectURI = app.PostLogoutURIs[0]
	}
	return n
}

// withNativeOIDCEnv fills each OIDC variable the developer, the KCL or the
// frontend's .env file has not already set.
func withNativeOIDCEnv(env []string, n *nativeOIDC) []string {
	if n == nil {
		return env
	}
	for _, kv := range n.vars() {
		if kv[1] != "" && envLookupLastOr(env, kv[0]) == "" {
			env = withForcedEnv(env, kv[0], kv[1])
		}
	}
	return env
}

// describeNativeOIDC is the launch lines for a native frontend's sign-in: the
// EFFECTIVE values (after shell and KCL overrides), and why --device-host does
// not move the issuer.
func describeNativeOIDC(feName string, n *nativeOIDC, env []string, deviceHost string) []string {
	if n == nil {
		return nil
	}
	p := "[up] frontend " + feName + ": "
	issuer := envLookupLastOr(env, oidcIssuerEnv)
	lines := []string{fmt.Sprintf("%ssign-in: issuer %s, client %s, redirect %s", p, issuer, envLookupLastOr(env, oidcClientIDEnv), envLookupLastOr(env, oidcRedirectURIEnv))}
	if isLoopbackOrigin(issuer) {
		port := ""
		if u, err := url.Parse(issuer); err == nil {
			port = u.Port()
		}
		msg := p + "the issuer is localhost, which reaches the iOS simulator only"
		if port != "" {
			msg += "; an Android emulator needs `adb reverse tcp:" + port + " tcp:" + port + "`"
		}
		if deviceHost != "" {
			msg += ". --" + deviceHostFlag + " does not apply to the IdP: Zitadel resolves its instance from the Host header (localhost) and mints tokens under that issuer, so forge does not advertise another one"
		}
		lines = append(lines, msg)
	}
	return lines
}

func announceNativeOIDC(w io.Writer, feName string, n *nativeOIDC, env []string, deviceHost string) {
	for _, l := range describeNativeOIDC(feName, n, env, deviceHost) {
		fmt.Fprintln(w, l)
	}
}

// expoApp is the part of a frontend's app.json that names its callback.
type expoApp struct {
	Expo struct {
		Scheme json.RawMessage `json:"scheme"`
		IOS    struct {
			BundleIdentifier string `json:"bundleIdentifier"`
		} `json:"ios"`
		Android struct {
			Package string `json:"package"`
		} `json:"android"`
	} `json:"expo"`
}

func (a expoApp) schemes() []string {
	var one string
	if json.Unmarshal(a.Expo.Scheme, &one) == nil && one != "" {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(a.Expo.Scheme, &many)
	return many
}

// nativeAppSchemes reads the callback scheme of each native frontend that has
// a native application and no declared callback, from the frontend's own
// app.json. The callback is `<scheme>:/oauthredirect` (RFC 8252 section 7.1).
//
// The scheme is the entry of `expo.scheme` that is a valid private-use scheme
// (lowercase reverse-DNS, at least one dot: release.IsNativePrivateScheme), NOT
// the bundle id: a bundle id may carry uppercase letters and `_` (Android and
// iOS allow them, `forge project new` allows them in a project name) which no
// RFC 8252 scheme can, and `expo.scheme` is what registers the link with the
// OS, so the scheme the IdP trusts and the scheme the app answers are one fact.
// A frontend whose app.json has none is reported with the exact edit that
// fixes it, rather than left to fall back to the mock provider unexplained.
func nativeAppSchemes(e *KCLEntities, id *release.ShapeIdentity, projectDir string) (map[string]string, []string) {
	schemes := map[string]string{}
	var warnings []string
	for _, fe := range e.Frontends {
		app, ok := nativeApplicationFor(id, fe.Name)
		if !ok || len(app.RedirectURIs) > 0 {
			continue
		}
		dir := fe.Path
		if dir != "" && !filepath.IsAbs(dir) {
			dir = filepath.Join(projectDir, dir)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "app.json"))
		if err != nil {
			if dynamic := dynamicAppConfig(dir); dynamic != "" {
				warnings = append(warnings, fmt.Sprintf("frontend %s: NO native sign-in client was registered: it configures Expo with %s, and forge reads only a static app.json, so its callback scheme cannot be derived. Declare the callback instead: redirect_uris = [\"com.acme.%s:/oauthredirect\"] on its kind = \"native\" forge.IdentityApplication (and list the same scheme in expo.scheme)", fe.Name, dynamic, sanitizeSchemeLabel(fe.Name)))
				continue
			}
			warnings = append(warnings, fmt.Sprintf("frontend %s: cannot read its app.json (%v), so its native callback cannot be derived", fe.Name, err))
			continue
		}
		var a expoApp
		if err := json.Unmarshal(raw, &a); err != nil {
			warnings = append(warnings, fmt.Sprintf("frontend %s: app.json is not valid JSON (%v)", fe.Name, err))
			continue
		}
		if scheme := a.callbackScheme(); scheme != "" {
			schemes[fe.Name] = scheme
			continue
		}
		warnings = append(warnings, a.noSchemeWarning(fe.Name))
	}
	return schemes, warnings
}

// callbackScheme is the first expo.scheme entry usable as a native callback.
func (a expoApp) callbackScheme() string {
	for _, s := range a.schemes() {
		if release.IsNativePrivateScheme(s) {
			return s
		}
	}
	return ""
}

// noSchemeWarning says why the app has no usable callback scheme and the exact
// app.json edit that gives it one.
func (a expoApp) noSchemeWarning(frontend string) string {
	suggest := suggestedScheme(append(append([]string{a.Expo.IOS.BundleIdentifier, a.Expo.Android.Package}, a.schemes()...), frontend)...)
	have := "has no expo.scheme"
	if got := a.schemes(); len(got) > 0 {
		have = fmt.Sprintf("has expo.scheme %q, and none of it is a valid private-use scheme (RFC 8252 section 7.1: lowercase letters, digits and '-', joined by at least one '.', e.g. com.acme.app)", got)
	}
	return fmt.Sprintf("frontend %s: NO native sign-in client was registered, so the app will run on its mock provider: its app.json %s. Add the reverse-DNS scheme to app.json: \"scheme\": [%q, %q] (the bundle id may stay as it is). Or declare the callback yourself: redirect_uris = [\"%s:/oauthredirect\"] on a kind = \"native\" forge.IdentityApplication", frontend, have, frontend, suggest, suggest)
}

// suggestedScheme turns the first available bundle id into a valid scheme:
// lowercased, any character a scheme cannot hold replaced by '-', and a dot
// guaranteed. With no bundle id it is com.example.<frontend>.
func suggestedScheme(bundleIDs ...string) string {
	for _, b := range bundleIDs {
		if b == "" {
			continue
		}
		if s := sanitizeScheme(b); release.IsNativePrivateScheme(s) {
			return s
		}
	}
	return "com.example." + sanitizeSchemeLabel(bundleIDs[len(bundleIDs)-1])
}

func sanitizeScheme(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	for strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", ".")
	}
	return out
}

// sanitizeSchemeLabel is one DNS-label-shaped segment: lowercase letters,
// digits and '-', starting with a letter.
func sanitizeSchemeLabel(s string) string {
	l := strings.ReplaceAll(sanitizeScheme(s), ".", "-")
	if l == "" || l[0] < 'a' || l[0] > 'z' {
		l = "app" + l
	}
	return l
}

// dynamicAppConfig names an app.config.{js,ts,...} file in dir, the dynamic
// form of Expo's configuration, which forge does not evaluate.
func dynamicAppConfig(dir string) string {
	for _, n := range []string{"app.config.ts", "app.config.js", "app.config.mjs", "app.config.cjs"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return n
		}
	}
	return ""
}
