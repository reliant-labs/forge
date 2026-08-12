// Package devidp provisions a project's dev identity provider (Zitadel):
// registering the project and its single-page application, and publishing
// the values Zitadel GENERATES (client_id, project id) so the rest of the
// stack can reference them by name instead of by value.
//
// It is a public package — unlike its internal/devidp sibling in the forge
// repo itself — because the code that calls it runs INSIDE a scaffolded
// project's own binary (the `auth idp-provision` subcommand, run as a
// deploy-time Job), which cannot import forge's internal packages across
// the module boundary.
package devidp

import "strings"

// RedirectGlob builds the dev-IdP redirect-URI PATTERN a scaffolded SPA
// registers, from the frontend's origin glob and its URL base path.
//
// THE BUG THIS FIXES. The pattern used to be hardcoded as
// "http://localhost:*/auth/callback" — origin glob plus the bare callback
// path, with no base path segment. That matches a frontend served at the
// host root, but not one mounted under a prefix (forge.yaml
// frontends[].base_path, e.g. "/internal"): the browser's real callback is
// "http://localhost:3002/internal/auth/callback", and doublestar's `*`
// does not cross a `/` — so the registered pattern rejected every callback
// from a base-pathed frontend, failing at the LAST redirect of an
// otherwise perfect sign-in.
//
// basePath is the frontend's forge.yaml base_path verbatim — "" for a
// frontend served at the host root, "/internal" for one mounted under a
// prefix. It is inserted between the origin and the callback path exactly
// where the browser puts it (window.location.origin + BASE_PATH +
// CALLBACK_PATH — see basepath_gen.ts / oidc-provider.ts), so the glob and
// the literal URL Zitadel actually receives agree on shape.
//
// The `*` still spans ONLY the port: it is embedded in originGlob (the
// caller's "http://localhost:*"), never introduced here, so a base path
// containing a literal "*" is impossible — forge's own base_path
// validation (config.ValidateBasePath) restricts segments to
// [A-Za-z0-9._-], and a hand-edited config.k is the author's own
// responsibility, same as any other KCL value.
func RedirectGlob(originGlob, basePath, callbackPath string) string {
	basePath = strings.TrimSuffix(basePath, "/")
	return originGlob + basePath + callbackPath
}

// LogoutGlob builds the post-logout redirect PATTERN. It is the bare
// origin, not origin+basePath: oidc-provider.ts sends
// `window.location.origin` alone as post_logout_redirect_uri (unlike the
// callback URI, it does not append BASE_PATH), so the registered pattern
// must match that literally, not what RedirectGlob composes for the
// callback route.
func LogoutGlob(originGlob string) string {
	return originGlob
}
