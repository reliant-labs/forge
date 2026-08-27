package removalguard

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// THE TABLE
//
// One entry per feature that has been REMOVED from forge. Recording a new
// removal is exactly one new `removal` entry — name it, list the patterns that
// constitute a reference to it, and (only if a legitimate look-alike exists)
// add a narrow allowance with the reason spelled out.
//
// Rules for writing an entry:
//
//   - Patterns should be IDENTIFIER-shaped, not English-prose-shaped. Prose is
//     unbounded ("a test asserting the dev-auth bypass is gone" legitimately
//     names it); identifiers are the thing that actually compiles, ships and
//     misleads.
//   - Allowances are suppressed SPANS, not suppressed files: a finding is
//     excused only when the exact text that matched sits inside a match of the
//     allowance's Token. A file-wide allowance (nil Token) needs a very good
//     reason, and Paths to keep it contained.
//   - A too-permissive allowance silently defeats the guard. If an allowance
//     needs more than a sentence to justify, the pattern is probably wrong.
//
// ─────────────────────────────────────────────────────────────────────────────

var removals = []removal{
	{
		Name: "tenancy",
		Why: "Multi-tenancy was removed from forge. Tenant scoping is application-owned " +
			"domain policy now; forge ships no tenant column, header, context key or helper.",
		Patterns: []*regexp.Regexp{
			// tenant, tenants, tenancy, TenantID, tenant_id, X-Tenant-Id,
			// multi-tenant, --tenant. The leading \b keeps "lieutenant" and
			// "maintenance" out.
			regexp.MustCompile(`(?i)\btenan[tc]`),
			// camelCase-embedded forms the \b above cannot see:
			// WithTestTenant, testTenant, scopeTenant. Case-SENSITIVE, so the
			// lowercase-t English words can never reach it.
			regexp.MustCompile(`[a-z0-9]Tenant`),
			// multitenant / multitenancy written without a separator.
			regexp.MustCompile(`(?i)multitenan`),
		},
		Allowances: []allowance{
			{
				Name: "the OAuth issuer's own multi-tenant URL shape",
				Reason: "An identity provider's issuer URL is frequently per-tenant " +
					"(`https://your-tenant.auth0.com`), and a provider MAY publish a query " +
					"parameter of its own on the authorization endpoint that the flow has to " +
					"preserve. Both the Go client (pkg/oauth2) and the scaffolded browser flow " +
					"name that shape in example URLs and in the test that proves the endpoint's " +
					"query survives.\n" +
					"This is the IdP's tenancy, not forge's: it is a substring of a URL the " +
					"application configures, and forge ships no tenant column, header, context " +
					"key or helper on the strength of it. Scoped to the word inside a hostname " +
					"or query parameter, so a `TenantID` field or an `X-Tenant-Id` header in " +
					"these same files still fails.",
				// Three shapes, all of them "tenant as a URL component":
				//   - inside a hostname:            your-tenant.auth0.com
				//   - as a query parameter, set:    ?tenant=acme
				//   - as a query parameter, read:   Get("tenant"), get("tenant")
				// plus the hyphenated adjective in prose about such issuers.
				Token: regexp.MustCompile(`(?i)[-.a-z0-9]*tenant[-.a-z0-9]*\.[a-z]|tenant=|"tenant"|tenant = %q|multi-tenant`),
				Paths: []string{
					"pkg/oauth2/",
					"internal/templates/frontend/shared-web/src/lib/auth/",
					"internal/templates/frontend_auth_flow_test.go",
					// Zitadel's OWN multi-tenancy: the dev IdP resolves which
					// instance a request belongs to from the Host header. That
					// is a property of the product forge runs as a container,
					// not a tenant concept forge ships. Scoped by the same
					// "multi-tenant" token, so a TenantID field here still fails.
					"pkg/devidp/zitadel.go",
				},
			},
			{
				Name: "prose teaching that forge has no tenancy",
				Reason: "The db/write-policy skill's \"If rows belong to someone, that is a column\" section and " +
					"seedplan's diamond-disambiguation comment both name tenancy in order to say " +
					"forge does NOT have it: the skill tells the author that ownership must be a " +
					"column they write because forge stores none, and the seedplan comment records " +
					"that the rule is deliberately STRUCTURAL and must not start recognizing " +
					"`tenant_id`/`org_id`, because that would put back the domain concept the " +
					"package removed on purpose.\n" +
					"This is documentation OF the removal, and it is the text most likely to stop " +
					"someone reintroducing the feature — deleting it to satisfy the guard would " +
					"delete the explanation for why the guard exists. Scoped to the negating " +
					"phrasings, so a line in these same files that actually reintroduced a tenant " +
					"column or helper still fails.",
				Token: regexp.MustCompile(`no implicit tenant|nothing about tenancy, ownership or scope|` +
					"`company_id`/`tenant_id`/`org_id`"),
				Paths: []string{
					"internal/templates/project/skills/forge/db/SKILL.md",
					"internal/templates/project/skills/forge/db/write-policy/SKILL.md",
					// The rendered copies of the same templates, tracked in-repo.
					".claude/skills/db/SKILL.md",
					".claude/skills/db-write-policy/SKILL.md",
					"pkg/seedplan/diamond.go",
				},
			},
		},
	},
	{
		Name: "authorization",
		Why: "Application-level authorization (RBAC, role checks, the pkg/crud Authorize " +
			"hook, the frontend route-guard role machinery) was removed. forge still does " +
			"AUTHENTICATION — proving who the caller is — and stops there; deciding what a " +
			"caller may do is the application's job.",
		Patterns: []*regexp.Regexp{
			regexp.MustCompile(`(?i)\bauthz\b`),
			// authorizer, Authorizer, authorizer_gen.go, NewGeneratedAuthorizer.
			// No trailing \b: '_' is a word character, so `authorizer\b` would
			// miss "authorizer_gen.go" — the exact straggler this guard was
			// written for.
			regexp.MustCompile(`(?i)authorizer`),
			// IsAuthorized, isAuthorized, useIsAuthorized. Contiguous, so the
			// prose "not authorized" cannot reach it.
			regexp.MustCompile(`(?i)isauthoriz`),
			// AuthorizeOptions, AuthorizeFunc, AuthorizeHook, AuthorizeRequest.
			regexp.MustCompile(`Authorize[A-Z]`),
			// RequireRole, RequiresRole, required_roles, requiredRoles.
			regexp.MustCompile(`(?i)\brequire[sd]?[_-]?roles?\b`),
			// HasRole, hasRole, has_role, hasAnyRole, has_any_role.
			regexp.MustCompile(`(?i)\bhas[_-]?(any[_-]?)?roles?\b`),
			// RoleCheck, role_guard, roleGate, role_claim.
			regexp.MustCompile(`(?i)\brole[_-]?(check|claim|gate|guard)s?\b`),
			// The JSX prop <RouteGuard roles={["admin"]}>. Lowercase and
			// unspaced, so Go's `claims.Roles = []string{...}` (an identity
			// claim from the IdP, which is authentication) cannot reach it.
			regexp.MustCompile(`\broles=\{`),
			// The word itself. forge no longer has application RBAC of any
			// kind, so any surviving "RBAC" outside the Kubernetes deploy
			// surface is a straggler — see the k8s allowances below.
			regexp.MustCompile(`(?i)\brbac\b`),
		},
		Allowances: []allowance{
			{
				Name:   "HTTP Authorization header",
				Reason: "`Authorization: Bearer …` is AUTHENTICATION, which forge still supports and generates. Never confuse the header with the removed authorization feature.",
				Token:  regexp.MustCompile(`(?i)\bauthorizations?\b`),
			},
			{
				Name: "the OAuth 2.0 authorization endpoint and request",
				Reason: "OAuth 2.0's own vocabulary collides head-on with the removed feature's. " +
					"`AuthorizeRequest`, `buildAuthorizeUrl` and `AuthorizeUrl` name the " +
					"AUTHORIZATION ENDPOINT of RFC 6749 §3.1 — the URL a user agent is " +
					"redirected to in order to AUTHENTICATE and consent. That is token " +
					"acquisition, which is the authentication half forge kept; it has nothing to " +
					"do with the removed `Authorize` policy hook, role checks or RBAC.\n" +
					"The `Authorize[A-Z]` pattern above was written for AuthorizeHook / " +
					"AuthorizeFunc / AuthorizeOptions — a POLICY callback deciding what a caller " +
					"may do. Scoped by path to the two OAuth clients (the Go pkg/oauth2 and the " +
					"scaffolded browser flow) and by token to the endpoint spellings, so an " +
					"`AuthorizeHook` or a role check appearing in these same files still fails.",
				Token: regexp.MustCompile(`Authorize(?:Request|Url|URL|Endpoint|Params?)\b`),
				Paths: []string{
					"pkg/oauth2/",
					"internal/templates/frontend/shared-web/src/lib/auth/",
				},
			},
			{
				Name:   "AuthN/AuthZ as security-review vocabulary",
				Reason: "The generic code-review skill teaches reviewers to look for MISSING authorization in the user's application — which owns that policy now — under the standard AuthN/AuthZ shorthand. The removed forge feature was always spelled `authz` (package and identifier), never `AuthZ`.",
				Token:  regexp.MustCompile(`AuthZ`),
				Paths:  []string{"**/code-review/security-review/SKILL.md", "**/code-review-security-review/SKILL.md"},
			},
			{
				Name:   "HTTP 401 Unauthorized",
				Reason: "`unauthorized` / `Unauthorized` / `http.StatusUnauthorized` / the `network:unauthorized` event are the HTTP 401 status — a wire-level authentication outcome, unrelated to the removed feature.",
				Token:  regexp.MustCompile(`(?i)unauthoriz(ed|ation)?`),
			},
			{
				Name:   "Kubernetes RBAC identifiers",
				Reason: "Kubernetes RBAC grants the POD's ServiceAccount access to the API server. It is pod identity, not application authorization, and deleting it breaks `forge env deploy` (operators lose CRD/Lease access). These spellings are unambiguously Kubernetes anywhere they appear.",
				Token: regexp.MustCompile(`(?i)` + strings.Join([]string{
					`rbac\.authorization\.k8s\.io`, // ClusterRole/RoleBinding apiVersion + apiGroup
					`\+kubebuilder:rbac`,           // controller-gen RBAC markers
					`[a-z0-9]*_rbac\b`,             // cluster_rbac, namespaced_rbac, operator_cluster_rbac
					`rbac_[a-z0-9_]+`,              // rbac_lib (the KCL RBAC module)
					`(cluster|namespaced)rbac`,     // ClusterRBAC, forge.ClusterRBAC
					`rbacspec`,                     // RBACSpec
					`rbac\.k\b`,                    // kcl/lib/rbac.k
				}, `|`)),
			},
			{
				Name:    "Kubernetes RBAC named in prose next to a Kubernetes noun",
				Reason:  "Docs, skills and comments say bare \"RBAC\" where no identifier spelling applies (\"Deployment / Job / RBAC\", \"RBAC denied, malformed kubeconfig\"). Requiring an unambiguous Kubernetes noun on the SAME line keeps the carve-out tied to pod identity: an application-RBAC claim (\"Connect RPC, JWT auth, RBAC, PostgreSQL\") has no such neighbour and still fails.",
				Token:   regexp.MustCompile(`(?i)\brbac\b`),
				Context: kubernetesNoun,
			},
			{
				Name: "Kubernetes RBAC prose on the deploy surface",
				Reason: "The KCL module, the cluster code, serverkit's manager wiring, the deploy command and the deploy design docs discuss Kubernetes RBAC across paragraphs, so the Kubernetes noun is often on a neighbouring line — and the manifest-render tests bind it to a local variable. These files render or apply Kubernetes manifests and nothing else. Application RBAC never lived in any of them; a re-introduction lands in handlers, middleware, frontend or skills, all of which stay guarded.\n" +
					"`env_render*.go` and `clusterhealth*.go` join this list for the same reason, not a weaker one. `forge env render` prints the manifests an environment would apply — one of which IS a ClusterRoleBinding — and the Cluster Workloads check reads pod status from the API server, where `RBAC denying the list` is one of the ways it must answer UNDETERMINED rather than pass. Both talk to Kubernetes and nothing else.",
				Token: regexp.MustCompile(`(?i)\brbac\b`),
				Paths: []string{
					"kcl/",
					"internal/cluster/",
					"internal/kclrender/",
					"internal/kclvendor/",
					"internal/kclplugin/",
					"pkg/serverkit/",
					"docs/design/",
					"internal/cli/deploy*.go",
					"internal/cli/env_render*.go",
					"internal/doctor/clusterhealth*.go",
					"internal/templates/deploy_build_test.go",
				},
			},
			{
				Name:   "Connect PermissionDenied wire code",
				Reason: "`svcerr.PermissionDenied` / `connect.CodePermissionDenied` is the standard Connect/gRPC status code an application returns from ITS OWN policy check. forge transports the code; it does not decide it.",
				Token:  regexp.MustCompile(`(?i)permission[_ ]?denied`),
			},
			{
				Name: "the lint rules that DETECT the removed annotations",
				Reason: "proto-options and vendored-protos exist precisely because a project's vendored " +
					"forge.proto kept declaring `authz_public` / `required_roles` / `authz_custom` / " +
					"`default_roles` on field numbers upstream had reserved: buf compiled them, forge's " +
					"own descriptor had no such fields, and 104 annotations across 14 service protos " +
					"declared an authorization posture enforced by nothing, silently. These rules name " +
					"the dead spellings in order to FIND them in user projects — the reference is the " +
					"feature working, and deleting it would delete the detection along with the record " +
					"of what it is for. Scoped to the lint rules' own files and to the removed option " +
					"spellings, so an actual role check or Authorize hook in these files still fails.",
				Token: regexp.MustCompile(`(?i)\bauthz\b|\brequire[sd]?[_-]?roles?\b|\bdefault_roles\b`),
				Paths: []string{
					"internal/cli/lint/lint_proto_options.go",
					"internal/cli/lint/lint_proto_options_test.go",
					"internal/cli/lint/lint_vendored_protos.go",
					"internal/cli/lint/help_surface_test.go",
				},
			},
			{
				Name: "Kubernetes RBAC prose where the Kubernetes noun is a line away",
				Reason: "Three places describe POD-identity RBAC in prose whose Kubernetes noun sits on a " +
					"neighbouring line, so the kubernetesNoun Context above cannot see it: README's KCL " +
					"model list (\"Application, Environment, ConfigMap, Ingress, RBAC\"), the " +
					"idp-provision workload comment explaining why that job needs a Role to PATCH a " +
					"ConfigMap in its own namespace, and the auth command-tree template's WHY IT NEEDS " +
					"RBAC paragraph about the ServiceAccount token every pod is projected. All three " +
					"are the ServiceAccount permissions that make `forge env deploy` work, not " +
					"application authorization — which none of these files has ever contained. Scoped " +
					"to the bare word in these three paths; every other RBAC spelling stays guarded.",
				Token: regexp.MustCompile(`(?i)\brbac\b`),
				Paths: []string{
					"README.md",
					"internal/codegen/workloads_kcl.go",
					"internal/templates/project/cmd-tree-auth.go.tmpl",
				},
			},
		},
	},
	{
		Name: "ambient-environment-knobs",
		Why: "forge/pkg — the library that compiles into every generated binary — no longer " +
			"changes its behaviour from the process environment. The authentication opt-out, the " +
			"operator gate, the leader-election knobs and the HMAC secret-by-env-var name were all " +
			"removed: a library reads what its caller passed, and a value the app declares nowhere " +
			"cannot be reviewed, deployed, or explained after the fact. Each has a field on the " +
			"options/config struct the package already had. Naming one of these variables in " +
			"guidance tells a reader to set something nothing reads — and in the auth case, to " +
			"believe a server can be run unauthenticated from a shell.",
		Patterns: []*regexp.Regexp{
			// The authentication opt-out. Word-bounded so an app's own,
			// unrelated AUTH_MODE (reliant has one) is not what this matches —
			// this repo simply must not have the spelling at all.
			regexp.MustCompile(`\bAUTH_MODE\b`),
			// The second operator gate serverkit consulted.
			regexp.MustCompile(`\bRUN_OPERATORS\b`),
			// The operatorkit knobs: lease identity/namespace, probe address,
			// client rate, and the leader-election timing trio.
			regexp.MustCompile(`\bLEADER_ELECTION_(?:ID|NAMESPACE)\b`),
			regexp.MustCompile(`\bHEALTH_PROBE_BIND_ADDRESS\b`),
			regexp.MustCompile(`\bOPERATOR_(?:CLIENT_QPS|CLIENT_BURST|LEASE_DURATION|RENEW_DEADLINE|RETRY_PERIOD)\b`),
			// The HMAC validator's "read the secret from the variable this
			// field names" indirection, in its Go and config-file spellings.
			regexp.MustCompile(`\bSecretEnv\b`),
			regexp.MustCompile(`\bsecret_env\b`),
			// cmdkit's flag-then-env resolver: the same laundering, one layer
			// down — the command stayed lint-clean because the read happened
			// inside the library.
			regexp.MustCompile(`\bcmdkit\.(?:Resolve|FirstNonEmpty)\b`),
		},
		Allowances: []allowance{
			{
				Name: "the tests that prove the variables are inert",
				Reason: "Each removal is pinned by a test that EXPORTS the variable and asserts " +
					"nothing changes — which is the only way \"you cannot disable auth from a shell\" " +
					"stays true. Those tests must name the spellings to set them. Scoped to the " +
					"assertion files so the names cannot drift back into shipped code or guidance.",
				Paths: []string{
					"pkg/authn/authn_test.go",
					"pkg/serverkit/operator_gate_test.go",
					"pkg/appkit/operatorkit/operatorkit_test.go",
					"internal/templates/project/middleware_test.go",
				},
			},
			{
				Name: "the historical survey in the operator design investigation",
				Reason: "docs/design/OPERATOR_AGNOSTIC.md §1 and FORGE_COMPOSITION.md quote an " +
					"external repository's manifests as they stood, file:line, as the evidence for a " +
					"design call. Rewriting a quoted survey falsifies the record. Both docs' " +
					"RECOMMENDATIONS were corrected, and OPERATOR_AGNOSTIC.md carries a note at the " +
					"top saying the mechanism changed.",
				Paths: []string{"docs/design/"},
			},
		},
	},
	{
		Name: "dev-auth-bypass",
		Why: "The dev-auth bypass — a synthetic bearer token the server accepted in dev mode — " +
			"was deleted server-side. Every client that still mints it is sending a credential " +
			"no backend honors, which fails as a confusing 401 instead of a missing feature.",
		Patterns: []*regexp.Regexp{
			// DEV_BYPASS_TOKEN, DevBypassToken, isDevBypass, devBypass,
			// dev-bypass-do-not-use-in-prod, NEXT_PUBLIC_AUTH_DEV_BYPASS.
			// The separator is optional but may not be a space, so prose that
			// merely NAMES the removed feature ("the dev-auth bypass is gone",
			// which is what a regression test must say) stays legal.
			regexp.MustCompile(`(?i)dev[_-]?bypass`),
			// The sentinel value itself, spelled out so a plain grep for the
			// literal lands here.
			regexp.MustCompile(`dev-bypass-do-not-use-in-prod`),
		},
	},
	{
		Name: "packs",
		Why: "The pack subsystem was retired wholesale — there is no pack root, no pack " +
			"manifest and no `packs` / `pack_overrides` / `features.packs` config key. What " +
			"packs installed is now owned scaffold plus forge/pkg libraries. Naming a pack " +
			"tells a reader (or an agent) to install something that cannot be installed.",
		// A pack reference is only a straggler when the pack it names is not on
		// disk — see packOnDisk. That keeps this entry correct without a pattern
		// edit if packs ever return, and makes a FUTURE pack deletion fail here
		// automatically.
		Resolve: packOnDisk,
		// SCOPE — read before widening. This entry forbids references that NAME
		// a pack or a pack ARTIFACT: things with an on-disk referent Resolve can
		// check. It deliberately does NOT police the word "pack" in prose.
		// forge still ships `crud.Pack` (the live response projection, ~50 uses)
		// and the English verb ("the read path never packs it"), so a pattern
		// broad enough to catch stale prose also catches those — and a pattern
		// that cries wolf gets weakened, which would defeat this guard for all
		// four removals. Naming a pack that cannot be installed is the harm;
		// that is what is caught here.
		Patterns: []*regexp.Regexp{
			// "the jwt-auth pack", "an auth pack", "the audit-log pack's".
			// Two things keep this off `crud.Pack`: the article + name make it a
			// noun phrase the English verb never forms, and `pack` must be
			// LOWERCASE, because the live Go identifier is always `Pack`.
			regexp.MustCompile("\\b(?:[Aa]n?|[Tt]he)\\s+`?([a-z][a-z0-9-]*)`?\\s+packs?\\b"),
			// "jwt-auth pack", "audit-log pack's" — a hyphenated pack name needs
			// no article to be unambiguous.
			regexp.MustCompile("`?\\b([a-z0-9]+(?:-[a-z0-9]+)+)`?\\s+packs?\\b"),
			// A pack PATH: packs/<name>, internal/packs.
			regexp.MustCompile(`\bpacks/([a-z0-9][a-z0-9._-]*)`),
			regexp.MustCompile(`\binternal/packs\b`),
			// The pack MANIFEST format.
			regexp.MustCompile(`(?i)\bpack\.yaml\b`),
			// The retired forge.yaml keys, in YAML and in Go/JSON literals. The
			// trailing colon is what separates the key from the English verb.
			regexp.MustCompile(`\bfeatures\.packs\b`),
			regexp.MustCompile(`\bpack_overrides\b`),
			regexp.MustCompile(`"?\bpacks"?\s*:\s*[\[{a-z"]`),
		},
		Allowances: []allowance{
			{
				Name:   "the removed-key registry",
				Reason: "This table is the MECHANISM that tells a user a forge.yaml key is gone: it must name `packs`, `pack_overrides` and `features.packs` to match them and print the migration message. Deleting these names would silently accept a retired key instead of rejecting it. File-wide because the registry's prose and its keys are inseparable.",
				Paths:  []string{"internal/config/validate.go"},
			},
		},
	},
	{
		Name: "root-component-manifest",
		Why: "The project-root `components.json` manifest was retired. What a project " +
			"CONTAINS is discovered from the code that declares it — the proto descriptor, " +
			"internal/workers/, internal/operators/, cmd/ — via codegen.DiscoverProjectComponents, " +
			"and the project KIND is read off the same tree (config.deriveProjectKindFromSources). " +
			"forge.yaml is project-global only and per-env config is KCL. A second file claiming " +
			"to say what a project contains is a source of truth that is wrong exactly when " +
			"nobody remembered to update it.",
		// READ BEFORE EDITING — the live look-alike.
		//
		// forge still SCAFFOLDS `deploy/kcl/components.k`
		// (codegen.ComponentsKCLRelPath): the project's own, TRACKED,
		// hand-editable declaration of what it is made of. It is written once
		// and never regenerated, which is exactly what separates it from the
		// retired root manifest — it is not a second source of truth forge
		// keeps in sync, it IS the source of truth, and it is KCL rather than
		// JSON. It must keep working; deleting it breaks every render.
		//
		// It is separated from the patterns below by SPELLING: the retired
		// manifest is `components.json` at the project ROOT, and this is
		// `components.k` under deploy/kcl/. See the components.k entry in
		// TestLegitimateLookalikesAreStillPresent, which fails if a widened
		// pattern ever swallows the live file.
		//
		// SCOPE: this entry forbids the retired FILE and the retired Go API
		// that read and wrote it. It deliberately does not police the word
		// "components" or a bare `components:` key — the KCL module, the KCL
		// schemas and the generated projection all use them legitimately, and
		// a pattern that cries wolf gets weakened, which would defeat this
		// guard for every removal in the table. (If forge ever ships a
		// shadcn/ui CLI config — also literally named components.json — that
		// is a real look-alike and earns a narrow Paths-scoped allowance, not
		// a pattern edit.)
		Patterns: []*regexp.Regexp{
			// The manifest by name, on any surface. The escaped dot is the
			// whole separation: `components_gen.json` cannot match it.
			regexp.MustCompile(`(?i)\bcomponents\.json\b`),
			// The kind derivation that read the manifest. Case-SENSITIVE and
			// \b-terminated, so the live `deriveProjectKindFromSources` (kind
			// from the real sources) and `EffectiveProjectKind` cannot reach
			// it.
			regexp.MustCompile(`\bDeriveProjectKind\b`),
			// The manifest reader/writers. All three name a components FILE,
			// which is the retired mechanism.
			regexp.MustCompile(`(?i)\b(?:hasComponentsFile|WriteComponentsFile|AppendComponentToFile)\b`),
		},
	},
	{
		Name: "the forge.components KCL package and its Server/Binary schemas",
		Why: "`Server` meant two different things: a PROTO SERVICE (a set of RPCs " +
			"mounted on a shared mux) and a DEPLOYABLE (an image + args + placement). " +
			"They are not the same object — one binary serving twelve Connect services " +
			"is ONE deployable — and conflating them invited a declaration per proto " +
			"service. A real project declared 14 `servers` that rendered 10 Deployments " +
			"from 2 binaries. What replaced it is `forge.workloads`, whose `Workload` " +
			"schema is exactly one deployable unit, with a `kind` discriminator " +
			"(service/worker/cron/job/operator/tool) instead of six subschemas that " +
			"shared every field and differed only in which expansion ran. " +
			"`Binary` is gone outright: it named a property EVERY workload has (they " +
			"are all executables with args) and its expansion was byte-identical to " +
			"`Worker`'s. What it MEANT — built into the image, never scheduled — is " +
			"`kind = \"tool\"`, which renders no manifest at all rather than an " +
			"unscheduled Deployment nobody addressed.",
		Patterns: []*regexp.Regexp{
			// The package, on any surface (import path or prose).
			regexp.MustCompile(`\bforge\.components\b`),
			// The retired schema names, as a project would write them. Anchored
			// on the `fc.` alias the old scaffold used so the English words
			// "server" and "binary" cannot match.
			regexp.MustCompile(`\bfc\.(?:Server|Worker|Cron|Job|Operator|Binary|Component|ComponentPort|ComponentEnv)\b`),
			// The retired render entry point + env schema.
			regexp.MustCompile(`\brender_components\b|\bComponentEnv\b|\bComponentPort\b`),
			// The retired Go emitters. Their replacements are Workload*.
			regexp.MustCompile(`\b(?:ComponentsKCLRelPath|ComponentsKCLExists|ComponentStanza|ComponentStanzaHint|AppendComponentStanza|ScaffoldComponentsKCL)\b`),
		},
		Allowances: []allowance{
			{
				Name: "the changelog entry announcing the rename",
				Reason: "The CHANGELOG has to name what it removed, or the entry cannot " +
					"tell a reader upgrading what stopped existing.",
				Token: regexp.MustCompile(`forge\.components|fc\.(?:Server|Worker|Cron|Job|Operator|Binary|Component|ComponentPort|ComponentEnv)|render_components|ComponentEnv|ComponentPort|ComponentsKCLRelPath|ComponentsKCLExists|ComponentStanza|ComponentStanzaHint|AppendComponentStanza|ScaffoldComponentsKCL`),
				Paths: []string{"CHANGELOG.md"},
			},
		},
	},
	{
		Name: "generated components_gen.json",
		Why: "Deploy has ONE source of truth and it is KCL. forge used to also emit " +
			"`deploy/kcl/components_gen.json` — a gitignored, regenerated-every-run " +
			"projection of the discovered Inventory that each per-env main.k read via " +
			"`fc.load_components(fc.COMPONENTS_GEN)`. That split one concept across two " +
			"files, one of them untracked, so a FRESH CLONE rendered ZERO manifests " +
			"SILENTLY until someone ran `forge generate`. It also made forge decide " +
			"things it has no business deciding: which components exist in which " +
			"environment, and whether a migration step runs. Those are per-env choices " +
			"a project must be able to hand-edit — bring in NATS, use a hosted database " +
			"in prod and a container in dev, change a port — and a file forge rewrites " +
			"every run cannot hold them. What replaced it is `deploy/kcl/components.k`: " +
			"scaffolded ONCE, tracked, appended to by `forge scaffold`, never " +
			"regenerated. Drift is reported by `forge lint`, not repaired.",
		Patterns: []*regexp.Regexp{
			// The file, on any surface. Underscore-anchored, so the live
			// `components.k` cannot match it.
			regexp.MustCompile(`\bcomponents_gen\.json\b`),
			// The Go API that wrote it.
			regexp.MustCompile(`\b(?:ComponentsJSONRelPath|GenerateComponentsJSON|ComponentsToJSON)\b`),
			// The KCL loaders that read it. `load_components`/`load_migrate`
			// were the only readers; COMPONENTS_GEN was the path constant.
			regexp.MustCompile(`\b(?:load_components|load_migrate|COMPONENTS_GEN)\b`),
		},
		Allowances: []allowance{
			{
				Name: "the changelog entry announcing the removal",
				Reason: "The CHANGELOG has to name what it removed, or the entry cannot " +
					"tell a reader upgrading what stopped existing.",
				Token: regexp.MustCompile(`components_gen\.json|ComponentsJSONRelPath|GenerateComponentsJSON|ComponentsToJSON|load_components|load_migrate|COMPONENTS_GEN`),
				Paths: []string{"CHANGELOG.md"},
			},
		},
	},
	{
		Name: "project add / project scaffold",
		Why: "Scaffolding collapsed onto ONE verb where arity picks the granularity: " +
			"`forge scaffold` births everything the protos imply, `forge scaffold <noun> …` " +
			"scaffolds exactly one thing. The two-word spellings `forge project add <noun>` " +
			"and `forge project scaffold` are gone — no alias, no hidden name, no shim. " +
			"`add` was the wrong verb (these commands write code, they do not add to a list) " +
			"and `project` carried no information (every forge command operates on a project).",
		Patterns: []*regexp.Regexp{
			// The fully-spelled invocation, in help text, docs, skills,
			// comments and error hints.
			regexp.MustCompile(`forge\s+project\s+(?:add|scaffold)\b`),
			// The same command with the binary name substituted at runtime
			// ("%s project add entity", "reliant forge project add worker").
			// Anchored on a real noun so the English "project scaffold"
			// (as in "a project scaffolded by an older forge") cannot reach
			// it.
			regexp.MustCompile(`\bproject\s+add\s+(?:service|worker|operator|crd|frontend|scenario|webhook|package|adapter|binary|library|handler-file|rpc|entity)\b`),
			// The Go ARGV form: exec/test invocations pass the command as
			// separate string args, so the tokens are never adjacent in the
			// source and neither pattern above can see them. 25 files —
			// almost the whole `-tags e2e` corpus — carried the old spelling
			// past the rename precisely this way, and the lane compiles
			// without running, so nothing failed until one was executed.
			regexp.MustCompile(`"project",\s*"(?:add|scaffold)"`),
		},
		Allowances: []allowance{
			{
				Name: "the tests that prove the old spellings are gone",
				Reason: "These files assert against the assembled cobra tree that `forge project add …` " +
					"and `forge project scaffold` resolve to nothing — they must name the removed " +
					"spellings to test for their absence. Scoped by both path and token, so any OTHER " +
					"reference in the same files still fails.\n" +
					"group_strict_test.go earned its entry the hard way: it was NOT listed, the argv " +
					"pattern flagged its `{\"project\", \"add\", \"entity\"}` case, and the case was " +
					"swept to `{\"scaffold\", \"entity\"}` — which is a VALID command, so the " +
					"assertion silently stopped testing anything and started failing for an unrelated " +
					"reason. A guard that forces a rewrite of the one test asserting a thing is absent " +
					"destroys the assertion it was protecting.",
				// Both spellings: the user-typed form, and the Go ARGV form these
				// assertions actually use (cobra Find takes a []string).
				Token: regexp.MustCompile(`(?:forge\s+)?project\s+(?:add|scaffold)(?:\s+[a-z-]+)?|"project",\s*"(?:add|scaffold)"`),
				Paths: []string{
					"internal/cli/scaffold_surface_test.go",
					"internal/cli/new_next_steps_test.go",
					"internal/cli/group_strict_test.go",
				},
			},
			{
				Name: "the changelog entry announcing the rename",
				Reason: "A Keep-a-Changelog `### Changed` entry has to name the spelling it replaced, " +
					"or readers cannot tell which of their invocations broke. The changelog is prose " +
					"about history, not a shipped surface — no tool reads it, and nothing scaffolds from it.",
				Token: regexp.MustCompile(`(?:forge\s+)?project\s+(?:add|scaffold)(?:\s+[a-z-]+)?`),
				Paths: []string{"CHANGELOG.md"},
			},
		},
	},
	{
		Name: "per-component port",
		Why: "A component never carries a port. Every service in a binary mounts onto the SAME " +
			"Connect mux and the process listens ONCE, on AppConfig.port (env PORT, default " +
			"8080) — config.DefaultServePort. Any other port is a DEPLOY fact declared per " +
			"environment in deploy/kcl/<env>/main.k on forge.workloads.Workload.ports, so " +
			"nothing forge introspects (the proto descriptor, owned worker/operator files, " +
			"cmd/) can state one. The Go-side carrier — ComponentConfig.Ports, PortSpec, " +
			"HTTPPortName, PrimaryPort() and the components_gen.json `ports` key — is gone. " +
			"It came back once already: e3b7fa39 deleted the port HEURISTIC but left the " +
			"FIELD, and the always-zero it returned then flowed into six consumers, an audit " +
			"finding that could never fire, a generated architecture doc that printed `0`, " +
			"and a scaffolded frontend baked to http://localhost:0.",
		Patterns: []*regexp.Regexp{
			// The accessor whose answer was always zero, and the two types
			// that carried it. All three are case-SENSITIVE Go identifiers
			// with no live homonym: the KCL schema is spelled ComponentPort
			// (no "Spec"), and a k8s/compose port list is `ports`/`Ports`,
			// which none of these can reach.
			regexp.MustCompile(`\bPrimaryPort\b`),
			regexp.MustCompile(`\bPortSpec\b`),
			regexp.MustCompile(`\bHTTPPortName\b`),
			// The JSON projection of the field, and the key it wrote into
			// components_gen.json. Anchored on `ports` as a JSON key with the
			// component doc's exact spelling, so `listen_ports`, `egress_ports`,
			// `host_ports` and K8sCluster's own `ports` stay untouched.
			regexp.MustCompile(`\bcomponentPortJSON\b`),
			// SCOPE: this entry forbids the per-COMPONENT port carrier, not
			// the word "port". Ports are real and everywhere — the KCL
			// ComponentPort schema, HostDeploy.listen_ports, K8sCluster.ports,
			// the frontend dev-server port, PORT itself. A pattern that
			// policed those would cry wolf, and a guard that cries wolf gets
			// weakened until it guards nothing.
		},
		Allowances: []allowance{
			{
				Name: "the dead-code guard's record of this exact defect",
				Reason: "deadcodeguard's doc, quarantine ledger and testdata fixture reproduce the " +
					"historical defect BY NAME — a planted Component.Ports/PrimaryPort pair and the " +
					"false-green test over it — because that is the shape its phantom-field rule " +
					"exists to catch. That is the guard's evidence, not a live carrier. Scoped to " +
					"that one package by path and to these identifiers by token.",
				Token: regexp.MustCompile(`\bPrimaryPort\b|\bPortSpec\b|\bHTTPPortName\b|\bcomponentPortJSON\b`),
				Paths: []string{"internal/deadcodeguard/"},
			},
			{
				Name: "the changelog entry announcing the removal",
				Reason: "A Keep-a-Changelog `### Removed` entry has to name the identifiers it removed, " +
					"or a reader whose build just broke cannot tell which symbol went away. The " +
					"changelog is prose about history, not a shipped surface — no tool reads it, and " +
					"nothing scaffolds from it.",
				Token: regexp.MustCompile(`\bPrimaryPort\b|\bPortSpec\b|\bHTTPPortName\b|\bcomponentPortJSON\b`),
				Paths: []string{"CHANGELOG.md"},
			},
		},
	},
	{
		Name: "the entity-annotation convention rules",
		Why: "`forgeconv-pk-annotation` and `forgeconv-timestamps` fired only on messages " +
			"carrying `(forge.v1.entity)`, and told the author to add `pk: true` / " +
			"`(forge.v1.field)`. Those annotations are retired — `forge generate` prints a " +
			"notice when it sees one, and the migration skill says to delete them. So the " +
			"rules could only ever teach the shape forge had just deleted, and only to " +
			"someone who had not migrated yet. Entities come from SQL now: db/migrations " +
			"drive the ORM projections, and the primary key and timestamp columns are the " +
			"migration's business. Two rules, and the whole annotation-tracking half of the " +
			"proto mini-parser that existed to feed them, are gone.",
		Patterns: []*regexp.Regexp{
			regexp.MustCompile(`forgeconv-pk-annotation`),
			regexp.MustCompile(`forgeconv-timestamps`),
			// The Go identifiers behind them, so a resurrection under the old
			// name is caught even before it acquires a rule string.
			regexp.MustCompile(`\bcheckPKAnnotation\b|\bcheckTimestampAnnotation\b|\bisTimestampShapedFieldName\b`),
			// The parser fields that existed only to feed the two rules. A
			// re-introduced HasEntityAnnotation is the same removal coming
			// back through the parser rather than through a rule.
			regexp.MustCompile(`\bHasEntityAnnotation\b|\bHasTimestampsTrue\b|\bHasSoftDeleteTrue\b|\bHasFieldAnnotation\b|\bHasPKTrue\b|\btrackEntityOpts\b`),
		},
	},
	{
		Name: "the marker-scaffold CRUD test pair",
		Why: "GenerateCRUDTests used to emit two files per service: `handlers_crud_gen_test.go` " +
			"(per-RPC AnyOutcome frames) and `handlers_crud_integration_test.go` (a build-tag-gated " +
			"suite). Both were retired for ONE user-owned lifecycle test, `handlers_crud_test.go`, " +
			"and GenerateCRUDTests now DELETES them from projects that still carry them. The " +
			"references survived anyway, and the worst one was on the success path: `forge generate` " +
			"printed \"Generated handlers/<svc>/handlers_crud_gen_test.go (unit) + " +
			"handlers_crud_integration_test.go (-tags integration)\" — announcing, by name, two files " +
			"it had just deleted and never writes. A `_gen` suffix also states forge OWNS the file; " +
			"the file it really writes is scaffold-once and user-owned, so the message inverted " +
			"ownership on the one line the author reads.",
		Patterns: []*regexp.Regexp{
			regexp.MustCompile(`\bhandlers_crud_gen_test\.go\b`),
			regexp.MustCompile(`\bhandlers_crud_integration_test\.go\b`),
		},
		Allowances: []allowance{
			{
				Name: "the retirement sweep that deletes them",
				Reason: "removeRetiredScaffoldTest has to NAME both files to delete them from projects " +
					"scaffolded before the retirement. Naming a file in order to remove it is the " +
					"removal, not a reference to it. Scoped to the two call lines and their comment " +
					"in the generator that performs the sweep.",
				Token: regexp.MustCompile(`\bhandlers_crud_gen_test\.go\b|\bhandlers_crud_integration_test\.go\b`),
				Paths: []string{"internal/codegen/crud_gen.go"},
			},
			{
				Name: "the ignore rule keeping a legacy project's copy committed",
				Reason: "`handlers/**/*_gen.go` is ignored, so a legacy project that still carries the " +
					"retired handlers_crud_gen_test.go would have it swept up by the glob. The " +
					".gitignore comment names the file to explain why *_gen_test.go is deliberately " +
					"NOT ignored — committing it is what keeps `go test ./...` green on a fresh " +
					"clone of such a project. The reference exists to protect a file forge no " +
					"longer writes, not to keep writing it. Scoped to .gitignore.",
				Token: regexp.MustCompile(`\bhandlers_crud_gen_test\.go\b`),
				Paths: []string{".gitignore"},
			},
			{
				Name: "the tests that assert both files are ABSENT",
				Reason: "The retirement sweep is covered by tests that build a project carrying the old " +
					"pair and assert it is gone afterwards. They must name the files to look for them. " +
					"Forcing a rewrite of the one test proving a thing is absent is how a guard " +
					"deletes its own evidence.",
				Token: regexp.MustCompile(`\bhandlers_crud_gen_test\.go\b|\bhandlers_crud_integration_test\.go\b`),
				Paths: []string{
					"internal/codegen/crud_gen_test.go",
					"internal/codegen/generator_test.go",
					"internal/linter/scaffolds/scaffolds_test.go",
				},
			},
			{
				Name: "migration skills addressing projects that already OWN the file",
				Reason: "What was removed is forge EMITTING the pair. A project scaffolded before the " +
					"retirement that then cleared every FORGE_SCAFFOLD marker owns its " +
					"handlers_crud_gen_test.go outright — the sweep leaves those alone — so a " +
					"migration skill still has to name the file to tell that reader what to do with " +
					"it. Same carve-out the sibling `handlers_crud_gen.go` pre-split name already " +
					"earns in knownGeneratedHandlerFiles.",
				Token: regexp.MustCompile(`\bhandlers_crud_gen_test\.go\b|\bhandlers_crud_integration_test\.go\b`),
				Paths: []string{
					// Any per-release migration skill under migrations/
					// earns this carve-out: naming the retired file is how
					// it tells a reader who still owns one what to do.
					"**/migrations/*/SKILL.md",
					"**/migration-upgrade/SKILL.md",
					"internal/templates/skills_validation_test.go",
				},
			},
			{
				Name: "the .gitignore note explaining the retirement to legacy projects",
				Reason: "The scaffolded .gitignore deliberately documents that a LEGACY project may " +
					"still carry the retired file and that it must be committed either way. Naming " +
					"a retired file in order to say it is retired is the removal, not a reference.",
				Token: regexp.MustCompile(`\bhandlers_crud_gen_test\.go\b|\bhandlers_crud_integration_test\.go\b`),
				Paths: []string{"internal/templates/project/.gitignore"},
			},
		},
	},
	{
		Name: "the root spellings of the `forge env` verbs",
		Why: "Every environment-REQUIRED lifecycle verb moved under the `env` noun and now takes " +
			"the environment as a POSITIONAL argument: `forge env up dev`, `forge env deploy prod`. " +
			"The root spellings — `forge up`, `forge down`, `forge deploy`, `forge smoke`, " +
			"`forge devstack`, `forge promote`, `forge secrets` — are gone, no alias and no hidden " +
			"name, and so is the `--env=<env>` flag that used to carry the environment on up/down. " +
			"An env is what these commands act ON, not a modifier, and `cobra.ExactArgs(1)` turns " +
			"forgetting it into an error instead of a silent default. Commands where the env really " +
			"IS an optional modifier (`forge build [environment]`, `forge run --env`) kept their " +
			"flag and stayed at the root. A surviving `forge up --env=dev` is a copy-pasteable " +
			"command — in a doc, a skill, a KCL comment or a scaffolded script — that now dies on " +
			"\"unknown command\".",
		Patterns: []*regexp.Regexp{
			// The removed root verbs, on any surface. The trailing \b is what
			// keeps `forge upgrade` out (and `forge deploys`, the English verb);
			// requiring `forge` IMMEDIATELY before the verb is what keeps both
			// the live `forge env deploy` and the live siblings `forge cluster
			// up` / `forge db migrate up` out.
			regexp.MustCompile(`\bforge\s+up\b`),
			regexp.MustCompile(`\bforge\s+down\b`),
			regexp.MustCompile(`\bforge\s+(?:deploy|smoke|devstack|promote|secrets)\b`),
			// The removed FLAG, anchored on the verb it belonged to — which
			// also catches the half-renamed `forge env up --env=dev`, the shape
			// a mechanical sweep produces and the one that reads as correct.
			// Anchoring on up/down is what leaves `forge build --push --env=dev`
			// and `forge run --env=staging` alone; requiring `=` or a space
			// after the flag is what leaves `docker compose up --env-file` alone.
			regexp.MustCompile(`\b(?:up|down)\s+--env[= ]`),
			// The Go ARGV form: exec/test invocations pass the command as
			// separate string args, so the tokens are never adjacent in the
			// source and neither pattern above can see them.
			regexp.MustCompile(`"(?:up|down)",\s*"--env`),
		},
	},
	{
		Name: "the `forge dev` command group",
		Why: "The k3d lifecycle and dev-state introspection verbs were promoted flat out of the " +
			"`dev` namespace onto `forge cluster`: `forge cluster up|down|reset|reload|status|" +
			"logs|info|urls|instances`. `forge dev` itself is not a command — nothing resolves it. " +
			"`forge dev port-forward` did not move at all: host port-forwarding was RETIRED in " +
			"favour of the Gateway API ingress model, where a service is reachable through a " +
			"declared route or not at all, so a doc that still names it promises a way out that " +
			"no longer exists.",
		Patterns: []*regexp.Regexp{
			// SCOPE — read before widening. This entry forbids `forge dev`
			// followed by a SUBCOMMAND. It deliberately does NOT police a bare
			// "forge dev", because that is a live English adjective all over the
			// tree: "a forge dev server", "every forge dev namespace", "the
			// forge dev loop", "a forge dev capability", "the forge dev
			// controller", "the forge dev-stack convention". A pattern broad
			// enough to catch stale prose also catches those — and a pattern
			// that cries wolf gets weakened, which would defeat this guard for
			// every removal in the table. Naming a subcommand that cannot be
			// invoked is the harm; that is what is caught here.
			regexp.MustCompile(`\bforge\s+dev\s+(?:cluster|status|info|logs|urls|instances|port-forward|up|down|reset|reload)\b`),
		},
	},
	{
		Name: "the retired package kinds",
		Why: "forge named six internal-package \"kinds\"; four bought nothing a rule could read " +
			"and are gone. `//forge:strategy` folded into `//forge:exclude-contract` (which says " +
			"the same thing and has real users). `//forge:interactor` and its " +
			"`forge scaffold package --type interactor` scaffold are gone — an orchestrator is a " +
			"service whose deps are other services' interfaces, so `--type service` already " +
			"births it, and the three-file interactor template tree emitted the service scaffold " +
			"with different comments. The \"utility\" kind was never a kind: it was " +
			"`interfaceCount == 0`, a rule predicate that needed no name. And the rule the " +
			"interactor marker gated is now `forgeconv-deps-are-interfaces`, un-gated and " +
			"applying wherever a `type Deps struct` exists — the gate is exactly why it fired on " +
			"zero packages while control-plane carried 28 concrete-typed Deps fields.",
		Patterns: []*regexp.Regexp{
			// SCOPE — read before widening. This entry forbids the MARKERS, the
			// FLAG VALUE, the retired rule id and the deleted Go API. It
			// deliberately does NOT police the words "interactor", "utility" or
			// "strategy". All three survive as ordinary design vocabulary that
			// forge still ships on purpose: the `interactor` skill (which now
			// opens by saying an interactor is NOT a forge package kind),
			// `forge scaffold package`'s own help explaining why an orchestrator
			// needs no flag, "pure-utility packages" in the contracts skill, and
			// "multiple implementations / strategy pattern" in service-layer.
			// A pattern broad enough to catch stale prose also catches those —
			// and a pattern that cries wolf gets weakened, which would defeat
			// this guard for every removal in the table. Naming a marker no
			// parser reads, or a flag value that now exits non-zero, is the
			// harm; that is what is caught here.
			regexp.MustCompile(`forge:strategy\b`),
			regexp.MustCompile(`forge:interactor\b`),
			// The flag value, in every spelling a doc, a skill, a charter or a
			// help string writes it: `--type interactor`, `--type=interactor`,
			// and the half-swept `--type=adapter|interactor`.
			regexp.MustCompile(`--type[= ]"?[a-z|]*interactor`),
			// The Go ARGV form: exec/test invocations pass the flag and its
			// value as separate string args, so the tokens are never adjacent
			// in the source and the pattern above cannot see them.
			regexp.MustCompile(`"--type",\s*"interactor"`),
			// Re-adding it to the flag map — the resurrection that happens
			// before any help text or doc mentions it.
			regexp.MustCompile(`"interactor":\s*true`),
			// The deleted three-file template tree.
			regexp.MustCompile(`internal-package/interactor\b`),
			// The retired rule id. The live successor is spelled
			// `forgeconv-deps-are-interfaces`; requiring the `interactor-`
			// infix is what keeps this off it.
			regexp.MustCompile(`forgeconv-interactor-deps-are-interfaces`),
			// The Go API behind all of it, so a resurrection is caught before
			// it acquires a marker, a flag or a rule string. All are
			// case-SENSITIVE identifiers with no live homonym.
			regexp.MustCompile(`\bHasInteractorDirective\b|\bInternalPackageInteractor\b|\bRuleInteractorDepsAreInterfaces\b`),
			// The "utility kind" predicate and the file that held it, plus the
			// strategy-marker reader.
			regexp.MustCompile(`\bisUtilityPackage\b|\bfileHasStrategyDirective\b|\butility_skip\.go\b`),
		},
	},
	{
		Name: "the seeder's demo identity and its table-name classifiers",
		Why: "pkg/seedplan used to read a table's NAME for domain meaning. `detectIdentity` " +
			"elected any table called `users`/`user` with a single string `id` key to be THE user " +
			"table, and stamped row 0 with DemoUserID/DemoUserEmail/DemoUserName — literals it " +
			"copied off the JWT scaffold and declared forge-wide canonical. `isOrgTableName` " +
			"carried a list of organization-shaped English spellings that OVERRODE the column " +
			"evidence beside it, so a table named `agencies` with first_name/last_name/" +
			"date_of_birth was still classified as a company. Both are gone, along with " +
			"identityKind and the hasSingleStringIDPK shape test that existed only to feed the " +
			"first one.\n" +
			"The demo identity was not cosmetic: `dev-user-001` is not a UUID, so a `users` table " +
			"with a `uuid` primary key — an entirely ordinary schema — made postgres reject the " +
			"INSERT, and because `forge db seed` is one transaction, that ONE table took the whole " +
			"dataset down with it. Which row is the authenticated principal is domain knowledge " +
			"the app declares; forge does not invent a user for an app that has none. The " +
			"COLUMN-name half of the same disease is the entry below.",
		Patterns: []*regexp.Regexp{
			// The three exported literals, and the constant block that held them.
			regexp.MustCompile(`\bDemoUser(?:ID|Email|Name)\b`),
			// The classifiers and the enum they produced. All case-SENSITIVE Go
			// identifiers with no live homonym anywhere in the tree.
			regexp.MustCompile(`\bdetectIdentity\b`),
			regexp.MustCompile(`\bisOrgTableName\b`),
			regexp.MustCompile(`\bhasSingleStringIDPK\b`),
			regexp.MustCompile(`\bidentity(?:Kind|None|User)\b`),
			// SCOPE — read before widening. This entry forbids the seeder's demo
			// IDENTITY and its table-name classifiers. It deliberately does NOT
			// police the literals `dev-user-001` / `dev@localhost` / `Dev User`:
			// those are the FRONTEND dev session and the JWT test minter's claims,
			// which are live, separately owned, and unrelated to what any row of a
			// seeded database contains. The seeder's sin was ADOPTING them as a
			// forge-wide truth, and that is what the identifiers above catch.
			// It also does not police the word "identity" — pod identity, an
			// identity mapping and an IdP identity claim are all live vocabulary.
		},
	},
	{
		Name: "the seeder's column-name domain heuristics",
		Why: "pkg/seedplan used to decide what a column MEANS from what it was CALLED. " +
			"`price`/`amount`/`*_cents` were money, `currency`/`*_currency` pinned to the constant " +
			"USD (even overriding a CHECK vocabulary that offered EUR and GBP), `*color*`/`*_hex` " +
			"and the design tokens primary/secondary/accent/background drew hex colors, `email`/" +
			"`*_email` drew addresses, `phone` drew numbers, `uri`/`link`/`*_url` drew URLs, " +
			"`avatar_url`/`image_url` drew a data: URI, `last4` drew four digits, `date_of_birth`/" +
			"`dob`/`born_on` drew birthdates, `date`/`*_date`/`*_on` drew ISO dates, `role` drew " +
			"{admin, member, viewer, editor, owner}, and `name`/`full_name` drew from a company " +
			"pool or — via detectPersonish, which read the SIBLING columns — a person pool. " +
			"Alongside them lived a dozen sample* vocabularies (names, titles, descriptions, " +
			"addresses, cities, states, countries, statuses, roles, types, locales, timezones) and " +
			"the placeholderPalette that fed the color and image branches.\n" +
			"All of it is gone. What noun belongs in a column is a DECISION — it is not in the " +
			"schema, forge cannot derive it, and a guess is right for the vocabulary it was " +
			"written against and silently wrong everywhere else: the heuristics satisfied an " +
			"email-format CHECK on a column spelled `email` and violated the identical CHECK on " +
			"one spelled `contact`, which aborts the whole transactional seed. The declaration " +
			"surface is db/seeds/vocab.yaml. What replaced them derives from what the author " +
			"DECLARED — the canonical type, the CHECK vocabulary, a regex CHECK (seedplan." +
			"SynthString builds a value from the pattern itself), length and range bounds, NOT " +
			"NULL, UNIQUE, the foreign keys — and an undescribed column gets the emitter's " +
			"self-labelling placeholder, seedplan.SyntheticStringPrefix + column + row.",
		Patterns: []*regexp.Regexp{
			// The dispatchers. Case-SENSITIVE Go identifiers; `isDateColumn`
			// and friends have no live homonym in the tree.
			regexp.MustCompile(`\bisCurrencyColumn\b|\bisColorColumn\b|\bisBirthDateColumn\b|\bisDateColumn\b`),
			regexp.MustCompile(`\bdetectPersonish\b|\bpersonFullName\b|\bpersonish\b`),
			regexp.MustCompile(`\bstringValue\b|\bintegerLiteral\b|\bSynthStringValue\b`),
			regexp.MustCompile(`\bisoDate\b|\bplaceholderImageURI\b|\bsvgURLEscape\b|\bplaceholderPalette\b`),
			// The vocabularies themselves. Case-SENSITIVE and \b-anchored, so
			// the lowercase `sample` never reaches a live camelCase-embedded
			// identifier.
			regexp.MustCompile(`\bsample(?:Names|FirstNames|LastNames|Titles|Descriptions|Addresses|Cities|States|Countries|Statuses|Roles|Types|Locales|Timezones)\b`),
			// SCOPE — read before widening. This entry forbids the SEEDER's
			// column-NAME dispatch and the pools it fed. Two things it
			// deliberately does not police:
			//
			//  1. The column names themselves. `email`, `price_cents`,
			//     `deleted_at` and the rest are ordinary schema vocabulary that
			//     every fixture, migration and skill legitimately spells.
			//  2. `SyntheticStringPrefix` and the `sample_` value it holds —
			//     that is the LIVE stamp the replacement puts on what it
			//     invents, spelled `sample_` on purpose.
			//
			// The frontend mock generator's second copy of the same heuristics
			// has its own entry below — it was a separate decision, made
			// separately.
		},
		Allowances: []allowance{
			{
				Name: "the frontend-config projector's own stringValue helper",
				Reason: "internal/cli/generate_frontend_config.go has an unrelated four-line " +
					"`stringValue(v any) (string, bool)` — a type assertion that reads a KCL-projected " +
					"config value as a string, reporting a non-string as \"not a string here\" rather " +
					"than coercing it. It reads OIDC issuer/client-id out of a rendered config map; " +
					"it draws nothing, knows no column names, and predates none of the seeder's " +
					"heuristics.\n" +
					"The generic lowercase spelling in the pattern above is what collides: no " +
					"`stringValue` — nor `integerLiteral`, nor `SynthStringValue` — appears anywhere " +
					"in pkg/seedplan's history; seedplan's real names are SynthString and " +
					"SyntheticStringPrefix. Scoped to this one file, so a genuine seeder heuristic " +
					"landing here (isCurrencyColumn, detectPersonish, samplesNames …) still fails.",
				Token: regexp.MustCompile(`\bstringValue\b`),
				Paths: []string{"internal/cli/generate_frontend_config.go"},
			},
			{
				Name: "seedplan's DECLARED-type date check",
				Reason: "pkg/seedplan/synth.go has an `isDateColumn(col)` that reads col.DeclType and " +
					"reports whether the column was DECLARED `DATE` — the one time type with no " +
					"time-of-day to render. keyTimeLiteral uses it to format a TIME key member " +
					"date-only rather than as a full instant.\n" +
					"It is the exact inverse of the removed heuristic, which is why the name " +
					"collides: the heuristic guessed a column's MEANING from what it was CALLED " +
					"(`date`/`*_date`/`*_on` drew an ISO date). This reads what the author " +
					"DECLARED in the schema, which is precisely the replacement's rule. Scoped to " +
					"the one file and the one identifier, so isCurrencyColumn, isColorColumn and " +
					"isBirthDateColumn still fail anywhere, and a name-sniffing isDateColumn " +
					"landing in any other file still fails too.",
				Token: regexp.MustCompile(`\bisDateColumn\b`),
				Paths: []string{"pkg/seedplan/synth.go"},
			},
		},
	},
	{
		Name: "the hand-written unauthenticated allow-list",
		Why: "The scaffolded pkg/middleware carried a hand-maintained map of procedure strings that " +
			"the auth interceptor read, alongside `(forge.v1.method).auth_required` in the proto — " +
			"two declaration surfaces for one fact. They could disagree, and only the map did " +
			"anything: an RPC could declare auth_required: true, be reported as authenticated by " +
			"`forge project graph` and the MCP manifest, and still serve anonymous callers. One " +
			"measured app shipped 17 of 20 CRUD RPCs open that way.\n" +
			"The proto is the declaration now. forge projects `auth_required: false` into the " +
			"Tier-1 pkg/middleware/procedures_gen.go as connect's own …Procedure constants, and " +
			"the scaffolded serve wiring runs the interceptor FAIL-CLOSED (AnonymousOK: false) " +
			"against it. Publishing an endpoint is one edit, on the rpc that declares it.",
		Patterns: []*regexp.Regexp{
			// The removed identifier. Case-SENSITIVE and \\b-anchored: the LIVE
			// generated symbol is the exported UnauthenticatedProcedures, and
			// `Unauthenticated` (the authn.Policy field) is untouched.
			regexp.MustCompile(`\bunauthenticatedProcedures\b`),
			// The non-gating posture the scaffold used to ship. A project may
			// still set it deliberately in its own tree; what may not come
			// back is forge GENERATING it — the serve template is the only
			// place in this repo that writes the literal, and
			// internal/templates/auth_test.go pins it to false.
			regexp.MustCompile(`AuthDeps\{AnonymousOK: true\}`),
		},
	},
	{
		Name: "the frontend mock generator's own demo vocabulary",
		Why: "internal/codegen/frontend_mocks.go carried a SECOND copy of the column-name " +
			"heuristics the seeder shed: pools for name/first_name/last_name/title/description/" +
			"status/role/type, integer ranges for age/price/quantity, float ranges keyed on the " +
			"substrings probability/ratio/percent, and a foreign-key target guessed by " +
			"pluralizing an `_id` stem with an \"s\".\n" +
			"It shipped one application with TWO demo vocabularies — the database said " +
			"`sample_name_1` where the frontend said \"Acme Corp\" — and, reading no constraints " +
			"at all, it mocked a `sku` column whose CHECK is `^[A-Z]{3}-[0-9]{4}$` as " +
			"`sample_sku_3`: data the very API the mock stands in for would reject. The pluralize " +
			"guess named tables that do not exist (`categorys`), so mock foreign keys referenced " +
			"ids no fixture carried.\n" +
			"There is now ONE dataset. codegen.SeedProjection resolves the project's own " +
			"seedplan.Plan and the fixtures carry what that plan writes at each (table, column, " +
			"row) — vocabulary from db/seeds/vocab.yaml, values from the CHECK constraints, keys " +
			"and references from the real foreign keys — and a column nothing describes gets the " +
			"same self-labelling placeholder in both places.",
		Patterns: []*regexp.Regexp{
			// The pools. Case-SENSITIVE and anchored on the `mock` prefix, so
			// the seeder entry's own `sample*` patterns and the live
			// `sample_` stamp are untouched. `[A-Z]` catches any pool name,
			// including one added later.
			regexp.MustCompile(`\bmockSample[A-Z]`),
			// SCOPE — read before widening. This entry forbids the mock
			// generator's own VOCABULARY. It deliberately does not police
			// `mockGenerateStringValue` / `mockGenerateIntegerValue`, which
			// are live: they are what a cell falls back to when the project
			// has no dataset to agree with, and they now emit only the
			// synthetic placeholder and the row number.
		},
	},
	{
		Name: "the `//forge:adapter` marker spelling",
		Why: "The marker was RENAMED to `//forge:outbound-io`, named for what it asserts — this " +
			"package calls OUT to a third-party system and serves nothing inbound — so knowing " +
			"when to stamp it requires reading the package, not learning a taxonomy. Nothing " +
			"parses `forge:adapter` any more, so a package still carrying it silently loses the " +
			"outbound-io-no-rpc lint and the observe heuristic's I/O signal: it does not fail, " +
			"it stops checking. The rule id `forgeconv-adapter-no-rpc` moved with it.",
		Patterns: []*regexp.Regexp{
			// SCOPE — read before widening. This entry forbids the MARKER
			// spelling and the retired rule id and Go API. It deliberately does
			// NOT police the word "adapter", which forge keeps everywhere ON
			// PURPOSE: `forge scaffold package --type adapter`, the `adapter`
			// skill, internal/templates/internal-package/adapter/, and the
			// prose calling a package an outbound adapter. "Adapter" is the
			// design pattern; outbound-io is the invariant a linter can check,
			// and only the second one was renamed. The `forge:` prefix is the
			// whole separation — see the adapter entries in
			// TestLegitimateLookalikesAreStillPresent, which fail if a widened
			// pattern ever swallows the live verb or skill.
			regexp.MustCompile(`forge:adapter\b`),
			regexp.MustCompile(`forgeconv-adapter-no-rpc`),
			// The deleted Go API. Case-SENSITIVE, \b-anchored identifiers; the
			// live successors are spelled HasOutboundIODirective /
			// InternalPackageOutboundIO / RuleOutboundIONoRPC and share no
			// substring with these.
			regexp.MustCompile(`\bHasAdapterDirective\b|\bInternalPackageAdapter\b|\bRuleAdapterNoRPC\b`),
			regexp.MustCompile(`\blintAdapterNoRPC\b|\blintAdapterPkg\b|\bhasAdapterMarker\b`),
		},
	},
	{
		Name: "the generated per-API MCP manifest and its bridge",
		Why: "forge generated gen/mcp/manifest.json — one Model Context Protocol tool per " +
			"Connect RPC — plus a stdio bridge (internal/mcpbridge) and two hosts for it " +
			"(`forge mcp serve` and the cmd/forge-mcp binary) whose only job was serving that " +
			"manifest. An agent can drive the project's own CLI and `forge api curl` directly, " +
			"so the manifest bought a second, generated description of the API that had to be " +
			"kept true to the protos forever. The RPC inventory an agent actually needs is " +
			"still in `forge project audit --json` (shape.services[].rpcs, with streaming " +
			"mode) and `forge project map`/`graph`.\n" +
			"This removal does NOT touch MCP as a CLIENT: .mcp.json / .mcp.json.example and " +
			"their templates configure chrome-devtools and reliant-docs MCP servers that " +
			"agents consume, and those stay.",
		Patterns: []*regexp.Regexp{
			// SCOPE — read before widening. This entry forbids the generated
			// MANIFEST, the bridge package, and the hosts. It deliberately does
			// NOT police the bare word "MCP", which forge keeps ON PURPOSE for
			// the chrome-devtools / reliant-docs client config, the `MCP` Go
			// initialism in internal/naming, and internal packages a user may
			// legitimately name `mcp/...`.
			//
			// The manifest path. Slash-separated, so a project's own
			// internal/mcp/database package cannot reach it.
			regexp.MustCompile(`gen/mcp/manifest\.json`),
			// The emitter's Go API and its call site in the pipeline.
			regexp.MustCompile(`\bGenerateMCPManifest\b|\bMCPGenInput\b|\bstepMCPManifest\b`),
			// The bridge package and the standalone binary. \b-anchored:
			// "forge-mcp" and "mcpbridge" share no substring with the live
			// chrome-devtools config.
			regexp.MustCompile(`\bmcpbridge\b|\bforge-mcp\b`),
			// The CLI host: the `forge mcp` command and its token env var.
			// The space is what keeps this off ".mcp.json" and "internal/mcp/".
			regexp.MustCompile(`\bforge mcp\b|\bFORGE_MCP_TOKEN\b|\bnewMCPCmd\b|\bnewMCPServeCmd\b`),
			// The audit field that existed only to tell an agent what the
			// bridge could dispatch. Streaming mode survives it.
			regexp.MustCompile(`\bmcp_callable\b|\bMCPCallable\b`),
		},
	},
}

