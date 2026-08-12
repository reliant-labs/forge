// frontend_auth_flow_test.go — the scaffolded sign-in flow must be complete
// and consistent across BOTH browser frontend kinds.
//
// The defect this guards against is the one frontend_routes_test.go documents
// from the other side: a scaffold that ships auth UI pointing at routes forge
// does not emit. The fix was to emit them — which creates the mirror-image
// risk, that one kind gains a route and the other silently does not. A
// Next.js scaffold with a working sign-in page and a Vite scaffold whose
// `/auth/callback` 404s is the same class of "looks finished" failure, just
// harder to see, because each tree is internally consistent.
//
// Every assertion here derives from the COMPOSED TEMPLATE TREE — the same
// listing the scaffolder writes from — rather than from a hardcoded file
// list, so a file that stops being emitted fails here instead of at a user's
// first login.
package templates

import (
	"path"
	"regexp"
	"strings"
	"testing"
)

// authRoutePaths are the two URLs the OIDC authorization-code flow needs. The
// callback path is also what a user registers with their IdP, so it is a
// published interface: changing it silently breaks every project whose
// provider is already configured.
var authRoutePaths = []string{"/auth/sign-in", "/auth/callback"}

// browserKinds are the frontend kinds that get the browser-redirect flow.
// React Native is deliberately absent — see TestReactNativeShipsNoBrowserOIDC.
var browserKinds = []string{"nextjs", "vite-spa"}

// authTreeFor returns the composed tree for a kind, keyed by the file's
// DESTINATION path in the generated project — i.e. FrontendTreeFile.Rel with
// any .tmpl suffix stripped, which is exactly the transformation the
// scaffolder applies when it writes. Keying on the destination means a file
// that changes between a plain and a templated form still satisfies these
// assertions, since the generated project is unchanged either way.
//
// Fails loudly on an empty tree, which would make every assertion below
// vacuously true.
func authTreeFor(t *testing.T, kind string) map[string]string {
	t.Helper()
	files, err := ListFrontendTree(kind)
	if err != nil {
		t.Fatalf("list %s tree: %v", kind, err)
	}
	if len(files) == 0 {
		t.Fatalf("%s composed an EMPTY template tree — this guard is now blind", kind)
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		out[strings.TrimSuffix(f.Rel, ".tmpl")] = f.Path
	}
	return out
}

// renderFrontend renders one template for a kind.
func renderFrontend(t *testing.T, tmplPath, kind string) string {
	t.Helper()
	content, err := FrontendTemplates().Render(tmplPath, FrontendTemplateData{
		FrontendName: "web",
		ProjectName:  "demo",
		Platform:     kind,
		Module:       "example.com/demo",
	})
	if err != nil {
		t.Fatalf("render %s for %s: %v", tmplPath, kind, err)
	}
	return string(content)
}

// TestBothBrowserKindsShipTheWholeAuthFlow pins that each browser kind
// carries every piece the flow needs: the mechanism, the storage decision,
// the provider, the screens, and a route for each of the two URLs.
func TestBothBrowserKindsShipTheWholeAuthFlow(t *testing.T) {
	t.Parallel()

	// The mechanism modules, shared by both kinds. Named by destination path
	// so a file moved between roots still satisfies this.
	sharedModules := []string{
		"src/lib/auth/oidc.ts",
		"src/lib/auth/oidc-storage.ts",
		"src/lib/auth/oidc-provider.ts",
		"src/lib/auth/auth-screens.tsx",
		// The mock must remain reachable: pure-mock frontend development is a
		// supported mode, not a legacy state.
		"src/lib/auth/session-provider.ts",
		"src/lib/auth/provider.ts",
		"src/lib/auth/context.tsx",
	}

	for _, kind := range browserKinds {
		tree := authTreeFor(t, kind)
		for _, rel := range sharedModules {
			if _, ok := tree[rel]; !ok {
				t.Errorf("%s: composed tree has no %s — the sign-in flow is incomplete in this kind", kind, rel)
			}
		}
	}
}

