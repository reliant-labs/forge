// Package config defines the canonical forge.yaml types shared by
// both the CLI (read) and the generator (write) packages. The
// forge.yaml schema deliberately exposes ~40 typed sections so YAML
// unmarshal can hydrate each block; splitting this file would just
// scatter the same surface across multiple packages and break the
// "one schema, one file" contract the generator relies on. The
// max-public-structs revive rule is therefore suppressed at the
// package-doc line below.

// forge:exclude-contract
//
// internal/config is a pure schema/data/constants package: it declares the
// canonical forge.yaml types plus package-level lookup tables (e.g.
// ExperimentalFeatureNames — the stable display order shared by `forge project audit`,
// the startup warning, and `forge project features`). The directive opts it
// out of the contract lint rules — specifically the exported-vars rule, which
// would otherwise demand ExperimentalFeatureNames become a getter. A getter is
// the ideal fix but its call sites reach into internal/cli (an ordered slice
// spread as `config.ExperimentalFeatureNames...`); converting it there is a
// cross-package change outside this pass. This is the accepted suppression for
// a genuine data/catalogue package.
//
// The directive is a FULL opt-out, not a lint-only one: it also stops
// contract codegen, so the narrow Service seam in contract.go gets no
// generated mock. That is the intended trade here — nothing mocks
// config — but it is the whole cost, so weigh it before adding the
// directive to a package whose mock anyone compiles against.
//
//nolint:revive // max-public-structs: see package doc above.
package config

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/linter/suppress"
)

// ProjectKind identifies the shape of a forge project. The default,
// "service", produces a Connect-RPC service scaffold (handlers,
// middleware, deploy manifests). "cli" produces a Cobra-based CLI
// binary with no server-shaped scaffolding. "library" produces a
// pure Go module with no cmd/ entry point.
const (
	ProjectKindService = "service"
	ProjectKindCLI     = "cli"
	ProjectKindLibrary = "library"
)

// ProjectBinary describes the binary packaging shape for a service
// project. "per-service" (the default) emits the canonical layout:
// one `cmd/server.go` cobra root with `server [services...]` filtering
// at the runtime layer, and one Application per service in deploy/
// KCL. "shared" emits one cobra subcommand per service so callers can
// invoke `./project <svc>` directly, and KCL emits a single
// MultiServiceApplication (one image, N Deployments) instead of N
// Applications. See FORGE_BACKLOG.md "Layer B" for tradeoffs.
const (
	ProjectBinaryPerService = "per-service"
	ProjectBinaryShared     = "shared"
)

// EffectiveProjectKind returns the project kind, defaulting to
// "service" so that older forge.yaml files without a kind: field
// continue to behave as service projects.
func EffectiveProjectKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case ProjectKindCLI:
		return ProjectKindCLI
	case ProjectKindLibrary:
		return ProjectKindLibrary
	default:
		return ProjectKindService
	}
}

// EffectiveProjectBinary returns the binary mode, defaulting to
// "per-service" so projects predating the field keep their existing
// codegen shape.
func EffectiveProjectBinary(binary string) string {
	switch strings.ToLower(strings.TrimSpace(binary)) {
	case ProjectBinaryShared:
		return ProjectBinaryShared
	default:
		return ProjectBinaryPerService
	}
}

// ProjectConfig represents the forge.yaml file.
// Fields align with proto/forge/project/v1/project.proto.
type ProjectConfig struct {
	Name       string `yaml:"name"`
	ModulePath string `yaml:"module_path"`
	// Kind is the project shape (service|cli|library). It is not a forge.yaml
	// field: the loader DERIVES it from the project's real sources — the KCL
	// deploy tree, the pkg/app composition root, internal/handlers/, the
	// service protos, and cmd/<name>/main.go (see deriveProjectKindFromSources).
	// The yaml tag is "-" so a stale `kind:` in forge.yaml is reported with a
	// migration hint instead of being honored.
	Kind   string `yaml:"-"`
	Binary string `yaml:"binary,omitempty"` // "per-service" (default), "shared" — one Go binary, cobra subcommand per service
	// ForgeVersion records the forge binary version that this project's
	// generated artifacts were last produced against. It is set at
	// `forge project new` time, bumped after a successful `forge project upgrade`, and
	// consulted by `forge generate` to warn when the forge binary on
	// PATH has drifted from the version pinned by the project. Empty
	// (legacy) projects are treated as "0.0.0".
	ForgeVersion string `yaml:"forge_version,omitempty"`
	// Harness records which AI harness this project was scaffolded for
	// (`forge project new --harness`). It is the ONLY durable record of that
	// choice: the flag is read once at scaffold time, and without it on disk
	// every later `forge generate` has to guess. The skills emitter reads it
	// to decide whether to deliver on-disk SKILL.md files at all — `claude`
	// gets .claude/skills/, and the default `reliant` (whose CLI discovers
	// forge's skills through this same forge.yaml, and whose `forge skill
	// load <name>` prints them from the binary) gets nothing written.
	//
	// Empty means UNSET, not reliant. The two are deliberately distinguished:
	// a project scaffolded before this field existed may hold delivered
	// skills that its harness depends on, so absence is read as "leave what
	// is already there alone" rather than as the default. See
	// harnessSkillsDirFor in internal/cli/generate_skills.go.
	Harness string `yaml:"harness,omitempty"`
	// NOTE: there is intentionally NO project-level `hot_reload:` field.
	// The single hot-reload switch is `features.hot_reload`, which the dev
	// loop and the air-config codegen actually read; a second top-level key
	// spelled the same way resolved through a different accessor that no
	// caller ever invoked. A `hot_reload:` key at the top level is reported
	// with a migration hint (see removedSchemaKeys).
	//
	// NOTE: there is intentionally NO `components:` field, in yaml or in
	// memory. What a project builds and runs — Connect servers, workers,
	// crons, operators, secondary binaries — is declared by the code itself
	// (the proto descriptor, internal/workers/, internal/operators/, cmd/),
	// and a config object carrying a copy would be a second source of truth
	// that is empty exactly when nobody remembered to fill it. A caller that
	// needs the component inventory asks the project for it:
	// codegen.DiscoverProjectComponents. A `components:` key in forge.yaml is
	// reported with a migration hint (see removedSchemaKeys).
	//
	// NOTE: there is intentionally NO `packages:` field either. An internal
	// package is declared by internal/<name>/contract.go — the file the
	// bootstrap/injector codegen has always walked for — and its
	// outbound-boundary claim by the `//forge:outbound-io` marker in its own
	// source. The retired list was never a codegen input: deleting it left
	// every package still imported, constructed and middleware-wrapped in
	// internal/app/compose.go. It steered only the REPORTERS, so a stale
	// entry made `forge project map` / `forge project audit` / the
	// architecture doc name a package that does not exist while hiding one
	// that does. They now resolve through codegen.DiscoverInternalPackages.
	// A `packages:` key is reported with a migration hint (see
	// removedSchemaKeys).
	Frontends []FrontendConfig `yaml:"frontends,omitempty"`
	// Frontend holds project-level frontend settings — distinct from
	// the per-frontend `Frontends []FrontendConfig` slice above. Today
	// it only carries the opt-in `workspaces:` flag that turns on the
	// pnpm-workspace + packages/api + packages/hooks layout so multiple
	// frontends (web + mobile) can share generated Connect clients and
	// React Query hook wrappers. When the flag is false (the default)
	// forge keeps the historic per-frontend layout exactly as before.
	Frontend FrontendProjectConfig `yaml:"frontend,omitempty"`
	// The section blocks below are all omitempty: a freshly scaffolded
	// forge.yaml leaves them absent and the loader fills shape-derived
	// defaults (see ApplyDerivedDefaults in derive.go). A present block
	// is taken literally — write the block (or a single key) to override.
	Database DatabaseConfig `yaml:"database,omitempty"`
	CI       CIConfig       `yaml:"ci,omitempty"`
	// NOTE: there is intentionally NO `build:` field. Build is declared
	// per-service, per-env in KCL (the polymorphic Build union —
	// GoBuild | DockerBuild | ShellBuild on each forge.Service); forge.yaml
	// carries zero build config. A `build:` key in forge.yaml is REJECTED
	// as an unknown key by the strict loader (walkUnknownKeys) rather than
	// silently ignored — version stamping moves to a KCL GoBuild.ldflags
	// `-X` entry.
	Deploy    DeployConfig    `yaml:"deploy,omitempty"`
	Docker    DockerConfig    `yaml:"docker,omitempty"`
	K8s       K8sConfig       `yaml:"k8s,omitempty"`
	Lint      LintConfig      `yaml:"lint,omitempty"`
	Contracts ContractsConfig `yaml:"contracts,omitempty"`
	// Config steers humans and LLMs away from reading the environment
	// directly (os.Getenv / os.LookupEnv / os.Environ) and toward forge's
	// generated, dependency-injected typed config object. See
	// [ConfigGuardConfig] for the enforce_typed_access / loader_package
	// semantics and the absent-key default (warn).
	Config ConfigGuardConfig `yaml:"config,omitempty"`
	// NOTE: there is intentionally NO `auth:` block. Authentication is owned
	// code — internal/app/auth.go's SetupAuth picks the validator, and
	// issuer/audience/JWKS are per-deployment values that live in env vars
	// routed through KCL. An `auth:` key is reported with a migration hint
	// (see removedSchemaKeys).
	Docs     DocsConfig     `yaml:"docs,omitempty"`
	Features FeaturesConfig `yaml:"features,omitempty"`
	Stack    StackConfig    `yaml:"stack,omitempty"`
	// Observability seeds the OWNED per-package observe_chain.go seam that a
	// generated component decorator routes through. The values are read at
	// `forge scaffold` time to stamp the initial chain (log level, etc.);
	// the seam is user-owned thereafter. See [ObservabilityConfig].
	Observability ObservabilityConfig `yaml:"observability,omitempty"`
	// API toggles project-level API protocol skins layered on top of the
	// Connect mux. Default zero-value leaves both REST and OpenAPI off so
	// existing projects regenerate identically. See [APIConfig] for the
	// per-field semantics.
	API APIConfig `yaml:"api,omitempty"`
	// Smoke declares APP-FLOW health checks that `forge env smoke <env>` runs in
	// addition to its built-in ingress route / dev-port probes. A route probe
	// only proves listeners are up; a flow check proves the APP actually works
	// (an end-to-end invariant only the app can express). See [SmokeConfig].
	Smoke SmokeConfig `yaml:"smoke,omitempty"`
}