// packOnDisk implements the "a referenced pack must exist" rule for the packs
// removal. name is empty for an unnamed reference ("a pack"), which resolves
// against the pack root itself.
func packOnDisk(root, name string) (bool, []string) {
	looked := filepath.Join(root, "internal", "packs")
	if name != "" {
		looked = filepath.Join(looked, name)
	}
	fi, err := os.Stat(looked)
	rel, _ := filepath.Rel(root, looked)
	return err == nil && fi.IsDir(), []string{filepath.ToSlash(rel)}
}

// kubernetesNoun matches vocabulary that only appears when the subject is
// Kubernetes itself. It scopes the bare-"RBAC" allowance: an application-level
// RBAC claim never shares a line with a kubeconfig, a ClusterRole or a KCL
// deploy path.
var kubernetesNoun = regexp.MustCompile(`(?i)` + strings.Join([]string{
	`\bk8s\b`, `kubernetes`, `kubectl`, `kubeconfig`, `kubebuilder`,
	`clusterrole`, `rolebinding`, `serviceaccount`, `\bcrd\b`,
	`cluster[- _]scoped`, `cluster[- _]rbac`, `namespaced`,
	`\bkcl\b`, `\bmanifests?\b`, `\bdeployments?\b`, `\bhelm\b`,
}, `|`))