// TestBothBrowserKindsRouteTheAuthPaths pins that each kind actually SERVES
// both URLs, in whatever way that kind expresses a route.
//
// The two mechanisms are genuinely different, so this checks each on its own
// terms rather than grepping for a shared string:
//
//   - Next.js App Router: a `page.tsx` at the path's directory.
//   - Vite + TanStack Router: a `createRoute({... path: "<p>" ...})` entry in
//     the scaffolded routes.tsx.
func TestBothBrowserKindsRouteTheAuthPaths(t *testing.T) {
	t.Parallel()

	// ── Next.js: the route IS the file path. ──
	nextTree := authTreeFor(t, "nextjs")
	for _, route := range authRoutePaths {
		rel := path.Join("src", "app", strings.TrimPrefix(route, "/"), "page.tsx")
		if _, ok := nextTree[rel]; !ok {
			t.Errorf("nextjs serves no %s: expected the App Router page %s.\n"+
				"    A scaffolded sign-in button that navigates to a route forge does not emit ships broken.",
				route, rel)
		}
	}

	// ── Vite: the route is an entry in routes.tsx. ──
	viteTree := authTreeFor(t, "vite-spa")
	routesRel := path.Join("src", "routes.tsx")
	routesTmpl, ok := viteTree[routesRel]
	if !ok {
		t.Fatalf("vite-spa composed no %s — the router entry point is gone", routesRel)
	}
	routes := renderFrontend(t, routesTmpl, "vite-spa")
	for _, route := range authRoutePaths {
		// Derived from the router's own declaration form, so a route
		// mentioned only in a comment does not count.
		re := regexp.MustCompile(`path:\s*"` + regexp.QuoteMeta(route) + `"`)
		if !re.MatchString(stripComments(routes)) {
			t.Errorf("vite-spa serves no %s: %s declares no `path: %q` route.\n"+
				"    The Next.js scaffold serves it, so a Vite project's callback would 404 where a Next.js one works.",
				route, routesRel, route)
		}
	}

	// A route declared but never added to the tree is not served. Both auth
	// routes must reach addChildren.
	for _, name := range []string{"signInRoute", "authCallbackRoute"} {
		if !strings.Contains(routes, name) {
			t.Errorf("vite-spa routes.tsx does not reference %s — a route that is declared but not added to the route tree is not served", name)
		}
	}
	addChildren := regexp.MustCompile(`(?s)addChildren\(\[(.*?)\]`).FindStringSubmatch(routes)
	if addChildren == nil {
		t.Fatal("vite-spa routes.tsx has no addChildren([...]) call — cannot verify the auth routes are mounted")
	}
	for _, name := range []string{"signInRoute", "authCallbackRoute"} {
		if !strings.Contains(addChildren[1], name) {
			t.Errorf("vite-spa: %s is declared but never passed to addChildren — the route exists in the file and not in the app", name)
		}
	}
}

