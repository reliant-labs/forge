package hostedimage

// The push base: WHERE a hosted artifact goes, composed from the env's own
// declaration and nothing else.
//
// `<registry_host>/<organization>/<project>` is the ONE subtree the platform
// admits for an org's project, and every hosted address forge computes hangs
// off it — images at `<base>/<image>`, a site release at
// `<base>/<site>/static.v1`, a config bundle at `<base>/bundle.v1/<env>`.
// That grammar is the registry's own (ADR-0003); forge composes the same
// strings so the address it pushes to and the address the platform admits
// cannot be two derivations of one fact.
//
// FORGE READS THE DECLARATION, NEVER THE SERVER. The registry host is a
// forge DEFAULT (DefaultRegistryHost) overridable in KCL, and the
// organization is DECLARED on forge.ControlPlane. Nothing here makes a call,
// which is the property the whole design turns on:
//
//   - `forge env render <env>` and `forge lint` judge a hosted image with no
//     credential and no network, so a render stays the reproducible
//     projection every other surface diffs against;
//   - the build and the deploy resolve the same bare image to the same
//     address without either of them deriving it somewhere else.
//
// WHAT THIS REPLACED, AND WHY. forge used to LEARN the base from the control
// plane — EnsureEnvironment returned it, forge wrote it to a per-env file
// under .forge/state, and the offline consumers read that file back. Three
// costs, all of them structural: the base was unknowable before the first
// successful authenticated call, so a fresh checkout could not render; a
// stale or absent record silently weakened the check rather than failing it;
// and "the server told us" plus "we wrote it down" are two sources that drift
// by construction. A default plus a declared org is the same fact with no
// call, no cache and no staleness — and a WRONG org is not forge's problem to
// detect, because the registry's realm refuses a push outside the token's own
// subtree and says so.

import "strings"

// DefaultRegistryHost is Reliant's hosted container registry — what
// `forge.ControlPlane {}` means when it names no registry_host.
//
// One fact, twice: kcl/schema.k's RELIANT_REGISTRY_HOST is the KCL copy,
// which is what a render actually reads. This is the Go copy, for the
// surfaces that have no KCL in front of them. A test renders the schema and
// fails if the two disagree (TestRegistryDefault_KCLMatchesGo).
//
// It is a HOST, with no scheme and no trailing slash, because it is joined
// into an image reference.
const DefaultRegistryHost = "registry.reliantapi.com"

// PushBase composes `<registryHost>/<organization>/<project>`.
//
// It returns "" when any segment is missing, and that is deliberately not an
// error here: this function has no env to name and no opinion about whether
// the caller can proceed. The callers decide, and they decide differently —
// KCL refuses a hosted env with no organization at load, a build refuses a
// bare image it cannot resolve, a render reports what it could not compare.
//
// registryHost defaults to DefaultRegistryHost when empty, so a declaration
// that names an org and leaves the host alone resolves to Reliant's registry
// — which is the overwhelmingly common case and the one `forge.ControlPlane
// {organization = "..."}` is meant to express.
func PushBase(registryHost, organization, project string) string {
	host := strings.TrimSpace(registryHost)
	if host == "" {
		host = DefaultRegistryHost
	}
	org := strings.TrimSpace(organization)
	proj := strings.TrimSpace(project)
	if org == "" || proj == "" {
		return ""
	}
	return NormalizeBase(host) + "/" + org + "/" + proj
}
