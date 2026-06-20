// Package config defines the canonical forge.yaml types shared by
// both the CLI (read) and the generator (write) packages. The
// forge.yaml schema deliberately exposes ~40 typed sections so YAML
// unmarshal can hydrate each block; splitting this file would just
// scatter the same surface across multiple packages and break the
// "one schema, one file" contract the generator relies on. The
// max-public-structs revive rule is therefore suppressed at the
// package-doc line below.

//nolint:revive // max-public-structs: see package doc above.
package config

import (
	"strings"

	"go.yaml.in/yaml/v3"
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
// Applications. See FORGE_BACKLOG.md "Layer B" + the
// migrations/v0.x-to-binary-shared/ skill for tradeoffs.
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
	// Kind is the project shape (service|cli|library). It is NO LONGER a
	// forge.yaml field: as of the ProjectStore Phase-2 data move it DERIVES
	// from the components (see DeriveProjectKind) — a project with a
	// server-shaped component is a service, a binary-only project is a cli,
	// an empty one is a library. The loader sets this field after reading
	// components.json; the yaml tag is "-" so a stale `kind:` in forge.yaml
	// is rejected with a migration hint. Every consumer that reads cfg.Kind
	// is unchanged.
	Kind    string `yaml:"-"`
	Binary  string `yaml:"binary,omitempty"` // "per-service" (default), "shared" — one Go binary, cobra subcommand per service
	Version string `yaml:"version,omitempty"`
	// ForgeVersion records the forge binary version that this project's
	// generated artifacts were last produced against. It is set at
	// `forge new` time, bumped after a successful `forge upgrade`, and
	// consulted by `forge generate` to warn when the forge binary on
	// PATH has drifted from the version pinned by the project. Empty
	// (legacy) projects are treated as "0.0.0".
	ForgeVersion string `yaml:"forge_version,omitempty"`
	// HotReload toggles the air-based hot-reload dev loop for `forge run`.
	// *bool so "absent" (nil → derived: on for service kind, off otherwise)
	// is distinguishable from an explicit `hot_reload: false` opt-out. Use
	// EffectiveHotReload; don't read the pointer directly.
	HotReload *bool `yaml:"hot_reload,omitempty"`
	// Components is the unified list of everything this project builds and
	// runs: Connect-RPC servers, in-process workers, scheduled crons,
	// controller-runtime operators, and standalone binaries. The Kind field
	// on each entry is THE discriminator (server|worker|cron|operator|binary).
	//
	// As of the ProjectStore Phase-2 per-service data move, components are
	// AUTHORED in the project-root components.json file (see
	// [ComponentsFileName]) — NOT in forge.yaml. The yaml tag is therefore
	// "-": forge.yaml can no longer carry a `components:` block (a stale one
	// is rejected with a migration hint — see removedSchemaKeys). The loader
	// reads components.json and populates this field, so every consumer that
	// reads cfg.Components (and the ProjectStore wrapping it) is unchanged.
	Components []ComponentConfig `yaml:"-"`
	Packages   []PackageConfig   `yaml:"packages,omitempty"`
	Frontends  []FrontendConfig  `yaml:"frontends,omitempty"`
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
	Database  DatabaseConfig  `yaml:"database,omitempty"`
	CI        CIConfig        `yaml:"ci,omitempty"`
	Build     BuildConfig     `yaml:"build,omitempty"`
	Deploy    DeployConfig    `yaml:"deploy,omitempty"`
	Docker    DockerConfig    `yaml:"docker,omitempty"`
	K8s       K8sConfig       `yaml:"k8s,omitempty"`
	Lint      LintConfig      `yaml:"lint,omitempty"`
	Contracts ContractsConfig `yaml:"contracts,omitempty"`
	Auth      AuthConfig      `yaml:"auth,omitempty"`
	Docs      DocsConfig      `yaml:"docs,omitempty"`
	Features  FeaturesConfig  `yaml:"features,omitempty"`
	Stack     StackConfig     `yaml:"stack,omitempty"`
	// API toggles project-level API protocol skins layered on top of the
	// Connect mux. Default zero-value leaves both REST and OpenAPI off so
	// existing projects regenerate identically. See [APIConfig] for the
	// per-field semantics.
	API           APIConfig               `yaml:"api,omitempty"`
	Packs         []string                `yaml:"packs,omitempty"`
	PackOverrides map[string]PackOverride `yaml:"pack_overrides,omitempty"`
}