// commonAllowances apply to every removal in the table.
var commonAllowances = []allowance{
	{
		Name:   "the guard's own source",
		Reason: "This package necessarily spells out every forbidden pattern; matching itself would make the guard permanently red.",
		Paths:  []string{"internal/removalguard/"},
	},
}

// ─────────────────────────────────────────────────────────────────────────────
// Types
// ─────────────────────────────────────────────────────────────────────────────

// removal is one feature that has been deleted from forge.
type removal struct {
	// Name identifies the removal in failure output.
	Name string
	// Why records what was removed and what replaced it, so a future reader
	// who trips the guard can tell a straggler from a legitimate look-alike.
	Why string
	// Patterns are the spellings that constitute a reference to the feature.
	Patterns []*regexp.Regexp
	// Allowances excuse legitimate look-alikes. See allowance.
	Allowances []allowance

	// Resolve, when set, makes the Patterns CONDITIONAL: a match is a
	// straggler only when the artifact it names is missing from disk. Submatch
	// 1 of the matching pattern, when the pattern has one, is the name; it is
	// empty for an unnamed reference. It returns whether the artifact exists
	// and where it looked, which goes into the failure message.
	//
	// This is how to express "a referenced X must exist" instead of a literal
	// list of dead X names: adding or deleting an X later needs no edit here,
	// and a FUTURE deletion is caught the moment it lands.
	Resolve func(root, name string) (exists bool, searched []string)
}