// Component kind constants. Kind is the single discriminator on a
// [ComponentConfig] — it replaces the old `services[].type` +
// `services[].kind` pair and the separate `binaries:` block.
//
//   - server   — Connect-RPC handlers + client + frontend
//     hooks + bootstrap row + cobra subcommand (was type=go_service).
//   - worker   — in-process ContextWorker goroutine; bootstrap Workers row.
//   - cron     — scheduled job; Schedule drives it. In-process scheduled
//     goroutine for dev, CronJob in deploy. First-class (was
//     worker + kind:cron).
//   - job      — ONE-SHOT: runs a command to completion, exits 0, and
//     gates the components named in Before. The portable "do this once,
//     then proceed" primitive (k8s init container / compose
//     service_completed_successfully / host run-and-wait).
//   - operator — controller-runtime manager + CRDs.
//   - binary   — standalone cobra subcommand cmd/<name>.go (one image,
//     run `./app <name>`); no bootstrap wiring (was the binaries: block).
const (
	ComponentKindServer   = "server"
	ComponentKindWorker   = "worker"
	ComponentKindCron     = "cron"
	ComponentKindJob      = "job"
	ComponentKindOperator = "operator"
	ComponentKindBinary   = "binary"
)

// ComponentConfig describes one buildable/runnable unit of a forge project
// — a Connect server, worker, cron, operator or secondary binary. Its Kind
// field selects which scaffold + deploy treatment the component receives;
// see the ComponentKind* constants.
//
// It carries NO struct tags, and that is load-bearing: a component is never
// serialized to or parsed from a config file. The declaration is the code
// (the proto descriptor, internal/workers/, internal/operators/, cmd/) and
// this type is only the in-memory shape codegen.DiscoverProjectComponents
// hands back after reading it. Per-environment facts (placement, replicas,
// exposed ports) are declared in `deploy/kcl/<env>/main.k`, not here.
type ComponentConfig struct {
	Name string
	// Kind is THE discriminator: server|worker|cron|operator|binary.
	// See the ComponentKind* constants.
	Kind          string
	Path          string
	Schedule      string // cron expression for kind=cron
	ProtoPackages []string
	// Group is the API group for kind=operator components. e.g.
	// "reliant.dev". Read from the operator package's APIGroup const.
	Group string
	// Version is the API version for kind=operator components. e.g.
	// "v1alpha1". Read from the operator package's APIVersion const.
	Version string
	// CRDs lists the CRDs reconciled by this operator — one per
	// operators/<operator>/<crd-name>_controller.go on disk.
	CRDs []CRDConfig
}

// DefaultServePort is the ONE port fact forge itself knows: the port a
// scaffolded binary listens on. Every service in a binary mounts onto the
// same Connect mux and the process listens exactly once, on AppConfig.port
// (env PORT, default 8080) — so the port belongs to the BINARY, never to an
// individual component.
//
// Any OTHER port is a deploy fact and lives in `deploy/kcl/<env>/main.k`
// (forge.workloads.Workload.ports, refined per environment). Nothing forge
// introspects — the proto descriptor, the owned worker/operator files, cmd/ —
// carries a port, so there is deliberately no per-component port field to read.
const DefaultServePort = 8080

// EffectiveKind returns the lowercased, trimmed kind, defaulting to
// "server" for empty input (a component with no kind is a Connect
// server — the historical `type: go_service` default).
func (c ComponentConfig) EffectiveKind() string {
	k := strings.ToLower(strings.TrimSpace(c.Kind))
	if k == "" {
		return ComponentKindServer
	}
	return k
}

// IsServer reports whether the component is a Connect-RPC server.
func (c ComponentConfig) IsServer() bool { return c.EffectiveKind() == ComponentKindServer }

// IsWorker reports whether the component is an in-process worker.
func (c ComponentConfig) IsWorker() bool { return c.EffectiveKind() == ComponentKindWorker }

// IsCron reports whether the component is a scheduled cron job.
func (c ComponentConfig) IsCron() bool { return c.EffectiveKind() == ComponentKindCron }

// IsJob reports whether the component is a one-shot job.
func (c ComponentConfig) IsJob() bool { return c.EffectiveKind() == ComponentKindJob }

// IsOperator reports whether the component is a controller-runtime operator.
func (c ComponentConfig) IsOperator() bool { return c.EffectiveKind() == ComponentKindOperator }

// IsBinary reports whether the component is a standalone binary subcommand.
func (c ComponentConfig) IsBinary() bool { return c.EffectiveKind() == ComponentKindBinary }

// CRDConfig represents a single Custom Resource Definition reconciled by an
// operator, discovered from the <crd-name>_controller.go shim that
// `forge scaffold crd <name> --operator <op>` writes. Untagged for the
// same reason as [ComponentConfig]: it is never serialized.
type CRDConfig struct {
	// Name is the PascalCase CRD type name. e.g. "Workspace".
	Name string
	// Group is the API group, defaulting to the parent operator's
	// Group. Carried explicitly so a single operator can manage CRDs
	// from multiple groups.
	Group string
	// Version is the API version. Defaults to the parent operator's
	// Version.
	Version string
	// Shape is the reconciler scaffold style. One of
	// "state-machine" (phase-driven), "config" (declarative-only,
	// no state), "composite" (manages sub-resources). Drives which
	// template is rendered for the controller shim.
	Shape string
}

// FrontendConfig defines a frontend application (e.g. Next.js, React Native).
type FrontendConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`           // "nextjs", "react-native", "vite-spa"
	Kind string `yaml:"kind,omitempty"` // "web" (default/Next.js), "mobile" (React Native), "vite-spa" (Vite + React + tanstack-router)
	Path string `yaml:"path"`
	// Source declares this frontend's code as "that repo at that ref"
	// instead of a directory that must already be on disk. Set it INSTEAD
	// of Path when the frontend lives in another repository:
	//
	//	frontends:
	//	  - name: reliant-web
	//	    type: vite-spa
	//	    source:
	//	      repo: github.com/reliant-labs/reliant
	//	      ref: v1.6.3
	//	      subdir: web
	//
	// forge fetches the pin into a machine-local cache and builds from
	// there, so the frontend builds in CI — where only this repository is
	// checked out — and builds the SAME bytes on every machine. A bare
	// `path` to a sibling checkout has neither property: it fails wherever
	// the sibling is absent, and where it is present it ships whatever
	// happened to be checked out.
	//
	// Path and Source are mutually exclusive; a frontend declaring both is
	// rejected at load. Local iteration against a working copy goes through
	// `.forge/source-overrides.yaml` (machine-local, gitignored) rather
	// than through editing the pin — see internal/gitsource.
	Source *GitSource `yaml:"source,omitempty"`
	// Port is the frontend's dev-server listen port. Omitted / 0 means
	// EPHEMERAL: `forge run` / `forge env up` allocate a free OS port at
	// launch and report it (see resolveEphemeralFrontendPorts). omitempty so
	// a scaffolded ephemeral frontend writes no `port:` line at all; an
	// explicit port is honored verbatim.
	Port int `yaml:"port,omitempty"`
	// Output selects the Next.js build/runtime shape for this frontend.
	// Only meaningful when Type == "nextjs"; ignored for react-native and
	// vite-spa (those have their own production shapes).
	//
	// Valid values:
	//   - "standalone" (default): production builds emit a self-contained
	//     Node server at `.next-prod/standalone/server.js`. This is the shape
	//     the shipped Dockerfile copies into its runner image, and the
	//     only default that supports the dynamic `[id]` CRUD detail/edit
	//     routes forge generates for every entity.
	//   - "static": production builds emit a static export
	//     (`output: "export"` gated on NODE_ENV=production) — pure HTML +
	//     JS + CSS the user can drop on a CDN or object store. The dev
	//     server stays unchanged (`next dev`). EXPLICIT OPT-IN ONLY:
	//     `output: "export"` requires generateStaticParams() on every
	//     dynamic route segment, and the generated CRUD detail/edit
	//     pages (`/<slug>/[id]`) are dynamic client routes whose ids
	//     only exist at runtime — `npm run build` fails on any project
	//     with a CRUD entity unless those pages are removed or given
	//     hand-written static params.
	//   - "server": full Next.js dev AND prod (no `output:` set). Use
	//     when you want `next start` semantics in prod for custom edge /
	//     ISR workflows.
	//
	// Defaults to "standalone" when empty. Pre-existing projects keep
	// their checked-in `next.config.ts`; the field doesn't
	// retroactively rewrite it.
	Output string `yaml:"output,omitempty"`
	// BasePath is the URL path prefix this frontend is mounted under
	// when it is NOT served from the host root — e.g. "/admin" for an
	// admin UI that a reverse proxy blends with another app on the same
	// host. Only meaningful when Type == "nextjs".
	//
	// Shape rules (validated by `forge validate` / LoadProject):
	//   - must start with "/"            ("/admin", "/internal/admin")
	//   - must not end with "/"          ("/admin/" is rejected)
	//   - must not be bare "/"           (root mount == leave it empty)
	//   - segments are limited to [A-Za-z0-9._-]
	//
	// What it drives:
	//   - next.config.ts: rendered as the build-time default for both
	//     `basePath` and `assetPrefix` (same value — assetPrefix is what
	//     keeps RSC/chunk URLs under the prefix so hydration works).
	//   - src/lib/basepath_gen.ts (Tier-1, regenerated every `forge
	//     generate`): exports BASE_PATH + joinBasePath() for URLs Next.js
	//     can't rewrite (window.location-built redirects, share links).
	//
	// The single runtime override is the NEXT_PUBLIC_BASE_PATH env var —
	// the ONLY base-path variable forge ever reads or writes. Empty
	// (the default) means the frontend is served from the host root.
	BasePath string `yaml:"base_path,omitempty"`
	// Routes restricts which entities this frontend gets generated CRUD
	// pages for. Entries are route SLUGS as they appear on disk and in the
	// URL — the plural, kebab-cased entity name ("llm-keys", "usage-events").
	//
	// Empty (the default) means EVERY CRUD entity in the project, which is
	// the right behavior for a project's single frontend and the wrong one
	// for every frontend after that. forge scaffolds a full list/detail/
	// create/edit route set per entity, so a purpose-built frontend — an
	// operator console that wants two of twenty entities — starts by deleting
	// most of what was just written, and re-deletes anything added later.
	//
	// Naming them here is an ALLOWLIST: a new entity added to the project
	// does not silently appear in this frontend. That is the safer default
	// for a frontend whose route set is a product decision (an internal
	// console must not grow a customer-facing page because someone added a
	// table), and it makes the intended surface readable in forge.yaml
	// instead of inferable only from which directories survived.
	//
	// Unknown slugs are reported by `forge generate` rather than ignored: a
	// typo'd or renamed entity would otherwise silently yield a frontend
	// missing the page its author asked for.
	Routes []string `yaml:"routes,omitempty"`
	// AuthMode picks how this frontend signs users IN. It does not change
	// what authentication means anywhere else: the backend validates the
	// same JWT either way, and forge still issues no tokens.
	//
	// "redirect" is the only value, and the default. `login()` sends the
	// browser to the IdP's own hosted pages, which is portable across
	// every IdP — and MFA, social sign-in and password reset come for
	// free because the provider's pages implement them.
	//
	// The field is kept (rather than removed) because a first-party
	// sign-in FORM is a legitimate thing to want, and a project that
	// builds one against its own IdP's API has somewhere to declare it.
	// forge does not scaffold one: driving a hosted sign-in from your own
	// form is provider-proprietary in every case — there is no standard
	// the way OIDC discovery generalizes the redirect flow — so a
	// scaffolded implementation is only ever right for one vendor.
	//
	// See the `auth/frontend` skill for what a first-party form has to
	// respect (in particular: the password goes to the IdP, never to your
	// forge backend, which mints no tokens and has no endpoint for one).
	AuthMode string `yaml:"auth_mode,omitempty"`
}