// PackOverride is a project-level override block for an installed pack,
// keyed by pack name under `pack_overrides:` in forge.yaml. It lets the
// project decline pack-shipped artifacts when its own code already
// supersedes them — e.g. an audit-log pack ships migrations the project
// has already authored under different names.
type PackOverride struct {
	// SkipMigrations skips rendering the pack's `migrations:` block at
	// install time. Useful when the project's own migrations supersede
	// the pack's (typical during forge migrations of an existing repo
	// where the schema is already in place).
	SkipMigrations bool `yaml:"skip_migrations,omitempty"`
}

// Component kind constants. Kind is the single discriminator on a
// [ComponentConfig] — it replaces the old `services[].type` +
// `services[].kind` pair and the separate `binaries:` block.
//
//   - server   — Connect-RPC handlers + authorizer + client + frontend
//     hooks + bootstrap row + cobra subcommand (was type=go_service).
//   - worker   — in-process ContextWorker goroutine; bootstrap Workers row.
//   - cron     — scheduled job; Schedule drives it. In-process scheduled
//     goroutine for dev, CronJob in deploy. First-class (was
//     worker + kind:cron).
//   - operator — controller-runtime manager + CRDs.
//   - binary   — standalone cobra subcommand cmd/<name>.go (one image,
//     run `./app <name>`); no bootstrap wiring (was the binaries: block).
const (
	ComponentKindServer   = "server"
	ComponentKindWorker   = "worker"
	ComponentKindCron     = "cron"
	ComponentKindOperator = "operator"
	ComponentKindBinary   = "binary"
)

// ComponentConfig represents one buildable/runnable unit of a forge
// project. Its Kind field selects which scaffold + deploy treatment the
// component receives; see the ComponentKind* constants.
//
// Host vs cluster placement (was services[].dev_target):
//
// An earlier revision (commit cd25640) put per-component host/cluster
// placement on this struct. The decision moved to the KCL layer in the
// feat/kcl-orchestration batch: deployment target is an environment
// concern (which env runs this on the host, which arch, which runner),
// not a component-shape concern. Per-env placement is now declared in
// `deploy/kcl/<env>/main.k`.
type ComponentConfig struct {
	Name string `yaml:"name"`
	// Kind is THE discriminator: server|worker|cron|operator|binary.
	// See the ComponentKind* constants.
	Kind string `yaml:"kind"`
	Path string `yaml:"path"`
	// Ports is a named port map (http/grpc/metrics/proxy/…). Each entry
	// unmarshals from EITHER a scalar int (`http: 8080`) or a struct
	// (`http: {port: 8080, protocol: tcp, expose: true}`). Consumers
	// reference ports BY NAME; a server's primary HTTP port is ports.http.
	Ports         map[string]PortSpec `yaml:"ports,omitempty"`
	Schedule      string              `yaml:"schedule,omitempty"` // cron expression for kind=cron
	ProtoPackages []string            `yaml:"proto_packages,omitempty"`
	Webhooks      []WebhookConfig     `yaml:"webhooks,omitempty"`
	// Group is the API group for kind=operator components. e.g.
	// "reliant.dev". Set when scaffolded via `forge add operator`.
	Group string `yaml:"group,omitempty"`
	// Version is the API version for kind=operator components. e.g.
	// "v1alpha1". Set when scaffolded via `forge add operator`.
	Version string `yaml:"version,omitempty"`
	// CRDs lists the CRDs reconciled by this operator. Each entry is
	// a CRD added via `forge add crd <name>` and lives under
	// operators/<operator>/<crd-name>_controller.go plus
	// api/<version>/<crd-name>_types.go.
	CRDs []CRDConfig `yaml:"crds,omitempty"`
}