// allowance excuses text that a removal's Patterns match but that is not a
// reference to the removed feature.
type allowance struct {
	// Name and Reason appear in nothing but the source; they exist so the
	// next reader can judge whether the carve-out is still earned.
	Name   string
	Reason string

	// Token scopes the allowance to the exact substrings it matches: a finding
	// is excused only when its matched span sits INSIDE a Token match on the
	// same line. A nil Token excuses the whole file and REQUIRES Paths.
	Token *regexp.Regexp

	// Context, when set, limits the allowance to lines that also match it. It
	// is how an ambiguous word is disambiguated by its neighbours rather than
	// by blanket-excusing the word.
	Context *regexp.Regexp

	// Paths limits the allowance to repo-relative, slash-separated paths. An
	// entry ending in "/" covers that directory and everything under it; a
	// leading "**/" matches at any depth; otherwise it is matched with
	// path.Match. Empty means every file.
	Paths []string
}

type finding struct {
	feature string
	path    string
	line    int
	pattern string
	text    string
	snippet string
	// note carries a Resolve verdict ("no such pack; looked in …").
	note string
}

// ─────────────────────────────────────────────────────────────────────────────
// Scan surface
// ─────────────────────────────────────────────────────────────────────────────

// skipDirs are never scanned. Everything excluded here is either not source
// (build output, dependency trees, per-developer runtime state) or would drown
// the guard in generated noise. Note what is NOT here: skills, docs, kcl,
// proto, internal/templates and dotfiles are all scanned, because that is
// exactly where the misses happened.
var skipDirs = map[string]bool{
	".git":         true, // VCS internals
	"node_modules": true, // npm dependency tree (vendored third-party code)
	"vendor":       true, // Go vendor tree (vendored third-party code)
	"dist":         true, // frontend build output
	"bin":          true, // compiled binaries
	".next":        true, // Next.js build cache
	".turbo":       true, // turborepo cache
	".vercel":      true, // Vercel build state
	"coverage":     true, // coverage report output
	"tmp":          true, // scratch output
	".forge":       true, // per-developer forge runtime state (gitignored)
}