// Auth-mode values for FrontendConfig.AuthMode.
const (
	// AuthModeRedirect hands sign-in to the IdP's hosted pages.
	AuthModeRedirect = "redirect"
)

// EffectiveAuthMode returns the frontend's sign-in mode, defaulting to the
// portable redirect flow. Empty means unset, not "no auth" — every mode
// authenticates; they differ only in where the user types their password.
func (f FrontendConfig) EffectiveAuthMode() string {
	if f.AuthMode == "" {
		return AuthModeRedirect
	}
	return f.AuthMode
}

// GitSource declares a component's code as "that repository at that ref"
// rather than a directory that must already exist on disk.
//
// It is the schema half of internal/gitsource (which holds the resolver,
// the cache and the local-override rules); this type is the plain data
// forge.yaml and KCL both unmarshal into. It lives here rather than in
// gitsource so internal/config keeps its no-internal-imports shape — the
// resolver converts to its own Source type at the boundary.
//
// The point is reproducibility, not convenience. A filesystem path to a
// sibling checkout fails wherever the sibling is absent (CI), and where it
// IS present it silently ships whatever happened to be checked out, so two
// machines produce different artifacts from identical commits. A ref makes
// the cross-repo dependency an explicit, reviewable, promotable version —
// the same property go.mod already gives the Go half of the same
// dependency.
type GitSource struct {
	// Repo is the repository: host/owner/name
	// ("github.com/reliant-labs/reliant"), which forge expands to an https
	// clone URL, or an explicit https://, ssh://, or git@host:owner/name
	// URL, which is used verbatim.
	Repo string `yaml:"repo" json:"repo"`
	// Ref is the tag, branch, or commit sha to check out. Required —
	// forge does not default to a repository's default branch, because an
	// unpinned dependency is precisely the problem this type solves.
	Ref string `yaml:"ref" json:"ref"`
	// Subdir is the path within the repository holding this component's
	// source ("web"). Empty means the repository root.
	Subdir string `yaml:"subdir,omitempty" json:"subdir,omitempty"`
}

// EffectivePath returns the frontend's directory relative to the project
// root: the declared `path`, falling back to the conventional
// `frontends/<name>` layout when it is empty. Every command that shells
// into a frontend (build, generate, lint) needs this same fallback, so it
// lives on the config type rather than being re-derived per call site.
//
// A frontend with a `source:` has NO meaningful value here until the
// source is resolved — its code is not in the project tree. Callers that
// can shell into a frontend must consult HasGitSource first and route
// through the resolver; EffectivePath keeps returning the conventional
// fallback so the many callers that only need a label are unchanged.
func (f FrontendConfig) EffectivePath() string {
	if f.Path != "" {
		return f.Path
	}
	return filepath.Join("frontends", f.Name)
}

// HasGitSource reports whether this frontend's code comes from another
// repository rather than from a directory in the project tree.
func (f FrontendConfig) HasGitSource() bool {
	return f.Source != nil && f.Source.Repo != ""
}

// FrontendToolchainDisabled reports whether the project has opted OUT of
// forge driving a Node toolchain over its frontends, via
// `stack.frontend.framework: none`. That setting means the frontends build
// and check out-of-band (deps not installed under forge's control, a
// non-npm toolchain, a vendored bundle), so forge must not shell into them
// — not for `npm run build`, and not for a typecheck either. Frontends
// stay in Frontends for the commands that only need their paths
// (generate, dev-serve).
func (c *ProjectConfig) FrontendToolchainDisabled() bool {
	return c != nil && c.Stack.EffectiveFrontendFramework() == "none"
}

// ToolchainFrontends returns the frontends forge owns a Node toolchain for:
// every declared frontend with its path resolved, or nil when the project
// opted out via `stack.frontend.framework: none`. It is the single
// project-level answer to "which frontends may forge shell into, and
// where do they live" — shared by the build target resolution and the
// lint pipeline's frontend lane so the two can never disagree about the
// set. Command-specific filters (--target, KCL deploy mode, per-command
// skips) layer on top of this set; they do not re-derive it.
func (c *ProjectConfig) ToolchainFrontends() []FrontendConfig {
	if c == nil || c.FrontendToolchainDisabled() {
		return nil
	}
	out := make([]FrontendConfig, 0, len(c.Frontends))
	for _, fe := range c.Frontends {
		fe.Path = fe.EffectivePath()
		out = append(out, fe)
	}
	return out
}

// FrontendProjectConfig holds project-level frontend settings — fields
// that apply to the whole project rather than a single frontend entry.
// Distinct from FrontendConfig (per-frontend) and from the cli loader's
// "did the user pass --frontend" notion. Today the only field is
// Workspaces, the opt-in pnpm workspaces toggle.
//
// The flag is intentionally project-level (not per-frontend) because
// the workspace layout reshapes the whole project tree (packages/api,
// packages/hooks, pnpm-workspace.yaml at root), not just one frontend.
type FrontendProjectConfig struct {
	// Workspaces opts the project into the pnpm-workspaces layout. When
	// true:
	//
	//   - A `pnpm-workspace.yaml` is emitted at the project root listing
	//     `packages/*` and `frontends/*` as members.
	//   - `packages/api/` contains the buf-generated Connect TS clients
	//     and proto types as a single workspace package (`@<scope>/api`).
	//   - `packages/hooks/` contains the React Query wrappers
	//     (`use-api-query.ts` / `use-api-mutation.ts`) and the generated
	//     per-service hooks (`packages/hooks/src/generated/`), exposed as
	//     `@<scope>/hooks`.
	//   - Each frontend `package.json` declares the workspace deps via
	//     `"@<scope>/api": "workspace:*"` and imports them by package name
	//     rather than by relative path.
	//
	// When false (the default), forge emits the historic per-frontend
	// layout — `frontends/<name>/src/gen/` for buf output, hooks
	// templated into each `frontends/<name>/src/hooks/` — byte-identical
	// to projects scaffolded before this flag landed.
	Workspaces bool `yaml:"workspaces,omitempty"`
}

// IsFrontendWorkspacesEnabled reports whether the project opted in to
// the pnpm-workspaces layout. Wraps ProjectConfig.Frontend.Workspaces
// so callers can read the effective flag without poking into the nested
// struct (and so we have one place to enforce future invariants — e.g.
// requiring at least 2 frontends before enabling).
func (c ProjectConfig) IsFrontendWorkspacesEnabled() bool {
	return c.Frontend.Workspaces
}

// HasReactNativeFrontend reports whether any frontend in the project is
// a React Native (Expo) app. Used to gate features that only apply to
// mobile — e.g. the `@<scope>/ui-native` workspace package.
//
// Returns true for frontends declared with `type: react-native` (or the
// historic `type: react_native` underscore form the validator also
// accepts).
func (c ProjectConfig) HasReactNativeFrontend() bool {
	for _, fe := range c.Frontends {
		t := strings.ToLower(strings.TrimSpace(fe.Type))
		if t == "react-native" || t == "react_native" {
			return true
		}
	}
	return false
}

// DatabaseConfig holds database-related settings.
//
// The driver is pinned to postgres: forge generates postgres-only data
// layers (the runtime ORM, the generate-time schema introspection, and
// the test harness all target real postgres). The only meaningful choice
// is postgres vs "none" (no database).
type DatabaseConfig struct {
	Driver          string                `yaml:"driver"` // "postgres" or "none"
	MigrationsDir   string                `yaml:"migrations_dir"`
	MigrationSafety MigrationSafetyConfig `yaml:"migration_safety,omitempty"`
	Seed            SeedConfig            `yaml:"seed,omitempty"`
}

// SeedConfig controls the deterministic development seed data materialized at
// runtime by `forge db seed` and `forge run` auto-seed. Seeds are never
// written into the project as files, and these settings only ever reach a dev
// database: `forge db seed apply`/`reset` refuse any other environment, and
// the migrate path — the one thing that runs against production — has no seed
// code path at all. The planner itself is a library (pkg/seedplan) that tests
// may call directly against their own database.
type SeedConfig struct {
	// Rows is the default rows per table (default 20 — fills a page and
	// exercises pagination).
	Rows int `yaml:"rows,omitempty"`
	// Salt perturbs synthesis: change for a different-but-stable dataset.
	Salt int `yaml:"salt,omitempty"`
	// RowsPerTable overrides Rows for specific tables.
	RowsPerTable map[string]int `yaml:"rows_per_table,omitempty"`
	// Auto controls `forge run` first-boot auto-seed. Nil = on by default.
	Auto *bool `yaml:"auto,omitempty"`
}

// EffectiveRows returns the default rows-per-table (falls back to 20).
func (c SeedConfig) EffectiveRows() int {
	if c.Rows > 0 {
		return c.Rows
	}
	return 20
}

// AutoEnabled reports whether `forge run` first-boot auto-seed is on. Nil
// Auto means "on by default".
func (c SeedConfig) AutoEnabled() bool {
	return c.Auto == nil || *c.Auto
}

// MigrationSafetyConfig controls migrationlint's three severity dials
// (unsafe add-column, destructive change, volatile default) and its
// list of allowlisted destructive migrations.
type MigrationSafetyConfig struct {
	Enabled            *bool    `yaml:"enabled,omitempty"`             // nil = enabled
	UnsafeAddColumn    string   `yaml:"unsafe_add_column,omitempty"`   // error, warn, off
	DestructiveChange  string   `yaml:"destructive_change,omitempty"`  // error, warn, off
	VolatileDefault    string   `yaml:"volatile_default,omitempty"`    // warn, error, off
	AllowedDestructive []string `yaml:"allowed_destructive,omitempty"` // file globs that may contain destructive changes
}