// HTTPPortName is the conventional name for a component's primary HTTP
// port in the Ports map. Server components serve their Connect mux here.
const HTTPPortName = "http"

// PortSpec describes one named port. It unmarshals from EITHER a YAML
// scalar int — `http: 8080` (the common case, Protocol/Expose default) —
// OR a struct — `http: {port: 8080, protocol: tcp, expose: true}`. See
// UnmarshalYAML.
type PortSpec struct {
	Port     int    `yaml:"port"`
	Protocol string `yaml:"protocol,omitempty"` // tcp (default), udp
	Expose   bool   `yaml:"expose,omitempty"`   // surface on the k8s Service / Dockerfile EXPOSE
}

// UnmarshalYAML accepts a bare scalar int (`http: 8080`) or a full
// mapping (`http: {port: 8080, protocol: tcp, expose: true}`). The
// scalar form is sugar for `{port: N}` with default protocol/expose so
// the common single-port case stays terse.
func (p *PortSpec) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var n int
		if err := value.Decode(&n); err != nil {
			return err
		}
		p.Port = n
		return nil
	}
	// Avoid infinite recursion: decode into a struct alias without the
	// custom UnmarshalYAML method.
	type rawPortSpec PortSpec
	var raw rawPortSpec
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*p = PortSpec(raw)
	return nil
}

// MarshalYAML emits the terse scalar form (`http: 8080`) when protocol
// and expose are at their defaults, and the full mapping otherwise. This
// keeps a freshly-scaffolded forge.yaml's ports: block as terse as the
// single-port common case allows, while round-tripping the struct form
// for ports that set protocol/expose.
func (p PortSpec) MarshalYAML() (any, error) {
	if p.Protocol == "" && !p.Expose {
		return p.Port, nil
	}
	type rawPortSpec PortSpec
	return rawPortSpec(p), nil
}

// PrimaryPort returns the component's primary HTTP port number (the
// ports.http entry), or 0 when no http port is declared. This is the
// port a server serves its Connect mux on and the one most consumers
// (dev loop, frontend nav, readiness) want.
func (c ComponentConfig) PrimaryPort() int {
	if c.Ports == nil {
		return 0
	}
	return c.Ports[HTTPPortName].Port
}

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

// IsOperator reports whether the component is a controller-runtime operator.
func (c ComponentConfig) IsOperator() bool { return c.EffectiveKind() == ComponentKindOperator }

// IsBinary reports whether the component is a standalone binary subcommand.
func (c ComponentConfig) IsBinary() bool { return c.EffectiveKind() == ComponentKindBinary }

// Servers returns the server-kind components — the Connect-RPC surfaces
// that get handlers, the served-set registration, and frontend hooks.
func (c ProjectConfig) Servers() []ComponentConfig {
	return c.componentsOfKind(ComponentKindServer)
}

// Workers returns the worker-kind components.
func (c ProjectConfig) Workers() []ComponentConfig {
	return c.componentsOfKind(ComponentKindWorker)
}

// Crons returns the cron-kind components.
func (c ProjectConfig) Crons() []ComponentConfig {
	return c.componentsOfKind(ComponentKindCron)
}

// Operators returns the operator-kind components.
func (c ProjectConfig) Operators() []ComponentConfig {
	return c.componentsOfKind(ComponentKindOperator)
}

// BinaryComponents returns the binary-kind components.
func (c ProjectConfig) BinaryComponents() []ComponentConfig {
	return c.componentsOfKind(ComponentKindBinary)
}

func (c ProjectConfig) componentsOfKind(kind string) []ComponentConfig {
	var out []ComponentConfig
	for _, comp := range c.Components {
		if comp.EffectiveKind() == kind {
			out = append(out, comp)
		}
	}
	return out
}