// skipExts are binary formats. Matching bytes inside them would be noise, and
// reading them wastes the scan.
var skipExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true,
	".webp": true, ".pdf": true, ".zip": true, ".gz": true, ".tgz": true,
	".tar": true, ".wasm": true, ".woff": true, ".woff2": true, ".ttf": true,
	".otf": true, ".eot": true, ".mp4": true, ".mov": true, ".jar": true,
	".so": true, ".dylib": true, ".dll": true, ".exe": true, ".test": true,
}

// skipFiles are machine-generated dependency manifests: their contents are the
// names and hashes of third-party modules, which forge does not control.
var skipFiles = map[string]bool{
	"go.sum":            true,
	"go.work.sum":       true,
	"package-lock.json": true,
	"pnpm-lock.yaml":    true,
	"yarn.lock":         true,
	"kcl.mod.lock":      true,
}

// maxFileSize caps a single scanned file. Anything larger is generated data,
// not a surface a human wrote a feature reference into.
const maxFileSize = 4 << 20

// ─────────────────────────────────────────────────────────────────────────────
// The guard
// ─────────────────────────────────────────────────────────────────────────────

func TestRemovedFeaturesLeaveNoReferences(t *testing.T) {
	root := repoRoot(t)

	// Match each file against every removal on a worker pool.
	//
	// This is the expensive half of the guard — thousands of files times 49
	// removals times their patterns, times every line. Serially it ran ~31s
	// here, and under `-race` (how CI runs the suite: `go test -race -count=1
	// ./...` with no -timeout, so Go's 10-minute default applies) it blew
	// past 600s and PANICKED. A timeout panic fails the whole `Test` job and
	// masks every other package's result behind what reads as a hang, which
	// is how this went unexamined while the job stayed red.
	//
	// Findings are collected per file and merged in sorted file order after
	// the pool drains, so the output is byte-identical to the serial version
	// — the failure message diffs against golden expectations and must not
	// reorder.
	type fileHits struct {
		idx       int
		byFeature map[string][]finding
	}

	var scanned []struct {
		rel     string
		content []byte
	}
	forEachScannedFile(t, root, func(rel string, content []byte) {
		scanned = append(scanned, struct {
			rel     string
			content []byte
		}{rel, content})
	})

	// Precompute each removal's allowance slice once rather than rebuilding
	// it per file — it is identical for every file and the append pair
	// allocated twice per file per removal.
	allowedFor := make([][]allowance, len(removals))
	for ri, rm := range removals {
		allowedFor[ri] = append(append([]allowance{}, commonAllowances...), rm.Allowances...)
	}

	hitsCh := make(chan fileHits, len(scanned))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for idx, f := range scanned {
		wg.Add(1)
		go func(idx int, rel string, content []byte) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			local := map[string][]finding{}
			lines := strings.Split(string(content), "\n")
			for ri, rm := range removals {
				for i, line := range lines {
					for _, hit := range matchLine(root, line, rm, allowedFor[ri], rel) {
						hit.feature, hit.path, hit.line = rm.Name, rel, i+1
						hit.snippet = strings.TrimSpace(line)
						local[rm.Name] = append(local[rm.Name], hit)
					}
				}
			}
			if len(local) > 0 {
				hitsCh <- fileHits{idx: idx, byFeature: local}
			}
		}(idx, f.rel, f.content)
	}
	wg.Wait()
	close(hitsCh)

	perFile := make([]map[string][]finding, len(scanned))
	for fh := range hitsCh {
		perFile[fh.idx] = fh.byFeature
	}

	byFeature := map[string][]finding{}
	for _, m := range perFile {
		for name, hits := range m {
			byFeature[name] = append(byFeature[name], hits...)
		}
	}

	for _, rm := range removals {
		hits := byFeature[rm.Name]
		if len(hits) == 0 {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d surviving reference(s) to the removed %q feature.\n", len(hits), rm.Name)
		fmt.Fprintf(&b, "\n  %s\n", rm.Why)
		b.WriteString("\nSurviving references:\n")
		for _, h := range hits {
			fmt.Fprintf(&b, "  %s:%d: matched %q via pattern `%s`\n", h.path, h.line, h.text, h.pattern)
			if h.note != "" {
				fmt.Fprintf(&b, "      %s\n", h.note)
			}
			fmt.Fprintf(&b, "      | %s\n", truncate(h.snippet, 160))
		}
		b.WriteString("\nDelete the reference. If it is a legitimate look-alike and not a\n")
		b.WriteString("straggler, add a narrow allowance (with its reason) to the\n")
		b.WriteString(`"` + rm.Name + `" entry in internal/removalguard/removalguard_test.go.` + "\n")
		b.WriteString("Do NOT widen a pattern to make this green.\n")
		t.Errorf("%s", b.String())
	}
}