// TestScaffoldedAuthUsesOnlyS256 pins the one PKCE property that is a silent
// downgrade rather than a visible failure.
//
// Under `plain` the code challenge IS the verifier, so an attacker who can
// observe the authorization request can redeem the code — and the flow still
// works perfectly, which is why no functional test would catch it. RFC 7636
// §7.2 permits plain only for clients that cannot compute SHA-256; a browser
// with crypto.subtle has no such excuse.
func TestScaffoldedAuthUsesOnlyS256(t *testing.T) {
	t.Parallel()

	for _, kind := range browserKinds {
		tree := authTreeFor(t, kind)
		tmplPath, ok := tree["src/lib/auth/oidc.ts"]
		if !ok {
			t.Fatalf("%s: no src/lib/auth/oidc.ts in the composed tree", kind)
		}
		src := renderFrontend(t, tmplPath, kind)

		// The challenge method sent on the wire, read out of the assignment
		// itself rather than from prose about it.
		methods := regexp.MustCompile(`code_challenge_method"?,\s*"([^"]+)"`).FindAllStringSubmatch(src, -1)
		if len(methods) == 0 {
			t.Fatalf("%s: oidc.ts sets no code_challenge_method — either the parameter is missing (providers will reject the request) or this guard has gone blind:\n%s", kind, src)
		}
		for _, m := range methods {
			if m[1] != "S256" {
				t.Errorf("%s: oidc.ts sends code_challenge_method=%q. Only S256 is acceptable; RFC 7636 §7.2 permits plain only for clients that cannot hash.", kind, m[1])
			}
		}

		// The digest algorithm, likewise read from the call.
		digests := regexp.MustCompile(`subtle\.digest\(\s*"([^"]+)"`).FindAllStringSubmatch(src, -1)
		if len(digests) == 0 {
			t.Errorf("%s: oidc.ts never calls crypto.subtle.digest — the challenge cannot be a real hash of the verifier", kind)
		}
		for _, d := range digests {
			if d[1] != "SHA-256" {
				t.Errorf("%s: oidc.ts hashes the verifier with %q; RFC 7636 §4.2 specifies SHA-256", kind, d[1])
			}
		}

		// Math.random must never be CALLED: it is not a CSPRNG, and a
		// predictable verifier or state removes the point of PKCE and of the
		// CSRF check respectively.
		//
		// Matched as a call, not as a substring. The file's own error text
		// names Math.random to tell a reader why there is no fallback, and a
		// bare substring check would flag that sentence — a guard that fires
		// on its own documentation is one people delete.
		if m := regexp.MustCompile(`Math\.random\s*\(`).FindString(stripComments(src)); m != "" {
			t.Errorf("%s: oidc.ts CALLS Math.random. A code verifier or state drawn from a non-cryptographic PRNG is guessable, which defeats PKCE.", kind)
		}

		// And the CSPRNG must actually be reached.
		if !strings.Contains(src, "getRandomValues") {
			t.Errorf("%s: oidc.ts never calls crypto.getRandomValues — there is no cryptographic source for the verifier or state", kind)
		}
	}
}