// CRDConfig represents a single Custom Resource Definition reconciled
// by an operator. CRDs are scaffolded via `forge add crd <name> --operator <op>`.
type CRDConfig struct {
	// Name is the PascalCase CRD type name. e.g. "Workspace".
	Name string `yaml:"name"`
	// Group is the API group, defaulting to the parent operator's
	// Group. Stored explicitly so a single operator can manage CRDs
	// from multiple groups.
	Group string `yaml:"group,omitempty"`
	// Version is the API version. Defaults to the parent operator's
	// Version.
	Version string `yaml:"version,omitempty"`
	// Shape is the reconciler scaffold style. One of
	// "state-machine" (phase-driven), "config" (declarative-only,
	// no state), "composite" (manages sub-resources). Drives which
	// template is rendered for the controller shim.
	Shape string `yaml:"shape,omitempty"`
}

// WebhookConfig represents a webhook endpoint within a service.
type WebhookConfig struct {
	Name string `yaml:"name"` // e.g. "stripe", "github"
}

// PackageConfig represents an internal package with a Go interface contract.
type PackageConfig struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind,omitempty"` // "" (default/generic), "client", "eventbus"
	// Type captures the hexagonal-architecture role chosen at scaffold
	// time: "service" (default — bootstrap-wired contract package),
	// "adapter" (outbound boundary, marked `// forge:adapter`),
	// "interactor" (use-case orchestrator, marked `// forge:interactor`).
	// Empty (omitted) is treated as "service" for backward compatibility
	// with packages scaffolded before the --type flag landed.
	Type string `yaml:"type,omitempty"`
}

