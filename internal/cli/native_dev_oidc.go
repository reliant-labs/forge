package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
// the frontend's .env.<env> wins over what forge injects. For these five it is
// per variable, except that naming the issuer or the client id takes the whole
// set (see withNativeOIDCEnv).
//
// The issuer is the IdP's origin as published by the identity converge:
// http://localhost:<idp port> by default (reaches the iOS simulator), and
// http://<device host>:<idp port> under --device-host, where the IdP serves
// that address as a second domain of the same instance and every service's
// jwt_issuer follows (see deviceIdPHost for the design and its alternatives).
// An Android emulator reaches the localhost form with
// `adb reverse tcp:<idp port> tcp:<idp port>`. The device-host form needs this
// machine's own LAN IPv4 address; an emulator's 10.0.2.2 alias is not one, so
// it leaves the issuer on localhost (see applyDeviceHostToIdP).
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

// nativeOIDCFromStatus is the sign-in configuration of a hosted env's native
// app as its control plane reports it (GetIdentityStatus), the hosted twin of
// nativeOIDCFromOutput. Nil for an app that is not a registered native client
// and for a control plane that does not report the native fields yet (an older
// one leaves kind, redirects and scopes empty), so nothing is guessed: a
// missing field is "not available", never a default.
func nativeOIDCFromStatus(doc identityStatusDocument, app identityAppWire) *nativeOIDC {
	if (app.Kind != "oidc_native" && app.Kind != release.IdentityAppNative) || app.ClientID == "" || doc.Issuer == "" || len(app.RedirectURIs) == 0 {
		return nil
	}
	return nativeOIDCFromOutput(identity.Output{
		Issuer: doc.Issuer,
		Scopes: doc.Scopes,
		Apps:   map[string]identity.AppOutput{app.Name: {ClientID: app.ClientID, RedirectURIs: app.RedirectURIs, PostLogoutURIs: app.PostLogoutURIs}},
	}, app.Name)
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

// withNativeOIDCEnv injects the dev client's five OIDC variables as ONE unit.
// They only make sense together (a scope list and redirect URIs belong to the
// client they were registered for), so when any layer (the developer's shell,
// the KCL env_vars, the frontend's .env.<env>) names an ISSUER or a CLIENT_ID,
// the app is pointed at another identity provider and forge injects none of
// them and says so. Otherwise it fills each variable no layer has set, which
// lets a developer still override the scopes or a redirect alone.
func withNativeOIDCEnv(env []string, n *nativeOIDC) []string {
	env, _ = withNativeOIDCEnvNote(env, n)
	return env
}

// withNativeOIDCEnvNote is withNativeOIDCEnv plus the one line to print when
// forge stood down, "" otherwise.
func withNativeOIDCEnvNote(env []string, n *nativeOIDC) ([]string, string) {
	if n == nil {
		return env, ""
	}
	var own []string
	for _, name := range []string{oidcIssuerEnv, oidcClientIDEnv} {
		if envLookupLastOr(env, name) != "" {
			own = append(own, name)
		}
	}
	if len(own) > 0 {
		return env, fmt.Sprintf("%s is set outside forge, so forge injects none of the dev sign-in client's %s variables "+
			"(the scopes and redirects belong to the dev IdP's client, not to the provider you configured); set the rest yourself, or unset it to use the dev IdP",
			strings.Join(own, " and "), "EXPO_PUBLIC_FORGE_OIDC_*")
	}
	for _, kv := range n.vars() {
		if kv[1] != "" && envLookupLastOr(env, kv[0]) == "" {
			env = withForcedEnv(env, kv[0], kv[1])
		}
	}
	return env, ""
}

func oidcEnvNames() []string {
	return []string{oidcIssuerEnv, oidcClientIDEnv, oidcScopesEnv, oidcRedirectURIEnv, oidcPostLogoutURIEnv}
}

// describeNativeOIDC is the launch lines for a native frontend's sign-in: the
// EFFECTIVE values (after shell and KCL overrides), and what the issuer does
// and does not reach.
//
// dh says what became of --device-host in this run, so a localhost issuer is
// explained by the real reason (see deviceHostState).
func describeNativeOIDC(feName string, n *nativeOIDC, env []string, dh deviceHostState) []string {
	if n == nil {
		return nil
	}
	p := "[up] frontend " + feName + ": "
	issuer := envLookupLastOr(env, oidcIssuerEnv)
	if issuer != n.Issuer || envLookupLastOr(env, oidcClientIDEnv) != n.ClientID {
		// The app is on another provider; withNativeOIDCEnvNote said so, and
		// forge's dev IdP lines would describe a client it is not using.
		return nil
	}
	lines := []string{fmt.Sprintf("%ssign-in: issuer %s, client %s, redirect %s", p, issuer, envLookupLastOr(env, oidcClientIDEnv), envLookupLastOr(env, oidcRedirectURIEnv))}
	if !isLoopbackOrigin(issuer) {
		if dh.Requested != "" {
			lines = append(lines, p+"the issuer is the --"+deviceHostFlag+" address: the dev IdP serves it for this run and the API validates that same issuer")
		}
		return lines
	}
	port := ""
	if u, err := url.Parse(issuer); err == nil {
		port = u.Port()
	}
	msg := p + "the issuer is localhost, which reaches the iOS simulator only"
	if port != "" {
		msg += "; an Android emulator needs `adb reverse tcp:" + port + " tcp:" + port + "`"
	}
	switch {
	case dh.Refused:
		msg += fmt.Sprintf(". --%s %s is not this machine's LAN IPv4 address, so the issuer stayed on localhost and a physical device cannot sign in; pass e.g. --%s 192.168.1.20", deviceHostFlag, strings.Trim(dh.Requested, "[]"), deviceHostFlag)
	case dh.Accepted != "":
		msg += ". --" + deviceHostFlag + " did not move it (the issuer was set explicitly, or the IdP could not serve " + dh.Accepted + " this run), so a physical device cannot sign in"
	case dh.Requested != "":
		msg += ". --" + deviceHostFlag + " did not move it (this frontend has no native application on the env's host-run identity, or the IdP could not serve " + strings.Trim(dh.Requested, "[]") + " this run), so a physical device cannot sign in"
	default:
		msg += "; a physical device needs --" + deviceHostFlag + " <this machine's LAN IPv4 address>, which makes the dev IdP serve that address too"
	}
	return append(lines, msg)
}

func announceNativeOIDC(w io.Writer, feName string, n *nativeOIDC, env []string, dh deviceHostState) {
	for _, l := range describeNativeOIDC(feName, n, env, dh) {
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
	return fmt.Sprintf("frontend %s: NO native sign-in client was registered, so the app will run on its mock provider: its app.json %s. "+
		"Set the reverse-DNS scheme in app.json: \"scheme\": %q (your bundleIdentifier/package can stay as they are). "+
		"Or declare the callback yourself: redirect_uris = [\"%s:/oauthredirect\"] on a kind = \"native\" forge.IdentityApplication. "+
		"Or opt out of the derived client with native_sign_in = False on this frontend. Background: the migrations/v1.4.0/native-auth playbook (`forge skill load migrations/v1.4.0/native-auth`)",
		frontend, have, suggest, suggest)
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

// deviceIdPHost is the device host the dev IdP should additionally serve, or
// "" when this run does not need it.
//
// # Design: one issuer per run, and why it moves
//
// A device cannot reach the IdP at localhost, and Zitadel decides both which
// instance serves a request and the `iss` it mints from the request's Host. So
// the issuer a device signs in at is whatever origin it dialled, and the API
// must validate exactly that string. Three ways to square that, and why this
// one:
//
//   - Run ONE issuer per run and make it the device origin when a device is
//     in play (chosen): the IdP serves localhost AND the device host as two
//     domains of the same instance (hostinfra adds the second on a restart,
//     no data reset), the converge publishes the device origin as the issuer,
//     and every consumer reads it from the same outputs: the API's jwt_issuer
//     and idp_base, the broker, the native app. Web sign-in on this machine
//     is unaffected because the web app's browser only talks to the app and
//     the broker dials the IdP itself, now at the LAN origin. Without
//     --device-host nothing changes, byte for byte.
//   - Make the API accept both issuers (a multi-issuer validator): widens what
//     the API trusts, needs an owned auth.go edit in every existing project,
//     and still leaves jwt_issuer, idp_base and userinfo each naming one.
//   - Make the device host THE external domain: a first boot then registers
//     only the LAN address, breaking localhost for the simulator and every
//     in-machine caller, and the address changes whenever the network does.
//
// The cost of the chosen design: the LAN address becomes the issuer for the
// run, so a network change means re-running `forge env up --device-host`
// (which converges the new one) and a session minted under another issuer
// signs in again. Returning to a run without --device-host publishes the
// localhost issuer again; nothing else needs undoing.
//
// Only when a targeted native frontend actually has a native application
// registered on the host-run identity: a run without native sign-in does not
// need the IdP on the network, and forge does not put it there.
func deviceIdPHost(e *KCLEntities, targets []string, deviceHost string) string {
	id := hostIdentity(e)
	if id == nil || deviceHost == "" {
		return ""
	}
	for _, fe := range e.Frontends {
		if !inTargetSet(targets, fe.Name) || !isNativeFrontend(fe.Type) {
			continue
		}
		if _, ok := nativeApplicationFor(id, fe.Name); ok {
			return deviceHost
		}
	}
	return ""
}

// deviceHostIsThisMachineIPv4 reports whether deviceHost is an IPv4 literal
// assigned to one of THIS machine's interfaces. The issuer moves to the device
// host only then, and only for an IPv4 literal, because everything that dials
// the issuer from here has to be able to vouch for where it goes: the API's
// userinfo lookup and the broker's IdP client accept plain HTTP only to an
// IPv4 address that is this machine's, checked at connect time. A hostname
// (what it resolves to is not ours to vouch for, and changes with the
// network), an IPv6 literal, an emulator alias (10.0.2.2), a tunnel or another
// machine's address would be an issuer those paths refuse.
func deviceHostIsThisMachineIPv4(deviceHost string, local []net.Addr) bool {
	ip := net.ParseIP(strings.Trim(deviceHost, "[]"))
	return ip != nil && ip.To4() != nil && addrsContainIP(local, ip)
}

// applyDeviceHostToIdP records the device host on the env's zitadel infra
// entry, which is what prewarmInfra (the IdP's domains), the identity
// converge (the issuer) and the launch announcement all read. It returns the
// notice to print, "" when this run has nothing to say. The IdP's listener is
// not changed: Zitadel binds every interface already (it has no listen
// address setting), so what this run adds is the instance's domain.
func applyDeviceHostToIdP(e *KCLEntities, targets []string, deviceHost string, local []net.Addr) string {
	host := deviceIdPHost(e, targets, deviceHost)
	if host == "" {
		return ""
	}
	zitadel, err := hostZitadel(e, hostIdentity(e))
	if err != nil {
		return ""
	}
	if !deviceHostIsThisMachineIPv4(host, local) {
		return fmt.Sprintf("[up] --%s %s is not an IPv4 address of this machine, so the dev IdP keeps its localhost issuer. "+
			"The issuer moves only for this machine's own LAN IPv4 address (not a hostname such as my-mac.local, an IPv6 address, 10.0.2.2 or a tunnel): "+
			"the API and the login broker send tokens and credentials to it over plain HTTP and accept that only for an address they can confirm is this machine's at connect time. "+
			"Pass the LAN IPv4 address of this machine, e.g. --%s 192.168.1.20.\n"+
			"[up] Native sign-in then works only where localhost reaches the IdP: the iOS simulator, or an Android emulator with `adb reverse tcp:%d tcp:%d`.",
			deviceHostFlag, host, deviceHostFlag, zitadel.Port, zitadel.Port)
	}
	zitadel.DeviceHost = host
	enableLocalUserInfo(e)
	notice := fmt.Sprintf("[up] --%s %s: the dev IdP also serves http://%s:%d and signs devices in under that issuer for this run (the API's jwt_issuer follows it).\n"+
		"[up] security: the dev IdP listens on every network interface of this machine, carries the committed dev credentials from idp-steps.yaml, and is plain HTTP; use a network you trust, not shared Wi-Fi.\n"+
		"[up] stop the stack (`forge env down`) before changing networks: the address is the issuer, and a new network means a new run with the new address.",
		deviceHostFlag, host, host, zitadel.Port)
	if len(hostIdentity(e).Social) > 0 {
		notice += fmt.Sprintf("\n[up] warning: this identity declares social sign-in, and its callback moves to http://%s:%d/idps/callback for this run. "+
			"Google refuses plain-HTTP private-IP redirect URIs and a provider app registered for the localhost callback will not match, so social sign-in on this machine does not work during a --%s run (password sign-in does).",
			host, zitadel.Port, deviceHostFlag)
	}
	return notice
}

// userInfoLocalHTTPEnv is the config knob (jwt_userinfo_allow_local_http) that
// lets the API's userinfo lookup reach the dev issuer at this machine's LAN
// address over plain HTTP. It is OFF everywhere except a host workload of the
// host-run identity in a --device-host run, where forge sets it here.
const userInfoLocalHTTPEnv = "JWT_USERINFO_ALLOW_LOCAL_HTTP"

// enableLocalUserInfo sets that knob on the launch env of every host workload
// that consumes the host-run identity, and on no other.
//
// PRECEDENCE, as the code stands: WorkloadEntity.HostEnv layers LaunchEnv OVER
// the values the workload declares in KCL, and a declared value (LaunchEnv
// included) beats the developer's shell unless FORGE_ENV_OVERRIDE names the
// key (hostlaunch.LayerHostEnvConflicts).
//
// A declared literal "false" is NOT a decision, and is overridden: it is the
// config field's default, which config_gen.workloadEnvMap projects onto EVERY
// workload of a scaffolded project, so it is indistinguishable from "nothing
// was said". Skipping it (as an earlier revision did) left the knob off in
// every real project and Claims.Email empty on every device. Any other
// declaration (a literal true, a secret or config-map reference) is left
// exactly as declared. To refuse the opt-in in a --device-host run, export
// JWT_USERINFO_ALLOW_LOCAL_HTTP=false with FORGE_ENV_OVERRIDE naming it.
func enableLocalUserInfo(e *KCLEntities) {
	id := hostIdentity(e)
	if id == nil {
		return
	}
	owned := release.IdentitySecretPrefix + id.Name
	for i := range e.Workloads {
		w := &e.Workloads[i]
		if !w.OnRuntime(RuntimeHost) || w.Runtime.Host == nil {
			continue
		}
		consumes := false
		for _, r := range secretRefsForService(w) {
			if r.SecretName == owned {
				consumes = true
				break
			}
		}
		if !consumes || declaresUserInfoDecision(w) {
			continue
		}
		if w.Runtime.Host.LaunchEnv == nil {
			w.Runtime.Host.LaunchEnv = map[string]string{}
		}
		w.Runtime.Host.LaunchEnv[userInfoLocalHTTPEnv] = "true"
	}
}

// declaresUserInfoDecision reports whether the workload declares the userinfo
// knob as something other than the projected default: a literal that parses
// as true, an unparseable literal, or a reference. Forge never overwrites
// those (see enableLocalUserInfo for why a literal false does not count).
func declaresUserInfoDecision(w *WorkloadEntity) bool {
	for _, ev := range w.EnvVars() {
		if ev.Name != userInfoLocalHTTPEnv {
			continue
		}
		if ev.SecretRef != "" || ev.ConfigMapRef != "" {
			return true
		}
		if v, err := strconv.ParseBool(strings.TrimSpace(ev.Value)); err != nil || v {
			return true
		}
	}
	return false
}

// deviceHostState is what became of --device-host in this run.
type deviceHostState struct {
	// Requested is what --device-host (or FORGE_DEV_DEVICE_HOST) said.
	Requested string
	// Accepted is the device host the IdP was told to serve: Requested when it
	// is an IPv4 address of this machine and a native application needs it,
	// else "".
	Accepted string
	// Refused is true when Requested was given and is not an IPv4 address of
	// this machine, the one reason forge refuses to move the issuer.
	Refused bool
}

// deviceHostStateFor derives that from the run's own decision
// (applyDeviceHostToIdP records an accepted host on the zitadel entry; the
// identity step clears it when the IdP could not serve it).
func deviceHostStateFor(e *KCLEntities, requested string, local []net.Addr) deviceHostState {
	st := deviceHostState{Requested: requested}
	if requested == "" {
		return st
	}
	if id := hostIdentity(e); id != nil {
		if z, err := hostZitadel(e, id); err == nil {
			st.Accepted = z.DeviceHost
		}
	}
	st.Refused = st.Accepted == "" && !deviceHostIsThisMachineIPv4(requested, local)
	return st
}