// matchLine returns every pattern hit on line that no allowance excuses and,
// when the removal has a Resolve, that names something missing from disk.
func matchLine(root, line string, rm removal, allowances []allowance, rel string) []finding {
	var out []finding
	for _, p := range rm.Patterns {
		for _, m := range p.FindAllStringSubmatchIndex(line, -1) {
			span := m[0:2]
			if excused(line, span, allowances, rel) {
				continue
			}
			hit := finding{pattern: p.String(), text: line[span[0]:span[1]]}
			if rm.Resolve != nil {
				name := ""
				if len(m) >= 4 && m[2] >= 0 {
					name = line[m[2]:m[3]]
				}
				exists, searched := rm.Resolve(root, name)
				if exists {
					continue
				}
				hit.note = fmt.Sprintf("→ names %q, which does not exist (looked in %s)",
					name, strings.Join(searched, ", "))
			}
			out = append(out, hit)
		}
	}
	return out
}

// excused reports whether span on line is covered by an allowance that applies
// to rel. A Token-scoped allowance excuses the span only when the span sits
// entirely inside one of the Token's matches on the same line — so excusing
// `rbac.authorization.k8s.io` never excuses a `RequireRole` sharing the line.
func excused(line string, span []int, allowances []allowance, rel string) bool {
	for _, a := range allowances {
		if len(a.Paths) > 0 && !pathCovered(rel, a.Paths) {
			continue
		}
		if a.Context != nil && !a.Context.MatchString(line) {
			continue
		}
		if a.Token == nil {
			return true // file-wide allowance
		}
		for _, ok := range a.Token.FindAllStringIndex(line, -1) {
			if ok[0] <= span[0] && span[1] <= ok[1] {
				return true
			}
		}
	}
	return false
}