// FrontendConfig defines a frontend application (e.g. Next.js, React Native).
type FrontendConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`           // "nextjs", "react-native", "vite-spa"
	Kind string `yaml:"kind,omitempty"` // "web" (default/Next.js), "mobile" (React Native), "vite-spa" (Vite + React + tanstack-router)
	Path string `yaml:"path"`
	Port int    `yaml:"port"`
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
	// Shape rules (validated by `forge validate` / LoadStrict):
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

// CIConfig holds CI/CD settings.
type CIConfig struct {
	Provider    string       `yaml:"provider"`             // "github" (default)
	GoVersion   string       `yaml:"go_version,omitempty"` // e.g. "1.26"
	Lint        CILintConfig `yaml:"lint,omitempty"`
	Test        CITestConfig `yaml:"test,omitempty"`
	VulnScan    CIVulnConfig `yaml:"vuln_scan,omitempty"`
	E2E         CIE2EConfig  `yaml:"e2e,omitempty"`
	Permissions CIPermConfig `yaml:"permissions,omitempty"`
	ExtraJobs   []CIExtraJob `yaml:"extra_jobs,omitempty"` // user extension point
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

// CIExtraJob defines a user-provided additional CI job.
type CIExtraJob struct {
	Name   string           `yaml:"name"`
	RunsOn string           `yaml:"runs_on,omitempty"` // default "ubuntu-latest"
	Steps  []CIExtraJobStep `yaml:"steps"`
}

// CIExtraJobStep defines a single step within a CIExtraJob.
type CIExtraJobStep struct {
	Name string            `yaml:"name,omitempty"`
	Uses string            `yaml:"uses,omitempty"`
	Run  string            `yaml:"run,omitempty"`
	With map[string]string `yaml:"with,omitempty"`
}

// EffectiveGoVersion returns the Go version for CI, defaulting to "1.26".
func (c *CIConfig) EffectiveGoVersion() string {
	if c.GoVersion != "" {
		return c.GoVersion
	}
	return "1.26"
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

// EffectiveRunsOn returns the runner for an extra job, defaulting to "ubuntu-latest".
func (j *CIExtraJob) EffectiveRunsOn() string {
	if j.RunsOn != "" {
		return j.RunsOn
	}
	return "ubuntu-latest"
}

// DeployConfig holds deployment pipeline settings.
type DeployConfig struct {
	Provider       string            `yaml:"provider"`           // "github"
	Registry       string            `yaml:"registry,omitempty"` // "ghcr" (default), "gar", "ecr"
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

// BuildConfig controls build-time version stamping. Empty = forge's
// defaults (version derived from git, stamped into main.version).
type BuildConfig struct {
	// Version pins the embedded build version, overriding git derivation.
	Version string `yaml:"version,omitempty"`
	// VersionVar is an ADDITIONAL `-ldflags -X` target stamped with the
	// resolved version, e.g.
	//   "github.com/acme/app/internal/buildinfo.Version"
	// main.version/commit/date are ALWAYS stamped; this is extra, for
	// code that can't import package main. Requires an exported pkg-level
	// string var at that path.
	VersionVar string `yaml:"version_var,omitempty"`
}

// DockerConfig holds Docker registry configuration.
type DockerConfig struct {
	Registry   string            `yaml:"registry"`
	BaseImages map[string]string `yaml:"base_images,omitempty"`
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

// LintConfig holds lint-related settings.
type LintConfig struct {
	Contract bool               `yaml:"contract"`
	Frontend FrontendLintConfig `yaml:"frontend,omitempty"`
	// HandlerFileMaxLOC is the per-file LOC threshold above which the
	// forgeconv-handler-file-size analyzer warns. Counts non-blank, non-
	// comment Go source lines under handlers/<svc>/*.go. A value of 0 (or
	// the field unset) is treated as the built-in default — see
	// [LintConfig.EffectiveHandlerFileMaxLOC] for the canonical value.
	HandlerFileMaxLOC int `yaml:"handler_file_max_loc,omitempty"`
}

// DefaultHandlerFileMaxLOC is the built-in threshold used by the
// forgeconv-handler-file-size analyzer when the project does not set
// lint.handler_file_max_loc in forge.yaml. Picked at 1000 because that
// roughly tracks "two screens of any modern editor" plus generous
// buffer — files past that point materially harm review velocity and
// almost always benefit from the per-RPC split that `forge add
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
// severity ("error"/"warn"/"off") the `no-important` and
// `no-inline-styles` rules use.
type FrontendLintConfig struct {
	CSSHealth      bool   `yaml:"css_health,omitempty"`       // enable stylelint-backed CSS health checks
	NoImportant    string `yaml:"no_important,omitempty"`     // error, warn, off
	NoInlineStyles string `yaml:"no_inline_styles,omitempty"` // error, warn, off
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
type ContractsConfig struct {
	Strict             bool     `yaml:"strict"`               // require contract.go for all internal packages with exported methods (default: true)
	AllowExportedVars  bool     `yaml:"allow_exported_vars"`  // allow exported package vars (default: false)
	AllowExportedFuncs bool     `yaml:"allow_exported_funcs"` // allow exported funcs without contract (default: true)
	Exclude            []string `yaml:"exclude"`              // packages that opt out
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

// IsStrict returns whether strict contract enforcement is enabled (default: true).
// When the config is zero-value (not explicitly set), strict defaults to true.
func (c ContractsConfig) IsStrict() bool {
	if !c.Strict && !c.AllowExportedVars && !c.AllowExportedFuncs && len(c.Exclude) == 0 {
		// Zero-value config — default to strict=true.
		return true
	}
	return c.Strict
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

// FeaturesConfig controls which forge features are active. The `features:`
// block in forge.yaml gates major subsystems (deploy, build, frontend, packs,
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
//   - Implicit invocations from orchestrators (e.g. `forge up` driving
//     the build/deploy/frontend phases) log a skip line and continue —
//     letting `forge up` succeed on whatever subsystems ARE enabled.
//   - Codegen pipeline steps gated on a feature skip silently when off,
//     mirroring the existing gate function shape under
//     internal/cli/generate_pipeline.go.
//
// New project scaffolding (`forge new --kind`) sets defaults per kind:
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
	Packs         *bool `yaml:"packs,omitempty"`         // forge packs (install/list/info), pack-generate hooks
	Deploy        *bool `yaml:"deploy,omitempty"`        // deploy pipeline: KCL render → kubectl apply, per-env deploy config codegen

	// Diagnostics enables runtime emission of pkg/diagnostics records at
	// Bootstrap time — slog warn lines for every unwired scaffold the
	// codegen pipeline registered (Tier-1 stubs, nil-wired Deps fields).
	// Default OFF: existing projects don't suddenly start logging warns on
	// regen. Opt-in by setting `features.diagnostics: true` in forge.yaml.
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
		FeaturePacks:         &f.Packs,
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
		f.HotReload == nil && f.Packs == nil &&
		f.Deploy == nil &&
		f.Diagnostics == nil && f.Experimental == (ExperimentalConfig{})
}

// ExperimentalConfig gates features that are not yet promised. Fields
// are plain bool (not *bool) — the zero value IS the default, and the
// default IS off. Loud-warning policy on startup when any field is true.
//
// What lives here today:
//
//   - Ingress:        Gateway API codegen + cert-manager + Traefik
//     wiring. Provider matrix is fragile and not yet
//     proven across real cloud providers.
//   - ExternalBuilds: RETIRED gate (kept as an accepted, inert key for
//     back-compat). `Service.build_cmd` is the build-side
//     mirror of `External.deploy_cmd`; since `forge deploy`
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
// nudge the user toward `forge upgrade`.
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
// `forge up` log a skip line and continue.
func (f FeaturesConfig) BuildEnabled() bool { return f.resolve(FeatureBuild) }

// PacksEnabled reports whether the pack subsystem is enabled (default: on).
// Disables `forge pack list/info/install/remove` and skips the pack
// generate-hooks step in the codegen pipeline.
func (f FeaturesConfig) PacksEnabled() bool { return f.resolve(FeaturePacks) }

// IngressEnabled reports whether Gateway API ingress is wired
// (default: OFF — opt-in under `features.experimental.ingress: true`).
// When off, forge skips ingress codegen, `forge cluster up` skips
// the Traefik + GatewayClass install, `forge cluster urls` returns nothing,
// and the audit ingress category is suppressed.
func (f FeaturesConfig) IngressEnabled() bool { return f.Experimental.Ingress }

// ExternalBuildsEnabled reports the raw value of the RETIRED
// `features.experimental.external_builds` flag. It no longer gates the
// build path: `build_cmd` is the build-side mirror of `External.deploy_cmd`
// (which needs no opt-in), so `forge build` of a build_cmd service runs
// unconditionally (fr-da9a6614fb). The accessor is retained for the
// startup warning / `forge audit` surface and any consumer still keyed off
// the flag; the build dispatcher in internal/cli/build.go no longer calls
// it.
func (f FeaturesConfig) ExternalBuildsEnabled() bool { return f.Experimental.ExternalBuilds }

// OperatorsEnabled reports whether controller-runtime operator codegen
// + CRD manifest generation is wired (default: OFF — opt-in under
// `features.experimental.operators: true`). When off, the operator
// binary codegen + CRD scaffold steps skip silently and
// `forge add operator` errors.
func (f FeaturesConfig) OperatorsEnabled() bool { return f.Experimental.Operators }

// DisabledFeatureError returns the canonical user-facing error for a
// disabled feature. Centralised so every gate site emits the same
// wording — sub-agents and humans grepping for the string find one
// authoritative format. The name argument is the lowercased feature
// name as it appears in forge.yaml (e.g. "deploy", "build", "packs").
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
// as a config key, a `--disable` flag value, or a `forge audit` field.
type FeatureName = string

// Feature name constants. These are the wire format — both YAML field
// names under `features:` (top-level) or `features.experimental:`
// (nested) and the strings emitted by `forge audit --json | jq
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
	FeaturePacks         FeatureName = "packs"
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
// order used by `forge audit`, the startup warning, and `forge features`.
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
// Used by the startup warning and `forge features`.
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
// every feature into a stable name→bool map. Used by `forge audit` to
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
		FeaturePacks:          f.PacksEnabled(),
		FeatureDeploy:         f.DeployEnabled(),
		FeatureIngress:        f.IngressEnabled(),
		FeatureExternalBuilds: f.ExternalBuildsEnabled(),
		FeatureOperators:      f.OperatorsEnabled(),
		FeatureStrictWiring:   f.StrictWiringEnabled(),
	}
}