// TestScaffoldedAuthKeepsTheTokenOutOfPersistentStorage pins the
// token-storage decision at the TEMPLATE level.
//
// The decision itself (in-memory only) plus its tradeoff is documented in
// oidc-storage.ts, which is the file a user reads. This test is the guard
// that keeps the code matching the documentation: the credential-bearing
// setters must not write to a persistent store, while the verifier and state
// legitimately do (they must survive the redirect).
func TestScaffoldedAuthKeepsTheTokenOutOfPersistentStorage(t *testing.T) {
	t.Parallel()

	for _, kind := range browserKinds {
		tree := authTreeFor(t, kind)
		tmplPath, ok := tree["src/lib/auth/oidc-storage.ts"]
		if !ok {
			t.Fatalf("%s: no src/lib/auth/oidc-storage.ts in the composed tree", kind)
		}
		src := stripComments(renderFrontend(t, tmplPath, kind))

		// localStorage outlives the tab and the browser session, so it is
		// never the right home for a bearer token in this design.
		if strings.Contains(src, "localStorage") {
			t.Errorf("%s: oidc-storage.ts touches localStorage. The scaffold's documented decision is in-memory only; "+
				"localStorage survives tab close and is readable by any script on the origin.", kind)
		}

		// document.cookie set from JS cannot be HttpOnly, so a cookie written
		// here would be script-readable AND automatically attached (inviting
		// CSRF) — the worst of both options.
		if regexp.MustCompile(`document\.cookie\s*=`).MatchString(src) {
			t.Errorf("%s: oidc-storage.ts writes document.cookie. A cookie set from JavaScript cannot be HttpOnly, "+
				"so it is script-readable and auto-attached; if you want a cookie session, the exchange belongs on a server.", kind)
		}

		// sessionStorage IS used, for the redirect round-trip values only.
		// Assert it is present (so the flow can actually survive a redirect)
		// and that what goes in is the verifier/state, not a token.
		if !strings.Contains(src, "sessionStorage") {
			t.Errorf("%s: oidc-storage.ts never touches sessionStorage — the PKCE verifier cannot survive the redirect to the provider, so no login can complete", kind)
		}
		for _, m := range regexp.MustCompile(`sessionStorage\.setItem\(\s*([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(src, -1) {
			switch m[1] {
			case "VERIFIER_KEY", "STATE_KEY", "RETURN_TO_KEY":
				// Single-use, single-login values. Expected.
			default:
				t.Errorf("%s: oidc-storage.ts writes %s to sessionStorage. Only the redirect round-trip values "+
					"(verifier, state, return path) may be persisted — a token there survives a reload and is XSS-readable.", kind, m[1])
			}
		}
	}
}

// TestScaffoldedAuthHardcodesNoIdentityProvider pins that the flow is
// configured, not baked in.
//
// forge ships Zitadel as an opt-in dev-compose convenience; a scaffold that
// named it in code would make every other provider a code change rather than
// a config change.
func TestScaffoldedAuthHardcodesNoIdentityProvider(t *testing.T) {
	t.Parallel()

	// Vendor names that would indicate a provider had been wired in rather
	// than configured.
	vendors := []string{
		"logto", "auth0", "okta", "keycloak", "zitadel", "clerk",
		"cognito", "firebaseapp", "onelogin", "accounts.google.com",
	}

	for _, kind := range browserKinds {
		for _, rel := range []string{
			"src/lib/auth/oidc.ts",
			"src/lib/auth/oidc-storage.ts",
			"src/lib/auth/oidc-provider.ts",
			"src/lib/auth/auth-screens.tsx",
		} {
			tree := authTreeFor(t, kind)
			tmplPath, ok := tree[rel]
			if !ok {
				t.Fatalf("%s: no %s in the composed tree", kind, rel)
			}
			// Comments dropped: naming providers in prose ("works with Auth0,
			// Keycloak, ...") is documentation, and useful. What matters is
			// that no vendor appears in CODE.
			code := strings.ToLower(stripCommentsKeepingStrings(renderFrontend(t, tmplPath, kind)))
			for _, vendor := range vendors {
				if strings.Contains(code, vendor) {
					t.Errorf("%s/%s names the identity provider %q in code. Endpoints must come from configuration "+
						"(issuer + client id) or OIDC discovery so any compliant provider works with no code change.",
						kind, rel, vendor)
				}
			}
		}
	}
}

// stripCommentsKeepingStrings removes // and /* */ comments while leaving
// string literals intact.
//
// The package's own stripComments cannot be used here: it is documented as
// naive about "//" inside a string, which is harmless for the route guard it
// was written for (an absolute URL is not a same-origin route) but fatal for
// THIS guard — it truncates `"https://tenant.auth0.com/oidc"` at the "//",
// deleting the very vendor name being looked for. Verified: with
// stripComments, a hardcoded issuer URL passed this test.
func stripCommentsKeepingStrings(src string) string {
	var out strings.Builder
	var quote byte // 0 when not inside a string
	for i := 0; i < len(src); i++ {
		c := src[i]

		if quote != 0 {
			out.WriteByte(c)
			switch {
			case c == '\\' && i+1 < len(src):
				// Escape: consume the next byte so an escaped quote does not
				// end the literal.
				i++
				out.WriteByte(src[i])
			case c == quote:
				quote = 0
			}
			continue
		}

		switch {
		case c == '"' || c == '\'' || c == '`':
			quote = c
			out.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			out.WriteByte('\n')
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				i++
			}
			i++ // land on '/', loop's i++ moves past it
		default:
			out.WriteByte(c)
		}
	}
	return out.String()
}