// pathCovered matches rel (repo-relative, slash-separated) against the
// allowance patterns: a trailing "/" is a directory prefix, a leading "**/"
// matches at any depth, anything else goes through path.Match.
func pathCovered(rel string, patterns []string) bool {
	for _, p := range patterns {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(rel, p) {
				return true
			}
			continue
		}
		if tail, ok := strings.CutPrefix(p, "**/"); ok {
			for _, suffix := range pathSuffixes(rel) {
				if match, _ := path.Match(tail, suffix); match {
					return true
				}
			}
			continue
		}
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
	}
	return false
}

// pathSuffixes yields rel and every sub-path of it starting at a "/" boundary,
// so a "**/"-anchored pattern can match at any depth.
func pathSuffixes(rel string) []string {
	out := []string{rel}
	for i, c := range rel {
		if c == '/' {
			out = append(out, rel[i+1:])
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────────
// The scan surface
// ─────────────────────────────────────────────────────────────────────────────

// TestScanSurfaceReachesEveryForgeSurface makes "we grepped the Go tree and
// declared victory" structurally impossible. Every removal so far was declared
// done while a non-Go surface survived; if a future skipDirs/skipExts edit
// silently drops one of these, this fails instead of the guard quietly
// scanning less.
func TestScanSurfaceReachesEveryForgeSurface(t *testing.T) {
	surfaces := map[string]struct {
		why   string
		match func(rel string) bool
	}{
		"skills that ship into downstream projects": {
			"a stale skill taught agents an API that does not compile",
			func(r string) bool {
				return strings.HasPrefix(r, "internal/templates/project/skills/") && strings.HasSuffix(r, ".md")
			},
		},
		"template TypeScript": {
			"three frontend templates kept minting a bearer token no backend honors",
			func(r string) bool {
				return strings.HasPrefix(r, "internal/templates/") && strings.HasSuffix(r, ".ts")
			},
		},
		"template text/template sources": {
			"scaffolded Go/YAML lives in .tmpl, invisible to a Go-only grep",
			func(r string) bool { return strings.HasSuffix(r, ".tmpl") },
		},
		"docs": {"docs outlive the code they describe", func(r string) bool {
			return strings.HasPrefix(r, "docs/") && strings.HasSuffix(r, ".md")
		}},
		"kcl": {"KCL examples kept a removed field alive for multiple rounds", func(r string) bool {
			return strings.HasSuffix(r, ".k")
		}},
		"proto": {"proto is the API contract; a dead field there is a shipped dead field", func(r string) bool {
			return strings.HasSuffix(r, ".proto")
		}},
		"dotfiles": {"a .gitignore breadcrumb survived a removal", func(r string) bool {
			return strings.HasPrefix(path.Base(r), ".")
		}},
		"Go": {"the one surface that never gets missed — assert it anyway", func(r string) bool {
			return strings.HasSuffix(r, ".go")
		}},
	}

	counts := map[string]int{}
	total := 0
	forEachScannedFile(t, repoRoot(t), func(rel string, _ []byte) {
		total++
		for name, s := range surfaces {
			if s.match(rel) {
				counts[name]++
			}
		}
	})

	if total < 500 {
		t.Fatalf("the guard scanned only %d files — the repo root or the skip lists are wrong; "+
			"a guard that scans nothing passes everything", total)
	}
	for name, s := range surfaces {
		if counts[name] == 0 {
			t.Errorf("the %s surface is not being scanned (%s)", name, s.why)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// The look-alikes the guard must NEVER kill
// ─────────────────────────────────────────────────────────────────────────────

// TestLegitimateLookalikesAreStillPresent asserts that each thing the guard
// must NOT kill — whether an allowance protects it or a pattern's spelling
// steers around it — is really in the tree. Without this, the carve-outs rot
// into unfalsifiable config: someone broadens a pattern, this test goes red
// naming the exact construct they broke, instead of the guard quietly
// "passing" after the construct was deleted.
func TestLegitimateLookalikesAreStillPresent(t *testing.T) {
	// Each entry is a construct forge legitimately keeps. The guard above must
	// stay green while these are present.
	lookalikes := []struct {
		what  string
		why   string
		token *regexp.Regexp
	}{
		{`"Authorization" header`, "authentication — forge still reads Bearer tokens off it", regexp.MustCompile(`"Authorization"`)},
		{"http.StatusUnauthorized", "HTTP 401", regexp.MustCompile(`StatusUnauthorized`)},
		{"network:unauthorized", "frontend 401 event", regexp.MustCompile(`network:unauthorized`)},
		{"rbac.authorization.k8s.io", "Kubernetes ClusterRole/RoleBinding apiVersion", regexp.MustCompile(`rbac\.authorization\.k8s\.io`)},
		{"+kubebuilder:rbac", "controller-gen RBAC marker on generated controllers", regexp.MustCompile(`\+kubebuilder:rbac`)},
		{"cluster_rbac", "KCL field granting an operator cluster-scoped API access", regexp.MustCompile(`cluster_rbac`)},
		{"ClusterRBAC", "the KCL schema behind cluster_rbac", regexp.MustCompile(`ClusterRBAC`)},
		{"RBACSpec", "Go-side KCL render input for RBAC manifests", regexp.MustCompile(`RBACSpec`)},
		{"namespaced_rbac", "KCL field granting a Service namespaced API access", regexp.MustCompile(`namespaced_rbac`)},
		{"rbac_lib", "the KCL module that renders the RBAC manifests", regexp.MustCompile(`rbac_lib`)},
		{"crud.Pack", "the live response-projection seam in forge/pkg/crud — not the retired pack subsystem", regexp.MustCompile(`\bPack:\s+func\(`)},
		{"the English verb \"packs\"", "\"the read path never packs it\" — prose the packs patterns must not reach", regexp.MustCompile(`never packs `)},
		{"svcerr.PermissionDenied", "Connect wire code an application returns from its own policy check", regexp.MustCompile(`svcerr\.PermissionDenied`)},
		{"connect.CodePermissionDenied", "the Connect status code it maps to", regexp.MustCompile(`CodePermissionDenied`)},
		{"deploy/kcl/workloads.k", "the LIVE, tracked, user-owned workload declaration KCL expands into k8s resources — not the retired root manifest; the extension and the directory are what separate them", regexp.MustCompile(`deploy/kcl/workloads\.k`)},
		{"WorkloadsKCLRelPath", "the const naming that scaffolded file", regexp.MustCompile(`WorkloadsKCLRelPath`)},
		{"WorkloadStanza", "the formatter that writes one workload into it, shared by the scaffold and the drift lint", regexp.MustCompile(`WorkloadStanza`)},
		{"forge.workloads.Port", "the LIVE KCL schema a project declares a real port on — the home a port moved TO, not the Go carrier it moved off", regexp.MustCompile(`\bfw\.Port\b`)},
		{"config.DefaultServePort", "the one port fact forge itself knows: the single mux every service in the binary mounts onto", regexp.MustCompile(`DefaultServePort`)},
		{"HostDeploy.listen_ports", "the host TCP ports a dev-mode service binds — a KCL deploy fact, unrelated to the removed per-component carrier", regexp.MustCompile(`listen_ports`)},
		{"K8sCluster.ports", "the k8s Service/container port list on a cluster deploy block", regexp.MustCompile(`ports\?: \[int\]|Ports\s+\[\]int`)},
		{"forge cluster up", "the LIVE k3d-lifecycle verb — a `forge up` pattern widened to drop the word between `forge` and `up` swallows it", regexp.MustCompile(`forge cluster up`)},
		{"forge run --env", "the LIVE flag on a command where the environment really is an optional modifier — the counter-example that keeps the `--env` pattern anchored on up/down", regexp.MustCompile(`forge run --env`)},
		{"forge env deploy", "the LIVE spelling the env-noun verbs moved TO — a root-verb pattern widened to ignore what sits between `forge` and the verb swallows it", regexp.MustCompile(`forge env deploy`)},
		{`the English "forge dev" adjective`, "\"every forge dev namespace\", \"the forge dev loop\", \"a forge dev capability\" — prose the `forge dev` pattern must not reach, which is why that pattern requires a subcommand after it. Matched as a family rather than one fixed sentence: any single phrasing can legitimately leave the tree with the file that held it (\"a forge dev server\" did), and the assertion worth keeping is that the adjective still has SOME live use the pattern spares", regexp.MustCompile(`(?i)\bforge dev (?:server|namespace|loop|capability|controller)\b`)},
		{"`--type adapter`", "the LIVE scaffold flag value — the marker was renamed, the verb was NOT; a `forge:adapter` pattern widened to drop the `forge:` prefix swallows it", regexp.MustCompile(`--type[= ]adapter\b`)},
		{"the `adapter` skill", "forge still ships it — \"adapter\" is the design pattern, `outbound-io` is only the invariant a linter checks", regexp.MustCompile(`forge skill load adapter`)},
		{"`// forge:outbound-io`", "the LIVE marker the adapter scaffold stamps — the spelling `forge:adapter` was renamed TO", regexp.MustCompile(`forge:outbound-io`)},
		{"HasOutboundIODirective", "the live reader that replaced HasAdapterDirective", regexp.MustCompile(`\bHasOutboundIODirective\b`)},
		{"`forgeconv-deps-are-interfaces`", "the LIVE rule id. The retired spelling carried an `interactor-` infix; a pattern widened to drop it deletes the rule that catches `Deps: *db.PostgresRepository`", regexp.MustCompile(`forgeconv-deps-are-interfaces`)},
		{"the design-pattern noun \"interactor\"", "the `interactor` skill, and `forge scaffold package`'s help explaining why an orchestrator needs no flag, both keep the word — only the MARKER, the FLAG VALUE and the template tree went away", regexp.MustCompile(`(?i)\binteractors?\b`)},
	}

	root := repoRoot(t)
	seen := make([]int, len(lookalikes))
	forEachScannedFile(t, root, func(rel string, content []byte) {
		if strings.HasPrefix(rel, "internal/removalguard/") {
			return // this file names them all; that proves nothing
		}
		for i, l := range lookalikes {
			seen[i] += len(l.token.FindAllIndex(content, -1))
		}
	})

	for i, l := range lookalikes {
		if seen[i] == 0 {
			t.Errorf("%s has vanished from the repo (%s).\n"+
				"Either it was deleted — which is a real regression, not a lint fix — or a\n"+
				"removalguard pattern was widened until it swallowed it. Do not \"fix\" this\n"+
				"by deleting the allowance.", l.what, l.why)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Plumbing
// ─────────────────────────────────────────────────────────────────────────────

// repoRoot finds the repository root from this test's own compiled-in source
// path, so it is correct under `go test ./...` from any directory and from any
// build cache location. It falls back to walking up from the working directory
// when the source tree has moved.
func repoRoot(t *testing.T) string {
	t.Helper()
	if _, self, _, ok := runtime.Caller(0); ok {
		if root, err := ascendToRoot(filepath.Dir(self)); err == nil {
			return root
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}
	root, err := ascendToRoot(wd)
	if err != nil {
		t.Fatalf("locate repo root from %s: %v", wd, err)
	}
	return root
}

// ascendToRoot walks up from dir to the directory holding forge's go.mod.
func ascendToRoot(dir string) (string, error) {
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.Contains(b, []byte("module github.com/reliant-labs/forge\n")) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod declaring module github.com/reliant-labs/forge found above the search start")
		}
		dir = parent
	}
}

// forEachScannedFile walks the whole repository and calls fn with each
// scannable file's repo-relative slash path and contents, in a stable order.
func forEachScannedFile(t *testing.T, root string, fn func(rel string, content []byte)) {
	t.Helper()

	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if skipFiles[d.Name()] || skipExts[strings.ToLower(filepath.Ext(p))] {
			return nil
		}
		if info, statErr := d.Info(); statErr == nil && info.Size() > maxFileSize {
			return nil
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(files)

	// Read the files concurrently. The scan is thousands of files against
	// every removal pattern, and under `-race` (which is how CI runs the
	// suite) the serial version took the package past `go test`'s 10-minute
	// default and panicked the whole job — masking every other package's
	// result behind a timeout that looked like a hang.
	//
	// Only the READ is parallel. fn is still invoked serially, in sorted
	// order, on the calling goroutine: the callers accumulate into shared
	// maps and their output is diffed against golden expectations, so
	// concurrent calls would both race and reorder findings.
	type readResult struct {
		rel     string
		content []byte
		err     error
	}
	results := make([]readResult, len(files))
	sem := make(chan struct{}, runtime.GOMAXPROCS(0))
	var wg sync.WaitGroup
	for i, rel := range files {
		wg.Add(1)
		go func(i int, rel string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			results[i] = readResult{rel: rel, content: content, err: err}
		}(i, rel)
	}
	wg.Wait()

	for _, res := range results {
		rel, content := res.rel, res.content
		if res.err != nil {
			t.Fatalf("read %s: %v", rel, res.err)
		}
		if isBinary(content) {
			continue
		}
		fn(rel, content)
	}
}

// isBinary reports whether content looks like a binary file. A NUL byte in the
// first 8 KiB is the same heuristic git uses.
func isBinary(content []byte) bool {
	head := content
	if len(head) > 8192 {
		head = head[:8192]
	}
	return bytes.IndexByte(head, 0) >= 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