// DiagnosticsEnabled reports whether the pkg/diagnostics runtime emit
// is wired by bootstrap (default: OFF). When OFF, codegen still emits
// pkg/app/diagnostics_gen.go (so `forge audit` can roll the data up
// from the file), but Bootstrap does not call diagnostics.Default.Boot
// — no slog lines, no strict-mode exit.
//
// Strict-wiring implies Diagnostics: enabling strict without diagnostics
// is a no-op, so we treat StrictWiringEnabled as forcing diagnostics on.
func (f FeaturesConfig) DiagnosticsEnabled() bool {
	if f.Experimental.StrictWiring {
		return true
	}
	return f.Diagnostics != nil && *f.Diagnostics
}

// StrictWiringEnabled reports whether the diagnostics strict-mode
// exit is wired by bootstrap (default: OFF — opt-in under
// `features.experimental.strict_wiring: true`). Used in tandem with
// DiagnosticsEnabled — strict-mode wraps the LogEmitter with
// StrictEmitter so any registered diagnostic terminates the process
// after the summary line.
func (f FeaturesConfig) StrictWiringEnabled() bool {
	return f.Experimental.StrictWiring
}

// StackConfig declares the technology choices for the project.
// These are forward-looking declarations — forge may not support all
// values yet, but they document intent and guide future codegen.
type StackConfig struct {
	Backend  StackBackend  `yaml:"backend,omitempty"`
	Frontend StackFrontend `yaml:"frontend,omitempty"`
	Database StackDatabase `yaml:"database,omitempty"`
	Proto    StackProto    `yaml:"proto,omitempty"`
	Deploy   StackDeploy   `yaml:"deploy,omitempty"`
	CI       StackCI       `yaml:"ci,omitempty"`
}