// IsEnabled reports whether migration safety linting is on. Nil
// Enabled means "on by default" so opt-in is implicit.
func (c MigrationSafetyConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// EffectiveUnsafeAddColumn returns the configured severity for the
// unsafe-add-column rule, falling back to "error" when unset/invalid.
func (c MigrationSafetyConfig) EffectiveUnsafeAddColumn() string {
	return effectiveSeverity(c.UnsafeAddColumn, "error")
}

// EffectiveDestructiveChange returns the configured severity for the
// destructive-change rule, falling back to "error" when unset/invalid.
func (c MigrationSafetyConfig) EffectiveDestructiveChange() string {
	return effectiveSeverity(c.DestructiveChange, "error")
}

// EffectiveVolatileDefault returns the configured severity for the
// volatile-default rule, falling back to "warn" when unset/invalid.
func (c MigrationSafetyConfig) EffectiveVolatileDefault() string {
	return effectiveSeverity(c.VolatileDefault, "warn")
}

func effectiveSeverity(value, fallback string) string {
	switch strings.ToLower(value) {
	case "error", "warn", "off":
		return strings.ToLower(value)
	case "warning":
		return "warn"
	default:
		return fallback
	}
}

// CIConfig holds CI/CD settings. Every field here is an input to the
// ONE-TIME workflow render: .github/workflows/*.yml is scaffold-once,
// user-owned (Tier-2) and `forge generate` never re-renders it, so editing
// this block changes nothing until the workflow file is deleted.
//
// There is intentionally NO `go_version:` key either: every setup-go step in
// the generated workflows pins with `go-version-file: go.mod`, so CI tracks
// the project's own declared toolchain and cannot drift from it.
//
// There is intentionally NO `extra_jobs:` key. Adding a job to a YAML file
// you already own is one edit in .github/workflows/ci.yml; routing it
// through forge.yaml was a second source of truth that the workflow
// generator never read (the template's `range .ExtraJobs` was fed from a
// struct nothing populated), so declared jobs silently vanished.
type CIConfig struct {
	Provider    string       `yaml:"provider"`
	Lint        CILintConfig `yaml:"lint,omitempty"`
	Test        CITestConfig `yaml:"test,omitempty"`
	VulnScan    CIVulnConfig `yaml:"vuln_scan,omitempty"`
	E2E         CIE2EConfig  `yaml:"e2e,omitempty"`
	Permissions CIPermConfig `yaml:"permissions,omitempty"`
}

// CILintConfig controls which linters run in CI.
type CILintConfig struct {
	Golangci        bool `yaml:"golangci"`         // default true
	Buf             bool `yaml:"buf"`              // default true
	BufBreaking     bool `yaml:"buf_breaking"`     // default true
	Frontend        bool `yaml:"frontend"`         // default true
	MigrationSafety bool `yaml:"migration_safety"` // default true
}

// CITestConfig controls test settings in CI.
type CITestConfig struct {
	Race     bool `yaml:"race"`     // default true
	Coverage bool `yaml:"coverage"` // default false
}

// CIVulnConfig controls vulnerability scanning in CI.
type CIVulnConfig struct {
	Go     bool `yaml:"go"`     // govulncheck, default true
	Docker bool `yaml:"docker"` // trivy, default true
	NPM    bool `yaml:"npm"`    // npm audit, default true
}

// CIE2EConfig controls end-to-end testing in CI.
type CIE2EConfig struct {
	Enabled bool   `yaml:"enabled"`           // default false
	Runtime string `yaml:"runtime,omitempty"` // "docker-compose" or "k3d"
}

// CIPermConfig controls CI workflow permissions.
type CIPermConfig struct {
	Contents string `yaml:"contents,omitempty"` // default "read"
}

// IsLintEnabled returns true if any linter is enabled.
// Zero value (all false) is treated as "all enabled" (sensible default).
func (c *CIConfig) IsLintEnabled() bool {
	return c.Lint == (CILintConfig{}) || c.Lint.Golangci || c.Lint.Buf || c.Lint.BufBreaking || c.Lint.Frontend || c.Lint.MigrationSafety
}

// IsTestRaceEnabled returns true if the race detector should be used.
// Zero value is treated as enabled.
func (c *CIConfig) IsTestRaceEnabled() bool {
	return c.Test == (CITestConfig{}) || c.Test.Race
}

// IsVulnScanEnabled returns true if any vulnerability scanner is enabled.
// Zero value is treated as "all enabled".
func (c *CIConfig) IsVulnScanEnabled() bool {
	return c.VulnScan == (CIVulnConfig{}) || c.VulnScan.Go || c.VulnScan.Docker || c.VulnScan.NPM
}

// EffectivePermContents returns the contents permission, defaulting to "read".
func (c *CIConfig) EffectivePermContents() string {
	if c.Permissions.Contents != "" {
		return c.Permissions.Contents
	}
	return "read"
}

// DeployConfig holds deployment PIPELINE-CONTROL settings (target_arch,
// migration_test, concurrency, frontend_deploy) plus two inputs consumed
// by the CI-workflow generator (generate_ci.go):
//
//   - the CI provider (github/gitlab/…) lives in `ci.provider`, not here
//     — the dead `deploy.provider` field was removed (see
//     removedSchemaKeys); generate_ci.go reads cfg.CI.Provider.
//   - `deploy.registry` is the image registry stamped into the generated
//     build-images.yml / deploy.yml workflows (EffectiveRegistry, default
//     "ghcr"). It overlaps `docker.registry` (the registry `forge build`
//     uses); the two are independent fields that can disagree.
//   - `deploy.environments` supplies optional per-env auto/protection/url
//     metadata for the generated deploy.yml. When empty, the deployable
//     environment set is derived from the on-disk deploy/kcl/<env>/
//     directories (buildDeployWorkflowData's ListEnvs fallback).
type DeployConfig struct {
	Registry       string            `yaml:"registry,omitempty"` // image registry for the generated CI workflows; overlaps docker.registry
	Environments   []DeployEnvConfig `yaml:"environments,omitempty"`
	Concurrency    DeployConcurrency `yaml:"concurrency,omitempty"`
	FrontendDeploy string            `yaml:"frontend_deploy,omitempty"` // "firebase", "vercel", "none"
	MigrationTest  bool              `yaml:"migration_test,omitempty"`  // test migrations before deploy

	// TargetArch is the GOARCH the deploy target cluster runs on. When
	// unset, forge defaults to amd64 (the predominant k8s host arch).
	// Setting this at the project level means Mac/arm64 dev machines
	// will cross-compile the Go binary (GOOS=linux GOARCH=<target>
	// CGO_ENABLED=0) and pass --platform=linux/<target> to docker
	// buildx so the image kubelet pulls actually runs on the node.
	//
	// Without cross-compile, an arm64-built image deployed onto an
	// amd64 node fails at pod startup with the opaque kernel-level
	// "exec format error". The CLI's --target-arch flag overrides
	// this per-invocation.
	TargetArch string `yaml:"target_arch,omitempty"`
}

// EffectiveTargetArch returns the deploy-target GOARCH. Order of
// precedence: explicit override (caller-provided), forge.yaml's
// deploy.target_arch, then the default "amd64". The "amd64" default
// reflects the empirical reality that the vast majority of k8s nodes
// are amd64; arm64 deployments must opt in via forge.yaml or
// --target-arch.
func (d *DeployConfig) EffectiveTargetArch(override string) string {
	if override != "" {
		return override
	}
	if d.TargetArch != "" {
		return d.TargetArch
	}
	return "amd64"
}

// DeployEnvConfig defines a deployment environment.
type DeployEnvConfig struct {
	Name       string `yaml:"name"`                 // staging, preprod, prod
	Auto       bool   `yaml:"auto,omitempty"`       // auto-deploy
	Protection bool   `yaml:"protection,omitempty"` // environment protection gates
	URL        string `yaml:"url,omitempty"`        // environment URL
}

// DeployConcurrency controls deployment concurrency settings.
type DeployConcurrency struct {
	Enabled          bool `yaml:"enabled"`                      // default true
	CancelInProgress bool `yaml:"cancel_in_progress,omitempty"` // default false
}

// SmokeConfig declares APP-FLOW health checks `forge env smoke <env>` probes
// alongside its built-in ingress/dev-port probes. The built-in probes only
// verify TRANSPORT (a listener answered); they can be GREEN while the app is
// functionally broken. A flow check lets the app DECLARE an end-to-end
// invariant that the OWNING SERVICE asserts INTERNALLY and exposes as an HTTP
// flow-health endpoint (200 healthy / 503 unhealthy). smoke just CURLS that
// endpoint and folds the status into its PASS/FAIL/exit report — so a green
// smoke means the app actually works, not just that ports are open.
//
// WHY AN ENDPOINT, NOT A COMMAND. The owning service already holds the access
// (DB creds, cluster vantage point) the assertion needs; running the check
// inside it avoids handing smoke privileged creds. smoke needs only a URL +
// reachability. The endpoint is STATUS-ONLY in public (200/503 + aggregate
// counts) so it leaks nothing sensitive anonymously; per-entity DETAIL lives
// behind auth or an internal-only port.
//
// The daemon-flow case that motivated this: `forge env smoke dev` was GREEN while
// the managed-daemon flow was broken, because no built-in probe could assert
// "every Ready daemon is attached to the gateway". reliant's daemon-gateway
// owns that state, so it exposes `/flow-health` (200/503) and smoke curls it.
type SmokeConfig struct {
	// FlowChecks are the declared app-flow health endpoints. Each is an HTTP
	// endpoint smoke probes; 2xx = PASS, anything else (typically 503) = FAIL
	// (RED), and any FAIL fails the whole smoke run (non-zero exit), exactly
	// like a failed route probe.
	FlowChecks []SmokeFlowCheck `yaml:"flow_checks,omitempty"`
}

// SmokeFlowCheck is one declared app-flow health endpoint `forge env smoke <env>`
// probes. The owning service asserts the invariant internally and returns 200
// (healthy) / 503 (unhealthy) at this endpoint; smoke curls it and merges the
// verdict into its summary + exit logic. It is the HTTP-endpoint analogue of a
// route probe — same machinery, declared by the app.
type SmokeFlowCheck struct {
	// Name labels the check in the smoke table / JSON (e.g. "daemon-flow").
	Name string `yaml:"name"`
	// URL is the flow-health endpoint smoke GETs. It may be a per-env literal
	// (e.g. "http://localhost:28091/flow-health" for dev) — scope it with Envs
	// when the URL differs per env. A 2xx is PASS; any other status (or a
	// transport failure) is FAIL.
	URL string `yaml:"url"`
	// Envs optionally scopes the check to specific smoke environments (by
	// name). Empty = probe in every env. Use it when the endpoint URL is
	// env-specific (the usual case — different host/port per env).
	Envs []string `yaml:"envs,omitempty"`
	// Description is an optional human note shown in the smoke detail column.
	Description string `yaml:"description,omitempty"`
}

// RunsInEnv reports whether this flow check should be probed for the given
// smoke environment. An empty Envs list means "every env".
func (c SmokeFlowCheck) RunsInEnv(env string) bool {
	if len(c.Envs) == 0 {
		return true
	}
	for _, e := range c.Envs {
		if e == env {
			return true
		}
	}
	return false
}

// EffectiveRegistry returns the deploy registry, defaulting to "ghcr".
func (d *DeployConfig) EffectiveRegistry() string {
	if d.Registry != "" {
		return d.Registry
	}
	return "ghcr"
}

// IsConcurrencyEnabled returns true if deploy concurrency is enabled.
// Zero value is treated as enabled.
func (d *DeployConfig) IsConcurrencyEnabled() bool {
	return d.Concurrency == (DeployConcurrency{}) || d.Concurrency.Enabled
}

// (BuildConfig was removed: forge.yaml no longer carries any build
// config. Build is a per-service, per-env polymorphic declaration in KCL
// — see the GoBuild | DockerBuild | ShellBuild union on forge.Service.
// Version stamping moved to a KCL GoBuild.ldflags `-X` entry. A `build:`
// key in forge.yaml is now rejected as an unknown key.)

// DockerConfig holds Docker build configuration for the PROJECT image (the
// root `Dockerfile` built by `forge build`). Per-service DockerBuild services
// declared in KCL carry their OWN registry + build_contexts on the KCL
// DockerBuild block; these are the project-image defaults / fallback.
//
// forge is FULLY base-image-AGNOSTIC: it does NOT discover, mirror, pin, or
// inject base images, and offers no mirror/pull-through setting. A Dockerfile's
// `FROM` lines are the COMPLETE source of truth — pin with `FROM …@sha256:…`,
// and route through a pull-through mirror by writing the mirror host into the
// `FROM` ref directly (e.g. `FROM us-docker.pkg.dev/<p>/dockerhub/alpine:3.21`).
// forge never rewrites a FROM.
type DockerConfig struct {
	Registry string `yaml:"registry"`
	// BuildContexts maps a build-context name to anything `docker buildx
	// --build-context name=value` accepts:
	//
	//   - A local filesystem path. Relative paths are resolved against the
	//     project root (the directory holding forge.yaml). The typical
	//     case is a sibling-checkout local replace directive (e.g. a
	//     `replace x => ../x` in go.mod where ../x lives outside the
	//     project's build context).
	//   - A `docker-image://<image>` ref. Passed through verbatim so a
	//     Dockerfile `FROM <name>` can be overridden with a specific image
	//     at build time (local override of a base image during dev,
	//     pin-by-digest in CI, etc.).
	//   - Any other scheme buildkit understands (e.g. `oci-layout://`,
	//     `https://`). Anything containing `://` is passed through
	//     unchanged.
	//
	// Each entry becomes a `--build-context name=value` arg to `docker
	// build`, letting Dockerfiles consume it via `FROM <name>` or
	// `COPY --from=<name>`. Empty when not set; existing projects with no
	// contexts see no change in build behaviour or output.
	BuildContexts map[string]string `yaml:"build_contexts,omitempty"`
}

// Typed-access enforcement levels for [ConfigGuardConfig.EnforceTypedAccess].
//
//   - off   — emit no env-reading guardrail at all.
//   - warn  — emit the forbidigo guardrail as an ADVISORY check: violations
//     are reported (during `forge lint` and in the generated .golangci.yml)
//     but never fail the build. This is the default for an ABSENT `config:`
//     block, so existing projects upgrade without a flag-day.
//   - error — emit the forbidigo guardrail as a GATING check: violations
//     fail `forge lint` / CI. `forge project new` scaffolds this for greenfield
//     projects, which carry no legacy env-reading debt.
const (
	EnforceTypedAccessOff   = "off"
	EnforceTypedAccessWarn  = "warn"
	EnforceTypedAccessError = "error"
)

// enforce-component-observe levels for [ConfigGuardConfig.EnforceComponentObserve].
//
//   - error — (default) a wired component with a Service interface + a
//     canonical New(Deps) Service constructor that has made NO observability
//     decision (neither `// forge:constructor` nor `// forge:no-observe`) fails
//     `forge lint`. The forcing-function that makes the in-process
//     observability decision discoverable instead of silently defaulted.
//   - off   — disable the check entirely.
//
// Unlike enforce_typed_access there is no "warn": the whole point is a gate.
const (
	EnforceComponentObserveOff   = "off"
	EnforceComponentObserveError = "error"
)

// DefaultLoaderPackage is the package path forge's config-loader codegen
// emits into a consuming project (generated from
// proto/config/v1/config.proto via the (forge.v1.config) annotation). It is
// the ONE package allowed to read the environment directly; the typed-access
// guardrail allowlists it so the generated loader can legitimately call
// os.Getenv / os.LookupEnv.
const DefaultLoaderPackage = "pkg/config"

// ConfigGuardConfig is the `config:` section of forge.yaml. It steers
// humans and LLMs away from reading the environment directly and toward
// forge's generated, dependency-injected typed config object.
//
// The block is OPTIONAL. When absent, EnforceTypedAccess resolves to "warn"
// (see [EnforceTypedAccessWarn]) so existing projects gain the advisory
// guardrail without a flag-day; `forge project new` writes an explicit
// `enforce_typed_access: error` for greenfield projects.
//
// ADOPTION / re-render: the strictness is projected into the generated
// .golangci.yml (forbidigo in `linters.enable` for error, settings-only for
// warn, absent for off). That file is a SCAFFOLD-ONCE, user-owned Tier-2
// artifact — `forge generate` never re-renders it, and `forge project upgrade` only
// auto-updates it when the on-disk copy is an unedited forge render (a
// verifying forge:hash marker). A freshly-scaffolded .golangci.yml carries no
// marker and is "user-owned from birth", so after changing
// enforce_typed_access in forge.yaml the user must explicitly re-render:
// `rm .golangci.yml && forge project upgrade` (re-scaffold), or `forge project upgrade
// --force` when they have no local .golangci.yml edits. This is the
// deliberate Tier-2 contract — forge will not silently stomp a hand-tuned
// linter config — not a bug. The forbidigo `msg`, the template's warn-mode
// comment, and `forge lint`'s advisory line all teach this path.
type ConfigGuardConfig struct {
	// EnforceTypedAccess selects the env-access guardrail strictness:
	// "off" | "warn" | "error". Empty resolves to "warn" — use
	// [ConfigGuardConfig.EffectiveEnforceTypedAccess], don't read the raw
	// string. Unknown values are rejected at load time (validate.go).
	EnforceTypedAccess string `yaml:"enforce_typed_access,omitempty"`
	// LoaderPackage is the allowlisted package path that may read the
	// environment directly — the home of forge's generated config loader.
	// Empty resolves to [DefaultLoaderPackage] ("pkg/config"); use
	// [ConfigGuardConfig.EffectiveLoaderPackage].
	LoaderPackage string `yaml:"loader_package,omitempty"`
	// EnforceComponentObserve selects the enforce-component-observe lint
	// strictness: "error" (default) | "off". Empty resolves to "error" — use
	// [ConfigGuardConfig.EffectiveEnforceComponentObserve], don't read the raw
	// string. Unknown values are rejected at load time (validate.go). See the
	// EnforceComponentObserve* constants for the semantics.
	EnforceComponentObserve string `yaml:"enforce_component_observe,omitempty"`
}

// EffectiveEnforceTypedAccess returns the resolved guardrail strictness,
// defaulting an absent/empty value to "warn" (advisory). It normalizes case
// and the "warning" alias. Validation (validate.go) has already rejected
// any other non-empty value, so this never silently swallows a typo.
func (c ConfigGuardConfig) EffectiveEnforceTypedAccess() string {
	switch strings.ToLower(strings.TrimSpace(c.EnforceTypedAccess)) {
	case EnforceTypedAccessOff:
		return EnforceTypedAccessOff
	case EnforceTypedAccessError:
		return EnforceTypedAccessError
	case EnforceTypedAccessWarn, "warning":
		return EnforceTypedAccessWarn
	default:
		return EnforceTypedAccessWarn
	}
}

// EffectiveLoaderPackage returns the allowlisted loader package path,
// defaulting an absent/empty value to [DefaultLoaderPackage].
func (c ConfigGuardConfig) EffectiveLoaderPackage() string {
	if p := strings.TrimSpace(c.LoaderPackage); p != "" {
		return p
	}
	return DefaultLoaderPackage
}

// TypedAccessGuardEnabled reports whether the env-access guardrail should be
// emitted at all (true unless the strictness is "off").
func (c ConfigGuardConfig) TypedAccessGuardEnabled() bool {
	return c.EffectiveEnforceTypedAccess() != EnforceTypedAccessOff
}

// TypedAccessGuardGates reports whether the guardrail FAILS the build (true
// only in "error" mode; "warn" is advisory, "off" emits nothing).
func (c ConfigGuardConfig) TypedAccessGuardGates() bool {
	return c.EffectiveEnforceTypedAccess() == EnforceTypedAccessError
}

// EffectiveEnforceComponentObserve returns the resolved enforce-component-observe
// strictness, defaulting an absent/empty value to "error" (the check is on by
// default). Any value other than a case-insensitive "off" resolves to "error";
// validation (validate.go) has already rejected non-empty non-{off,error}
// values, so a typo never silently disables the gate.
func (c ConfigGuardConfig) EffectiveEnforceComponentObserve() string {
	if strings.EqualFold(strings.TrimSpace(c.EnforceComponentObserve), EnforceComponentObserveOff) {
		return EnforceComponentObserveOff
	}
	return EnforceComponentObserveError
}

// ComponentObserveGuardEnabled reports whether the enforce-component-observe
// lint runs at all (true unless the strictness is "off").
func (c ConfigGuardConfig) ComponentObserveGuardEnabled() bool {
	return c.EffectiveEnforceComponentObserve() != EnforceComponentObserveOff
}

// LintConfig holds lint-related settings.
//
// There is intentionally NO `contract:` toggle here. Whether the contract
// lint runs is `features.contracts` — the flag the gate actually resolves;
// a second `lint.contract` boolean had no reader at all.
type LintConfig struct {
	Frontend FrontendLintConfig `yaml:"frontend,omitempty"`
	// HandlerFileMaxLOC is the per-file LOC threshold above which the
	// forgeconv-handler-file-size analyzer warns. Counts non-blank, non-
	// comment Go source lines under handlers/<svc>/*.go. A value of 0 (or
	// the field unset) is treated as the built-in default — see
	// [LintConfig.EffectiveHandlerFileMaxLOC] for the canonical value.
	HandlerFileMaxLOC int `yaml:"handler_file_max_loc,omitempty"`

	// Rules is the repo-wide severity dial for forge-owned lint rules:
	// rule ID → "off" | "warn" | "error". It is the COARSEST of the
	// three suppression levels (see internal/linter/suppress) and the
	// one to reach for last, because — like a path glob in
	// .golangci.yml — it silently swallows every future finding for
	// that rule, including the one you would have wanted to see.
	//
	// Prefer a file or line directive when the exemption is local. This
	// map is for rules that genuinely do not apply to the whole project
	// (a CLI has no handlers; an operator has no frontend), where the
	// alternative is annotating every file.
	//
	// The `warn` setting is the load-bearing one: it lets a project
	// adopt a newly-added opinionated rule incrementally instead of
	// choosing between "fails the build today" and "off".
	//
	// Only forge's OWN rules are addressable here. golangci-lint,
	// eslint, stylelint and buf each have their own config file and
	// their own suppression syntax, and forge deliberately does not
	// proxy them — a second place to configure the same linter is a
	// place for the two to disagree.
	//
	// The special key "*" applies to every forge rule; an exact rule ID
	// beats it.
	Rules map[string]string `yaml:"rules,omitempty"`
}

// ValidateRules reports rule-severity entries forge cannot interpret.
//
// This exists because of the failure mode the Override path itself
// cannot fix: an unknown key is treated as inert (the rule keeps
// firing), which is the safe direction to fail but also a silent one.
// A project that misspells `forgeconv-hadler-file-size: off` would
// otherwise never learn why the rule kept firing. Validating at
// config-load time turns that silence into a message.
//
// Rule IDs are NOT checked against a registry of known rules — a
// forge.yaml is shared across forge versions, and a rule that exists in
// the version your colleague runs but not yours must not fail your
// build. Only the SEVERITY vocabulary is closed.
func (c LintConfig) ValidateRules() []string {
	if len(c.Rules) == 0 {
		return nil
	}
	var problems []string
	for rule, spec := range c.Rules {
		if !suppress.ValidSeverity(spec) {
			problems = append(problems, fmt.Sprintf(
				"lint.rules[%q]: %q is not a severity — use \"off\", \"warn\", or \"error\"",
				rule, spec))
		}
	}
	sort.Strings(problems)
	return problems
}

// EffectiveRules returns the rule-severity map in the form the linters
// consume, with keys lowercased so forge.yaml is not case-sensitive
// about rule IDs.
func (c LintConfig) EffectiveRules() suppress.RuleSeverities {
	if len(c.Rules) == 0 {
		return nil
	}
	out := make(suppress.RuleSeverities, len(c.Rules))
	for rule, spec := range c.Rules {
		out[strings.ToLower(strings.TrimSpace(rule))] = spec
	}
	return out
}

// DefaultHandlerFileMaxLOC is the built-in threshold used by the
// forgeconv-handler-file-size analyzer when the project does not set
// lint.handler_file_max_loc in forge.yaml. Picked at 1000 because that
// roughly tracks "two screens of any modern editor" plus generous
// buffer — files past that point materially harm review velocity and
// almost always benefit from the per-RPC split that `forge scaffold
// handler-file` is intended to support.
const DefaultHandlerFileMaxLOC = 1000

// EffectiveHandlerFileMaxLOC returns the LOC threshold for the
// handler-file-size analyzer, defaulting to [DefaultHandlerFileMaxLOC]
// when the config value is zero or unset.
func (c LintConfig) EffectiveHandlerFileMaxLOC() int {
	if c.HandlerFileMaxLOC <= 0 {
		return DefaultHandlerFileMaxLOC
	}
	return c.HandlerFileMaxLOC
}

// FrontendLintConfig configures the frontend slice of `forge lint`:
// whether the stylelint-backed CSS health checks run, and which
// severity ("error"/"warn"/"off") the `no-important`,
// `no-inline-styles`, and `typecheck` rules use.
type FrontendLintConfig struct {
	CSSHealth      bool   `yaml:"css_health,omitempty"`       // enable stylelint-backed CSS health checks
	NoImportant    string `yaml:"no_important,omitempty"`     // error, warn, off
	NoInlineStyles string `yaml:"no_inline_styles,omitempty"` // error, warn, off
	// Typecheck is the severity of the TypeScript typecheck lane
	// ("error" | "warn" | "off"); see [FrontendLintConfig.EffectiveTypecheck].
	Typecheck string `yaml:"typecheck,omitempty"`
}

// EffectiveTypecheck returns the configured severity for the frontend
// TypeScript typecheck lane, defaulting to "error" when unset or invalid.
//
// The default gates on purpose: a type error is a real defect that the Go
// half of the pipeline can never see, and shipping it is exactly the class
// of break this lane exists to catch. Projects that want the signal
// without the gate set "warn"; "off" removes the lane entirely.
//
// Note this dial governs TYPE ERRORS only. A typecheck that could not RUN
// (deps not installed, no TypeScript resolvable) is always reported as a
// warning — never silently as a pass — and escalates to an error under
// `forge lint --strict`.
func (c FrontendLintConfig) EffectiveTypecheck() string {
	return effectiveSeverity(c.Typecheck, "error")
}

// EffectiveNoImportant returns the configured severity for the
// no-important rule, falling back to "warn" when unset/invalid.
func (c FrontendLintConfig) EffectiveNoImportant() string {
	return effectiveSeverity(c.NoImportant, "warn")
}

// EffectiveNoInlineStyles returns the configured severity for the
// no-inline-styles rule, falling back to "warn" when unset/invalid.
func (c FrontendLintConfig) EffectiveNoInlineStyles() string {
	return effectiveSeverity(c.NoInlineStyles, "warn")
}

// ContractsConfig controls contract enforcement linter behavior.
//
// It carries only the two ESCAPE HATCHES the analyzer reads: which
// packages opt out (`exclude`) and which project-local types the mock
// generator must treat as interfaces (`interface_types`). The old
// severity dials (`strict`, `allow_exported_vars`, `allow_exported_funcs`)
// are gone: the contract rules are unconditional — a package either opts
// out by path or obeys them — and no analyzer ever read those three
// booleans. Whether the lint runs at all is `features.contracts`.
type ContractsConfig struct {
	Exclude []string `yaml:"exclude"` // packages that opt out
	// InterfaceTypes lists additional cross-package interface types (over
	// and above the built-in list in internal/generator/contract) that the
	// mock generator should treat as mockable — i.e. emit "nil" as the
	// fallback zero value instead of the invalid composite literal "T{}".
	//
	// Entries are matched against the rendered Go type expression of a
	// contract method's return value, e.g. "billing.MeterClient" or
	// "myproject.SomeProjectLocalInterface". Use this when a contract
	// method returns a project-local interface that the mock generator
	// would otherwise mistakenly treat as a struct.
	InterfaceTypes []string `yaml:"interface_types"`
}

// IsExcluded returns true if the given package path matches any
// exclude pattern. Delegates to [MatchExclude] — the shared matcher
// used by the contract analyzer and the forgeconv lint surface so all
// three places agree on what "excluded" means. See the doc on
// MatchExclude for the matching rules and the deliberate exit from the
// pre-2026-06 inline implementation (empty-pattern handling +
// slash-normalisation).
func (c ContractsConfig) IsExcluded(pkgPath string) bool {
	return MatchExclude(c.Exclude, pkgPath)
}

// FeaturesConfig controls which forge project features are active. The `features:`
// block in forge.yaml gates major subsystems (deploy, build, frontend,
// ci, docs, observability, ...).
//
// THE BLOCK IS AN OVERRIDE SURFACE, NOT REQUIRED CONFIGURATION. Scaffolded
// forge.yaml files do not contain it. All fields are *bool so the loader can
// distinguish three states:
//
//   - absent (nil): the value is DERIVED from the project shape at load
//     time — kind (service/cli/library), whether a database driver is
//     configured, whether the frontends list is non-empty. See
//     DeriveFeatureDefaults in derive.go for the exact rule per feature.
//     For the canonical shape (kind=service, postgres, frontends present)
//     every derived value is "enabled", matching the historical
//     all-enabled default for projects without a features: block.
//   - explicitly true / explicitly false: taken literally; derivation
//     never overrides an explicit value.
//
// A FeaturesConfig that was not produced by the config loader (zero value
// in tests, hand-constructed) has no derivation context and resolves
// nil → enabled, preserving the historical zero-value semantics.
//
// Effect on the CLI surface and codegen pipeline:
//
//   - Direct invocations of a disabled subsystem's cobra command return a
//     clear `feature '<name>' is disabled in forge.yaml. Set
//     features.<name>: true to enable.` error.
//   - Implicit invocations from orchestrators (e.g. `forge env up` driving
//     the build/deploy/frontend phases) log a skip line and continue —
//     letting `forge env up` succeed on whatever subsystems ARE enabled.
//   - Codegen pipeline steps gated on a feature skip silently when off,
//     mirroring the existing gate function shape under
//     internal/cli/generate_pipeline.go.
//
// New project scaffolding (`forge project new --kind`) sets defaults per kind:
//
//   - service (default): all features enabled (preserves today's behavior).
//   - cli:               build/ci/docs enabled; everything else disabled.
//   - library:           ci/docs enabled; everything else disabled.
type FeaturesConfig struct {
	ORM           *bool `yaml:"orm,omitempty"`           // ORM projection of db/migrations (internal/db/*_orm.go)
	Codegen       *bool `yaml:"codegen,omitempty"`       // service/handler codegen from protos
	Migrations    *bool `yaml:"migrations,omitempty"`    // auto-generate SQL migrations
	CI            *bool `yaml:"ci,omitempty"`            // generate CI/CD workflows
	Build         *bool `yaml:"build,omitempty"`         // `forge build` Go binary + docker image pipeline
	Contracts     *bool `yaml:"contracts,omitempty"`     // contract linter enforcement
	Docs          *bool `yaml:"docs,omitempty"`          // documentation generation
	Frontend      *bool `yaml:"frontend,omitempty"`      // frontend scaffolding + codegen
	Observability *bool `yaml:"observability,omitempty"` // alloy, grafana dashboards, otel wiring
	HotReload     *bool `yaml:"hot_reload,omitempty"`    // air config generation
	Deploy        *bool `yaml:"deploy,omitempty"`        // deploy pipeline: KCL render → kubectl apply, per-env deploy config codegen

	// Diagnostics was to enable runtime emission of pkg/diagnostics records
	// at Bootstrap time. Nothing reads it: no codegen path emits the
	// registration file the runtime would boot from, so the knob drives
	// nothing today. It stays declared only so a forge.yaml that already
	// sets it does not trip the unknown-key check — give it a reader or
	// drop it in a major, but do not add a consumer that pretends it works.
	Diagnostics *bool `yaml:"diagnostics,omitempty"`

	// Experimental gates surface that hasn't been battle-tested across
	// real projects + cloud providers. Everything inside is default-OFF
	// (opt-in), every gated CLI invocation prints a one-line warning the
	// first time per process, and the schema is allowed to break between
	// forge versions without a deprecation cycle. Graduates to the
	// top-level FeaturesConfig (with the usual opt-out default-ON
	// semantics) when the feature has shipped through enough real
	// deployments to earn a backwards-compatibility promise.
	Experimental ExperimentalConfig `yaml:"experimental,omitempty"`

	// derived carries the shape-derived default for every stable feature,
	// resolved by the loader (ApplyDerivedDefaults via DeriveFeatureDefaults)
	// from kind / database / frontends. nil (zero-value FeaturesConfig,
	// hand-constructed in tests) falls back to the historical "absent =
	// enabled" semantics. Unexported + yaml-invisible: never serialized,
	// never user-set.
	derived map[FeatureName]bool `yaml:"-"`
}

// stablePtrs is the single feature registry: it maps every stable
// FeatureName to the address of its explicit *bool override field. The
// resolver, the write-side normalizer, and EffectiveFeatures all drive
// off this one map, so adding a stable feature is a single edit here (plus
// the field, the FeatureName constant, and its DeriveFeatureDefaults rule)
// instead of a transcription scattered across parallel switch arms.
func (f *FeaturesConfig) stablePtrs() map[FeatureName]**bool {
	return map[FeatureName]**bool{
		FeatureORM:           &f.ORM,
		FeatureCodegen:       &f.Codegen,
		FeatureMigrations:    &f.Migrations,
		FeatureCI:            &f.CI,
		FeatureBuild:         &f.Build,
		FeatureContracts:     &f.Contracts,
		FeatureDocs:          &f.Docs,
		FeatureFrontend:      &f.Frontend,
		FeatureObservability: &f.Observability,
		FeatureHotReload:     &f.HotReload,
		FeatureDeploy:        &f.Deploy,
	}
}

// IsZero reports whether the features block carries no explicit user
// choices — every stable flag nil and no experimental opt-ins. Implements
// yaml.IsZeroer so `features,omitempty` omits the block entirely from a
// marshalled forge.yaml when there is nothing explicit to record (the
// derived field is resolution context, not content).
func (f FeaturesConfig) IsZero() bool {
	return f.ORM == nil && f.Codegen == nil && f.Migrations == nil &&
		f.CI == nil && f.Build == nil && f.Contracts == nil &&
		f.Docs == nil && f.Frontend == nil && f.Observability == nil &&
		f.HotReload == nil &&
		f.Deploy == nil &&
		f.Diagnostics == nil && f.Experimental.IsZero()
}

// ExperimentalConfig gates features that are not yet promised. Fields
// are plain bool (not *bool) — the zero value IS the default, and the
// default IS off. Loud-warning policy on startup when any field is true.
//
// What lives here today:
//
//   - Ingress:        Gateway API codegen + cert-manager + Envoy
//     Gateway wiring. Provider matrix is fragile and not yet
//     proven across real cloud providers.
//   - ExternalBuilds: RETIRED gate (kept as an accepted, inert key for
//     back-compat). `Service.build_cmd` is the build-side
//     mirror of `External.deploy_cmd`; since `forge env deploy`
//     of an External target never required an opt-in,
//     gating `forge build` of the same target behind this
//     flag left the build/deploy pair with mismatched
//     maturity gates (fr-da9a6614fb). The build path no
//     longer consults this flag — build_cmd just builds.
//     Setting it true is harmless (and still accepted so
//     existing forge.yaml files don't trip the unknown-key
//     check); a future major can drop the field.
//   - Operators:      controller-runtime managers + CRD codegen. Niche,
//     under-exercised, the API may need to change as we
//     learn what real operator authors want.
//   - StrictWiring:   diagnostics fail-fast — any registered diagnostic
//     terminates the process after Bootstrap. Implies
//     Diagnostics: true. Stays experimental because the
//     diagnostics catalogue itself is still settling.
type ExperimentalConfig struct {
	Ingress        bool `yaml:"ingress,omitempty"`
	ExternalBuilds bool `yaml:"external_builds,omitempty"`
	Operators      bool `yaml:"operators,omitempty"`
	StrictWiring   bool `yaml:"strict_wiring,omitempty"`
}

// IsZero reports whether the experimental block carries nothing explicit.
func (e ExperimentalConfig) IsZero() bool {
	return !e.Ingress && !e.ExternalBuilds && !e.Operators && !e.StrictWiring
}

// resolve resolves a stable feature flag by name: an explicit value wins;
// absent (nil) resolves to the shape-derived default when the loader
// attached one, else to the historical "absent = enabled" default (zero
// value FeaturesConfig, hand-constructed in tests, no forge.yaml context).
// All public XxxEnabled() accessors are thin wrappers over this.
func (f FeaturesConfig) resolve(name FeatureName) bool {
	if ptr := *f.stablePtrs()[name]; ptr != nil {
		return *ptr
	}
	if f.derived == nil {
		return true
	}
	return f.derived[name]
}

// EffectiveKind returns the project kind, defaulting to "service".
func (c ProjectConfig) EffectiveKind() string {
	return EffectiveProjectKind(c.Kind)
}

// EffectiveBinary returns the binary mode, defaulting to "per-service"
// so legacy forge.yaml files without the field keep producing the
// canonical cmd/server.go shape.
func (c ProjectConfig) EffectiveBinary() string {
	return EffectiveProjectBinary(c.Binary)
}

// IsBinaryShared reports whether the project uses the shared-binary
// codegen mode (one Go binary, cobra subcommand per service, KCL
// MultiServiceApplication for deploy).
func (c ProjectConfig) IsBinaryShared() bool {
	return c.EffectiveBinary() == ProjectBinaryShared
}

// EffectiveForgeVersion returns the forge version pinned by this project,
// defaulting to "0.0.0" for legacy projects that predate the field.
// Callers can use the "0.0.0" sentinel to detect "no baseline yet" and
// nudge the user toward `forge project upgrade`.
func (c ProjectConfig) EffectiveForgeVersion() string {
	if strings.TrimSpace(c.ForgeVersion) == "" {
		return "0.0.0"
	}
	return c.ForgeVersion
}

// IsCLIKind reports whether the project is a CLI binary (no server scaffolding).
func (c ProjectConfig) IsCLIKind() bool { return c.EffectiveKind() == ProjectKindCLI }

// IsLibraryKind reports whether the project is a pure Go library (no cmd/).
func (c ProjectConfig) IsLibraryKind() bool { return c.EffectiveKind() == ProjectKindLibrary }

// IsServiceKind reports whether the project is a Connect-RPC service.
func (c ProjectConfig) IsServiceKind() bool { return c.EffectiveKind() == ProjectKindService }

// ORMEnabled reports whether the ORM feature is on (default: on).
func (f FeaturesConfig) ORMEnabled() bool { return f.resolve(FeatureORM) }

// CodegenEnabled reports whether codegen is on (default: on).
func (f FeaturesConfig) CodegenEnabled() bool { return f.resolve(FeatureCodegen) }

// MigrationsEnabled reports whether the migrations feature is on (default: on).
func (f FeaturesConfig) MigrationsEnabled() bool { return f.resolve(FeatureMigrations) }

// CIEnabled reports whether the CI feature is on (default: on).
func (f FeaturesConfig) CIEnabled() bool { return f.resolve(FeatureCI) }

// DeployEnabled reports whether the deploy feature is on. Stable flag:
// absent derives from project shape (deploy ⇔ kind == service — see
// DeriveFeatureDefaults), explicit `features.deploy: true|false` wins.
// Service scaffolds ship a deploy/kcl tree, so deploy is ON for the
// canonical service shape; cli/library kinds derive OFF.
func (f FeaturesConfig) DeployEnabled() bool { return f.resolve(FeatureDeploy) }

// ContractsEnabled reports whether contract enforcement is on (default: on).
func (f FeaturesConfig) ContractsEnabled() bool { return f.resolve(FeatureContracts) }

// DocsEnabled reports whether the docs feature is on (default: on).
func (f FeaturesConfig) DocsEnabled() bool { return f.resolve(FeatureDocs) }

// FrontendEnabled reports whether the frontend feature is on (default: on).
func (f FeaturesConfig) FrontendEnabled() bool { return f.resolve(FeatureFrontend) }

// ObservabilityEnabled reports whether the observability feature is on (default: on).
func (f FeaturesConfig) ObservabilityEnabled() bool { return f.resolve(FeatureObservability) }

// HotReloadEnabled reports whether the hot-reload feature is on (default: on).
func (f FeaturesConfig) HotReloadEnabled() bool { return f.resolve(FeatureHotReload) }

// BuildEnabled reports whether `forge build` is enabled (default: on).
// Direct `forge build` invocations error when off; orchestrators like
// `forge env up` log a skip line and continue.
func (f FeaturesConfig) BuildEnabled() bool { return f.resolve(FeatureBuild) }

// IngressEnabled reports whether Gateway API ingress is wired
// (default: OFF — opt-in under `features.experimental.ingress: true`).
// When off, forge skips ingress codegen, `forge cluster up` skips
// the Envoy Gateway + GatewayClass install, `forge cluster urls` returns
// nothing, and the audit ingress category is suppressed.
func (f FeaturesConfig) IngressEnabled() bool { return f.Experimental.Ingress }

// ExternalBuildsEnabled reports the raw value of the RETIRED
// `features.experimental.external_builds` flag. It no longer gates the
// build path: `build_cmd` is the build-side mirror of `External.deploy_cmd`
// (which needs no opt-in), so `forge build` of a build_cmd service runs
// unconditionally (fr-da9a6614fb). The accessor is retained for the
// startup warning / `forge project audit` surface and any consumer still keyed off
// the flag; the build dispatcher in internal/cli/build.go no longer calls
// it.
func (f FeaturesConfig) ExternalBuildsEnabled() bool { return f.Experimental.ExternalBuilds }

// OperatorsEnabled reports whether controller-runtime operator codegen
// + CRD manifest generation is wired (default: OFF — opt-in under
// `features.experimental.operators: true`). When off, the operator
// binary codegen + CRD scaffold steps skip silently and
// `forge scaffold operator` errors.
func (f FeaturesConfig) OperatorsEnabled() bool { return f.Experimental.Operators }

// DisabledFeatureError returns the canonical user-facing error for a
// disabled feature. Centralised so every gate site emits the same
// wording — sub-agents and humans grepping for the string find one
// authoritative format. The name argument is the lowercased feature
// name as it appears in forge.yaml (e.g. "deploy", "build", "frontend").
func DisabledFeatureError(name string) error {
	return errDisabledFeature{name: name}
}

// errDisabledFeature carries the feature name so callers can match
// programmatically (errors.As) without parsing the string. The Error()
// shape matches forge's existing single-line "feature 'X' is disabled in
// forge.yaml" idiom used by the pre-feature-block gates in deploy.go,
// docs.go and ci.go.
type errDisabledFeature struct {
	name string
}

func (e errDisabledFeature) Error() string {
	if IsExperimentalFeature(e.name) {
		return "feature '" + e.name + "' is experimental and opt-in. Set features.experimental." + e.name +
			": true in forge.yaml to enable; the API may change between forge versions."
	}
	return "feature '" + e.name + "' is disabled in forge.yaml. Set features." + e.name + ": true to enable."
}

// FeatureName is the canonical feature key. Stays a string alias so the
// constants below are usable directly anywhere the feature name shows up
// as a config key, a `--disable` flag value, or a `forge project audit` field.
type FeatureName = string

// Feature name constants. These are the wire format — both YAML field
// names under `features:` (top-level) or `features.experimental:`
// (nested) and the strings emitted by `forge project audit --json | jq
// '.features'`. Kept exported so external tooling can match against
// them without re-encoding the spelling. The Experimental* constants
// live under the nested block in YAML but flatten back to a single
// per-name keyspace at the audit-JSON layer.
const (
	FeatureORM           FeatureName = "orm"
	FeatureCodegen       FeatureName = "codegen"
	FeatureMigrations    FeatureName = "migrations"
	FeatureCI            FeatureName = "ci"
	FeatureBuild         FeatureName = "build"
	FeatureContracts     FeatureName = "contracts"
	FeatureDocs          FeatureName = "docs"
	FeatureFrontend      FeatureName = "frontend"
	FeatureObservability FeatureName = "observability"
	FeatureHotReload     FeatureName = "hot_reload"
	FeatureDeploy        FeatureName = "deploy"

	// Experimental feature names — opt-in under
	// `features.experimental.<name>: true`. Default OFF.
	FeatureIngress        FeatureName = "ingress"
	FeatureExternalBuilds FeatureName = "external_builds"
	FeatureOperators      FeatureName = "operators"
	FeatureStrictWiring   FeatureName = "strict_wiring"
)

// ExperimentalFeatureNames lists every Feature* constant that lives
// under `features.experimental:`. Iteration order is the stable display
// order used by `forge project audit`, the startup warning, and `forge project features`.
var ExperimentalFeatureNames = []FeatureName{
	FeatureIngress,
	FeatureExternalBuilds,
	FeatureOperators,
	FeatureStrictWiring,
}

// IsExperimentalFeature reports whether a feature name lives under the
// `features.experimental:` block (i.e. is default-OFF, opt-in, subject
// to schema change). Centralised so audit, the gate helper, and the
// startup-warning emitter share one source of truth.
func IsExperimentalFeature(name FeatureName) bool {
	for _, n := range ExperimentalFeatureNames {
		if n == name {
			return true
		}
	}
	return false
}

// EnabledExperimentalFeatures returns the names of experimental
// features currently turned on, in ExperimentalFeatureNames order.
// Used by the startup warning and `forge project features`.
func (f FeaturesConfig) EnabledExperimentalFeatures() []FeatureName {
	checks := map[FeatureName]bool{
		FeatureIngress:        f.Experimental.Ingress,
		FeatureExternalBuilds: f.Experimental.ExternalBuilds,
		FeatureOperators:      f.Experimental.Operators,
		FeatureStrictWiring:   f.Experimental.StrictWiring,
	}
	out := make([]FeatureName, 0, len(checks))
	for _, name := range ExperimentalFeatureNames {
		if checks[name] {
			out = append(out, name)
		}
	}
	return out
}

// EffectiveFeatures projects the resolved enabled/disabled state of
// every feature into a stable name→bool map. Used by `forge project audit` to
// surface the project's feature configuration at a glance, and by tests
// to assert per-kind scaffold defaults. The map is keyed by Feature*
// constants and is safe to JSON-marshal directly. Experimental features
// are flattened in alongside the stable set under their own keys —
// audit consumers can branch on IsExperimentalFeature(name) when they
// need to distinguish the two tiers.
func (f FeaturesConfig) EffectiveFeatures() map[string]bool {
	return map[string]bool{
		FeatureORM:            f.ORMEnabled(),
		FeatureCodegen:        f.CodegenEnabled(),
		FeatureMigrations:     f.MigrationsEnabled(),
		FeatureCI:             f.CIEnabled(),
		FeatureBuild:          f.BuildEnabled(),
		FeatureContracts:      f.ContractsEnabled(),
		FeatureDocs:           f.DocsEnabled(),
		FeatureFrontend:       f.FrontendEnabled(),
		FeatureObservability:  f.ObservabilityEnabled(),
		FeatureHotReload:      f.HotReloadEnabled(),
		FeatureDeploy:         f.DeployEnabled(),
		FeatureIngress:        f.IngressEnabled(),
		FeatureExternalBuilds: f.ExternalBuildsEnabled(),
		FeatureOperators:      f.OperatorsEnabled(),
		FeatureStrictWiring:   f.StrictWiringEnabled(),
	}
}

// StrictWiringEnabled reports whether the diagnostics strict-mode
// exit is wired by bootstrap (default: OFF — opt-in under
// `features.experimental.strict_wiring: true`) — strict-mode wraps the
// LogEmitter with StrictEmitter so any registered diagnostic terminates
// the process after the summary line.
func (f FeaturesConfig) StrictWiringEnabled() bool {
	return f.Experimental.StrictWiring
}

// StackConfig declares the technology choices for the project.
//
// Historically this block carried six sub-sections (backend, frontend,
// database, proto, deploy, ci) of "forward-looking declarations". Five of
// those (backend/database/proto/deploy/ci) were never consumed by any
// codegen path and merely DUPLICATED the canonical sources — `database.driver`,
// `ci.provider`, `docker.registry` + per-env KCL — so they were removed in
// the forge.yaml schema cleanup (FORGE_SHAPE_REDESIGN §4). Old keys parse
// with a migration warning (see removedSchemaKeys: stack.backend etc.).
//
// Only `stack.frontend.framework` remains: it is genuinely load-bearing
// (read by `forge scaffold frontend` and the frontend-build skip in build.go to
// know whether the project ships a frontend framework at all).
type StackConfig struct {
	Frontend StackFrontend `yaml:"frontend,omitempty"`
}

// StackFrontend declares the frontend framework.
type StackFrontend struct {
	Framework string `yaml:"framework,omitempty"` // "nextjs" (default), "react-native", "svelte", "none"
}

// EffectiveFrontendFramework returns the frontend framework, defaulting to "nextjs".
func (s StackConfig) EffectiveFrontendFramework() string {
	if s.Frontend.Framework != "" {
		return s.Frontend.Framework
	}
	return "nextjs"
}

// APIConfig holds project-level API protocol-skin toggles. Both fields
// default to false, so projects that omit the `api:` block continue to
// expose only the canonical Connect/gRPC handlers without any runtime
// transcoding or generated spec files.
//
// REST=true installs connectrpc.com/vanguard as middleware in front of
// the Connect mux. Vanguard transcodes REST↔Connect at runtime based on
// `google.api.http` annotations on RPCs; the CRUD proto scaffolder also
// emits standard REST-shaped annotations on Get/List/Create/Update/Delete
// RPCs so the default CRUD surface gains REST URLs without hand-editing.
//
// OpenAPI=true is owned by a sibling agent and emits an OpenAPI spec
// alongside the proto compile step. The two fields compose: with both
// on, the generated spec reflects the REST URLs.
type APIConfig struct {
	REST    bool `yaml:"rest,omitempty"`
	OpenAPI bool `yaml:"openapi,omitempty"`
}

// K8sConfig holds Kubernetes configuration.
type K8sConfig struct {
	KCLDir string `yaml:"kcl_dir"`
}

// DocsConfig holds documentation generation settings.
// ObservabilityConfig seeds the OWNED per-package observe_chain.go seam (the
// in-process component middleware chain the generated decorator routes
// through). It is a SCAFFOLD-TIME default: `forge scaffold` reads it to
// stamp the initial chain; the seam is user-owned afterward, so editing this
// block does not retro-rewrite existing seams — it only changes what newly
// scaffolded packages start with.
type ObservabilityConfig struct {
	// LogLevel is the slog level at which the LogMiddleware records
	// SUCCESSFUL component calls ("debug" | "info" | "warn" | "error").
	// Failures always log at Error regardless. Default "debug" keeps
	// success logging quiet under a production Info handler. An unknown
	// value falls back to the default.
	LogLevel string `yaml:"log_level,omitempty"`
}

// SlogLevelExpr resolves LogLevel to the Go expression for the matching
// slog.Level constant, used to stamp the observe_chain.go seam. Unknown or
// empty values default to slog.LevelDebug (quiet-on-success).
func (o ObservabilityConfig) SlogLevelExpr() string {
	switch strings.ToLower(strings.TrimSpace(o.LogLevel)) {
	case "info":
		return "slog.LevelInfo"
	case "warn", "warning":
		return "slog.LevelWarn"
	case "error":
		return "slog.LevelError"
	default:
		return "slog.LevelDebug"
	}
}

type DocsConfig struct {
	Enabled            *bool    `yaml:"enabled,omitempty"`              // nil = true (enabled by default)
	OutputDir          string   `yaml:"output_dir,omitempty"`           // default: "docs/generated"
	Format             string   `yaml:"format,omitempty"`               // "markdown" (default) or "hugo"
	Generators         []string `yaml:"generators,omitempty"`           // e.g. ["api", "architecture", "config", "contracts"]
	CustomTemplatesDir string   `yaml:"custom_templates_dir,omitempty"` // user template overrides
}

// IsEnabled returns whether docs generation is enabled (default: true).
func (d DocsConfig) IsEnabled() bool {
	if d.Enabled == nil {
		return true
	}
	return *d.Enabled
}

// EffectiveOutputDir returns the output directory, defaulting to "docs/generated".
func (d DocsConfig) EffectiveOutputDir() string {
	if d.OutputDir == "" {
		return "docs/generated"
	}
	return d.OutputDir
}

// EffectiveFormat returns the output format, defaulting to "markdown".
func (d DocsConfig) EffectiveFormat() string {
	if d.Format == "" {
		return "markdown"
	}
	return d.Format
}