// TestReactNativeShipsNoBrowserOIDC pins the deliberate omission.
//
// The flow implemented here is a BROWSER redirect flow: it calls
// window.location.assign, reads window.location.search, and uses
// sessionStorage. None of those exist under React Native, where the
// equivalent needs expo-auth-session and a native redirect scheme. Shipping
// these files into an RN tree would produce code that typechecks (RN's
// tsconfig includes the DOM lib) and crashes at runtime.
func TestReactNativeShipsNoBrowserOIDC(t *testing.T) {
	t.Parallel()

	tree := authTreeFor(t, "react-native")
	for _, rel := range []string{
		"src/lib/auth/oidc.ts",
		"src/lib/auth/oidc-storage.ts",
		"src/lib/auth/oidc-provider.ts",
		"src/lib/auth/auth-screens.tsx",
	} {
		if _, ok := tree[rel]; ok {
			t.Errorf("react-native composed %s — the browser redirect flow (window.location, sessionStorage) does not exist on this platform and would crash at runtime", rel)
		}
	}

	// It must still have a working provider seam, or the RN scaffold has no
	// auth at all.
	for _, rel := range []string{
		"src/lib/auth/provider.ts",
		"src/lib/auth/session-provider.ts",
		"src/lib/auth/context.tsx",
	} {
		if _, ok := tree[rel]; !ok {
			t.Errorf("react-native composed no %s — the AuthProvider seam is missing entirely", rel)
		}
	}
}

// TestAuthContextSelectsProviderPerPlatform pins the mock-vs-real switch.
//
// The scaffolded context must reach the SELECTOR (which decides from
// configuration) on browser kinds, and must NOT reach it on React Native,
// where the module it lives in is not emitted — a stale import there is a
// build failure in a generated project.
func TestAuthContextSelectsProviderPerPlatform(t *testing.T) {
	t.Parallel()

	for _, kind := range append(browserKinds, "react-native") {
		tree := authTreeFor(t, kind)
		tmplPath, ok := tree["src/lib/auth/context.tsx"]
		if !ok {
			t.Fatalf("%s: no src/lib/auth/context.tsx in the composed tree", kind)
		}
		src := renderFrontend(t, tmplPath, kind)
		importsSelector := strings.Contains(src, `from "./oidc-provider"`)

		if kind == "react-native" {
			if importsSelector {
				t.Errorf("react-native context.tsx imports ./oidc-provider, which is not emitted for this platform — the generated project will not build")
			}
			if !strings.Contains(src, "createSessionAuthProvider") {
				t.Errorf("react-native context.tsx must fall back to the mock provider — it is the only implementation this tree ships")
			}
			continue
		}

		if !importsSelector {
			t.Errorf("%s context.tsx does not import from ./oidc-provider, so the real provider can never be selected no matter how the app is configured", kind)
		}
		if !strings.Contains(src, "selectAuthProvider") {
			t.Errorf("%s context.tsx must build its default from selectAuthProvider() so the choice is made by configuration, not by a hardcoded provider", kind)
		}
	}
}

// TestAuthScreensAreSharedNotForked pins that the two kinds render the SAME
// screens.
//
// The failure this prevents is drift: two copies of a sign-in screen, one per
// framework, where a fix to the error handling reaches one scaffold and not
// the other. Only the `"use client"` prologue may differ, which is the
// project's established mechanism for a single-line platform delta.
func TestAuthScreensAreSharedNotForked(t *testing.T) {
	t.Parallel()

	rendered := make(map[string]string, len(browserKinds))
	for _, kind := range browserKinds {
		tree := authTreeFor(t, kind)
		tmplPath, ok := tree["src/lib/auth/auth-screens.tsx"]
		if !ok {
			t.Fatalf("%s: no src/lib/auth/auth-screens.tsx in the composed tree", kind)
		}
		// Both kinds must resolve to the SAME template file, not two copies.
		if want := path.Join("shared-web", "src/lib/auth/auth-screens.tsx.tmpl"); tmplPath != want {
			t.Errorf("%s resolves auth-screens.tsx to %s, not the shared %s — a per-kind copy will drift", kind, tmplPath, want)
		}
		rendered[kind] = renderFrontend(t, tmplPath, kind)
	}

	next := strings.TrimPrefix(rendered["nextjs"], "\"use client\";\n\n")
	if next == rendered["nextjs"] {
		t.Error("the Next.js render of auth-screens.tsx carries no \"use client\" prologue; it uses hooks and state, so the App Router will reject it as a server component")
	}
	if next != rendered["vite-spa"] {
		t.Error("the two browser kinds' auth screens differ by more than the \"use client\" prologue — they must be one shared implementation, or a fix will reach only one scaffold")
	}
}