// StackBackend declares the backend language and framework.
type StackBackend struct {
	Language  string `yaml:"language,omitempty"`  // "go" (default), "python", "rust", "typescript"
	Framework string `yaml:"framework,omitempty"` // future: "gin", "fiber", etc.
}

// StackFrontend declares the frontend framework.
type StackFrontend struct {
	Framework string `yaml:"framework,omitempty"` // "nextjs" (default), "react-native", "svelte", "none"
}

// StackDatabase declares the database technology.
type StackDatabase struct {
	Driver string `yaml:"driver,omitempty"` // "postgres" (default) or "none"
}

// StackProto declares the proto toolchain.
type StackProto struct {
	Enabled  *bool  `yaml:"enabled,omitempty"`  // nil = true
	Provider string `yaml:"provider,omitempty"` // "buf" (default), "protoc"
}

// StackDeploy declares the deployment target.
type StackDeploy struct {
	Target   string `yaml:"target,omitempty"`   // "k8s" (default), "docker-compose", "fly", "cloudrun", "lambda", "none"
	Provider string `yaml:"provider,omitempty"` // "k3d", "gke", "eks"
	Registry string `yaml:"registry,omitempty"` // "ghcr.io", "gcr.io", etc.
}

// StackCI declares the CI/CD provider.
type StackCI struct {
	Provider string `yaml:"provider,omitempty"` // "github" (default), "gitlab", "circleci", "none"
}

// EffectiveBackendLanguage returns the backend language, defaulting to "go".
func (s StackConfig) EffectiveBackendLanguage() string {
	if s.Backend.Language != "" {
		return s.Backend.Language
	}
	return "go"
}

// EffectiveFrontendFramework returns the frontend framework, defaulting to "nextjs".
func (s StackConfig) EffectiveFrontendFramework() string {
	if s.Frontend.Framework != "" {
		return s.Frontend.Framework
	}
	return "nextjs"
}

// EffectiveDatabaseDriver returns the database driver. The driver is
// pinned to postgres: the only off-ramp is "none" (no database). Any
// other value (including the empty default) resolves to "postgres".
func (s StackConfig) EffectiveDatabaseDriver() string {
	if s.Database.Driver == "none" {
		return "none"
	}
	return "postgres"
}

// IsProtoEnabled returns whether the proto toolchain is enabled (default: true).
func (s StackConfig) IsProtoEnabled() bool {
	return s.Proto.Enabled == nil || *s.Proto.Enabled
}

// EffectiveProtoProvider returns the proto provider, defaulting to "buf".
func (s StackConfig) EffectiveProtoProvider() string {
	if s.Proto.Provider != "" {
		return s.Proto.Provider
	}
	return "buf"
}

// EffectiveDeployTarget returns the deploy target, defaulting to "k8s".
func (s StackConfig) EffectiveDeployTarget() string {
	if s.Deploy.Target != "" {
		return s.Deploy.Target
	}
	return "k8s"
}

// EffectiveCIProvider returns the CI provider, defaulting to "github".
func (s StackConfig) EffectiveCIProvider() string {
	if s.CI.Provider != "" {
		return s.CI.Provider
	}
	return "github"
}

// AuthConfig holds authentication provider settings.
type AuthConfig struct {
	Provider    string             `yaml:"provider"` // "jwt", "api_key", "both", "none"
	JWT         JWTConfig          `yaml:"jwt,omitempty"`
	APIKey      APIKeyConfig       `yaml:"api_key,omitempty"`
	MultiTenant *MultiTenantConfig `yaml:"multi_tenant,omitempty"`
}

// MultiTenantConfig holds multi-tenancy settings for row-level tenant isolation.
type MultiTenantConfig struct {
	Enabled    bool   `yaml:"enabled"`
	ClaimField string `yaml:"claim_field,omitempty"` // JWT claim to extract tenant ID from, default: "org_id"
	ColumnName string `yaml:"column_name,omitempty"` // DB column name for tenant scoping, default: "org_id"
}

// EffectiveClaimField returns the claim field, defaulting to "org_id".
func (m MultiTenantConfig) EffectiveClaimField() string {
	if m.ClaimField == "" {
		return "org_id"
	}
	return m.ClaimField
}

// EffectiveColumnName returns the column name, defaulting to "org_id".
func (m MultiTenantConfig) EffectiveColumnName() string {
	if m.ColumnName == "" {
		return "org_id"
	}
	return m.ColumnName
}

// JWTConfig holds JWT-specific authentication settings.
type JWTConfig struct {
	Issuer        string `yaml:"issuer,omitempty"`
	Audience      string `yaml:"audience,omitempty"`
	JWKSURL       string `yaml:"jwks_url,omitempty"`
	SigningMethod string `yaml:"signing_method,omitempty"` // HS256, RS256, ES256
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

// APIKeyConfig holds API key authentication settings.
type APIKeyConfig struct {
	Header string `yaml:"header,omitempty"` // default: "X-API-Key"
}

// EffectiveAPIKeyHeader returns the API key header, defaulting to "X-API-Key".
func (a APIKeyConfig) EffectiveAPIKeyHeader() string {
	if a.Header == "" {
		return "X-API-Key"
	}
	return a.Header
}

// EffectiveSigningMethod returns the JWT signing method, defaulting to "RS256".
func (j JWTConfig) EffectiveSigningMethod() string {
	if j.SigningMethod == "" {
		return "RS256"
	}
	return j.SigningMethod
}

// K8sConfig holds Kubernetes configuration.
type K8sConfig struct {
	KCLDir string `yaml:"kcl_dir"`
}

// DocsConfig holds documentation generation settings.
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
