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

import "strings"

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
	Kind       string `yaml:"kind,omitempty"`   // "service" (default), "cli", "library"
	Binary     string `yaml:"binary,omitempty"` // "per-service" (default), "shared" — one Go binary, cobra subcommand per service
	Version    string `yaml:"version"`
	// ForgeVersion records the forge binary version that this project's
	// generated artifacts were last produced against. It is set at
	// `forge new` time, bumped after a successful `forge upgrade`, and
	// consulted by `forge generate` to warn when the forge binary on
	// PATH has drifted from the version pinned by the project. Empty
	// (legacy) projects are treated as "0.0.0".
	ForgeVersion string           `yaml:"forge_version,omitempty"`
	HotReload    bool             `yaml:"hot_reload"`
	Services     []ServiceConfig  `yaml:"services"`
	Packages     []PackageConfig  `yaml:"packages,omitempty"`
	Frontends    []FrontendConfig `yaml:"frontends,omitempty"`
	// Frontend holds project-level frontend settings — distinct from
	// the per-frontend `Frontends []FrontendConfig` slice above. Today
	// it only carries the opt-in `workspaces:` flag that turns on the
	// pnpm-workspace + packages/api + packages/hooks layout so multiple
	// frontends (web + mobile) can share generated Connect clients and
	// React Query hook wrappers. When the flag is false (the default)
	// forge keeps the historic per-frontend layout exactly as before.
	Frontend  FrontendProjectConfig `yaml:"frontend,omitempty"`
	Database  DatabaseConfig        `yaml:"database"`
	CI        CIConfig              `yaml:"ci"`
	Deploy    DeployConfig          `yaml:"deploy,omitempty"`
	Docker    DockerConfig          `yaml:"docker"`
	K8s       K8sConfig             `yaml:"k8s"`
	Lint      LintConfig            `yaml:"lint"`
	Contracts ContractsConfig       `yaml:"contracts"`
	Auth      AuthConfig            `yaml:"auth"`
	Docs      DocsConfig            `yaml:"docs"`
	Features  FeaturesConfig        `yaml:"features,omitempty"`
	Stack     StackConfig           `yaml:"stack,omitempty"`
	// API toggles project-level API protocol skins layered on top of the
	// Connect mux. Default zero-value leaves both REST and OpenAPI off so
	// existing projects regenerate identically. See [APIConfig] for the
	// per-field semantics.
	API           APIConfig               `yaml:"api,omitempty"`
	Packs         []string                `yaml:"packs,omitempty"`
	PackOverrides map[string]PackOverride `yaml:"pack_overrides,omitempty"`
	// Binaries declares non-server long-running processes scaffolded
	// via `forge add binary <name>`. Each entry produces a `cmd/<name>.go`
	// cobra subcommand and an `internal/<name>/` package owning lifecycle
	// + business logic. Binaries are distinct from:
	//   - services (Connect-RPC servers wired through pkg/app/bootstrap.go)
	//   - workers   (in-process goroutines under the canonical server)
	//   - operators (controller-runtime managers with CRDs)
	// A binary is the right shape for: reverse proxies, sidecars, off-
	// service NATS consumers, gateways — anything that needs its own
	// Deployment but doesn't fit the server/worker/operator templates.
	Binaries []BinaryConfig `yaml:"binaries,omitempty"`
}

// BinaryConfig represents a non-server long-running binary scaffolded
// via `forge add binary <name>`. The shape mirrors ServiceConfig's
// declarative bits — name, path on disk — without the Connect-RPC
// fields (Type/Webhooks/CRDs/Group).
type BinaryConfig struct {
	// Name is the binary identifier in CLI / display form. May contain
	// hyphens; the Go-package form is derived via ServicePackageName.
	// Example: "workspace-proxy".
	Name string `yaml:"name"`
	// Path is the cobra subcommand source file relative to the project
	// root. By convention "cmd/<package>.go" where <package> is the
	// Go-package form of Name. Stored explicitly so future renames can
	// avoid breaking forge.yaml-driven tooling.
	Path string `yaml:"path"`
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

// ServiceConfig represents a Go service definition.
//
// Host vs cluster placement (was services[].dev_target):
//
// An earlier revision (commit cd25640) put per-service host/cluster
// placement on this struct. The decision moved to the KCL layer in the
// feat/kcl-orchestration batch: deployment target is an environment
// concern (which env runs this on the host, which arch, which runner),
// not a service-shape concern. Per-env placement is now declared in
// `deploy/kcl/<env>/main.k` via the [Service] schema's `deploy` field.
// See the `migration/dev-target-to-kcl-deploy` skill for the move.
type ServiceConfig struct {
	Name          string          `yaml:"name"`
	Type          string          `yaml:"type"`           // "go_service", "worker", "operator"
	Kind          string          `yaml:"kind,omitempty"` // sub-type: worker kind ("cron"), empty = default
	Path          string          `yaml:"path"`
	Port          int             `yaml:"port,omitempty"`
	Schedule      string          `yaml:"schedule,omitempty"` // cron expression for kind=cron workers
	ProtoPackages []string        `yaml:"proto_packages,omitempty"`
	Webhooks      []WebhookConfig `yaml:"webhooks,omitempty"`
	// Group is the API group for type=operator services. e.g.
	// "reliant.dev". Set when scaffolded via `forge add operator`.
	Group string `yaml:"group,omitempty"`
	// Version is the API version for type=operator services. e.g.
	// "v1alpha1". Set when scaffolded via `forge add operator`.
	Version string `yaml:"version,omitempty"`
	// CRDs lists the CRDs reconciled by this operator. Each entry is
	// a CRD added via `forge add crd <name>` and lives under
	// operators/<operator>/<crd-name>_controller.go plus
	// api/<version>/<crd-name>_types.go.
	CRDs []CRDConfig `yaml:"crds,omitempty"`
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
type DatabaseConfig struct {
	Driver          string                `yaml:"driver"` // "postgres", "sqlite"
	MigrationsDir   string                `yaml:"migrations_dir"`
	SQLCEnabled     bool                  `yaml:"sqlc_enabled"`
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
// starters, ci, docs, observability, ...). All fields are *bool so the
// loader can distinguish "absent" (nil → default ENABLED, preserving
// backward compatibility for existing projects without a features: block)
// from "explicitly false" (the user opted out). Explicit true and nil both
// resolve to enabled — kept as a permitted shape so a project can write
// `features: { deploy: true, ... }` for documentation / clarity without
// changing behavior.
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
	ORM           *bool `yaml:"orm,omitempty"`           // protoc-gen-forge-orm codegen
	Codegen       *bool `yaml:"codegen,omitempty"`       // service/handler codegen from protos
	Migrations    *bool `yaml:"migrations,omitempty"`    // auto-generate SQL migrations
	CI            *bool `yaml:"ci,omitempty"`            // generate CI/CD workflows
	Build         *bool `yaml:"build,omitempty"`         // `forge build` Go binary + docker image pipeline
	Deploy        *bool `yaml:"deploy,omitempty"`        // generate deploy manifests (KCL, Dockerfiles)
	Contracts     *bool `yaml:"contracts,omitempty"`     // contract linter enforcement
	Docs          *bool `yaml:"docs,omitempty"`          // documentation generation
	Frontend      *bool `yaml:"frontend,omitempty"`      // frontend scaffolding + codegen
	Observability *bool `yaml:"observability,omitempty"` // alloy, grafana dashboards, otel wiring
	HotReload     *bool `yaml:"hot_reload,omitempty"`    // air config generation
	Packs         *bool `yaml:"packs,omitempty"`         // forge packs (install/list/info), pack-generate hooks
	Starters      *bool `yaml:"starters,omitempty"`      // forge starters (one-time business-integration copies)

	// Diagnostics enables runtime emission of pkg/diagnostics records at
	// Bootstrap time — slog warn lines for every unwired scaffold the
	// codegen pipeline registered (Tier-1 stubs, nil-wired Deps fields).
	// Default OFF: existing projects don't suddenly start logging warns on
	// regen. Opt-in by setting `features.diagnostics: true` in forge.yaml.
	Diagnostics *bool `yaml:"diagnostics,omitempty"`

	// StrictWiring upgrades the Diagnostics emitter to StrictEmitter, so
	// any registered diagnostic terminates the process after the summary
	// line. Implies Diagnostics: true at the wire site — production-grade
	// projects use this to fail-fast in CI. Default OFF.
	StrictWiring *bool `yaml:"strict_wiring,omitempty"`
}

// featureEnabled returns true if the *bool is nil (default) or explicitly true.
func featureEnabled(b *bool) bool {
	return b == nil || *b
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
func (f FeaturesConfig) ORMEnabled() bool { return featureEnabled(f.ORM) }

// CodegenEnabled reports whether codegen is on (default: on).
func (f FeaturesConfig) CodegenEnabled() bool { return featureEnabled(f.Codegen) }

// MigrationsEnabled reports whether the migrations feature is on (default: on).
func (f FeaturesConfig) MigrationsEnabled() bool { return featureEnabled(f.Migrations) }

// CIEnabled reports whether the CI feature is on (default: on).
func (f FeaturesConfig) CIEnabled() bool { return featureEnabled(f.CI) }

// DeployEnabled reports whether the deploy feature is on (default: on).
func (f FeaturesConfig) DeployEnabled() bool { return featureEnabled(f.Deploy) }

// ContractsEnabled reports whether contract enforcement is on (default: on).
func (f FeaturesConfig) ContractsEnabled() bool { return featureEnabled(f.Contracts) }

// DocsEnabled reports whether the docs feature is on (default: on).
func (f FeaturesConfig) DocsEnabled() bool { return featureEnabled(f.Docs) }

// FrontendEnabled reports whether the frontend feature is on (default: on).
func (f FeaturesConfig) FrontendEnabled() bool { return featureEnabled(f.Frontend) }

// ObservabilityEnabled reports whether the observability feature is on (default: on).
func (f FeaturesConfig) ObservabilityEnabled() bool { return featureEnabled(f.Observability) }

// HotReloadEnabled reports whether the hot-reload feature is on (default: on).
func (f FeaturesConfig) HotReloadEnabled() bool { return featureEnabled(f.HotReload) }

// BuildEnabled reports whether `forge build` is enabled (default: on).
// Direct `forge build` invocations error when off; orchestrators like
// `forge up` log a skip line and continue.
func (f FeaturesConfig) BuildEnabled() bool { return featureEnabled(f.Build) }

// PacksEnabled reports whether the pack subsystem is enabled (default: on).
// Disables `forge pack list/info/install/remove` and skips the pack
// generate-hooks step in the codegen pipeline.
func (f FeaturesConfig) PacksEnabled() bool { return featureEnabled(f.Packs) }

// StartersEnabled reports whether the starter subsystem is enabled
// (default: on). Disables `forge starter list/add`.
func (f FeaturesConfig) StartersEnabled() bool { return featureEnabled(f.Starters) }

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
	return "feature '" + e.name + "' is disabled in forge.yaml. Set features." + e.name + ": true to enable."
}

// FeatureName is the canonical feature key. Stays a string alias so the
// constants below are usable directly anywhere the feature name shows up
// as a config key, a `--disable` flag value, or a `forge audit` field.
type FeatureName = string

// Feature name constants. These are the wire format — both YAML field
// names under `features:` and the strings emitted by `forge audit
// --json | jq '.features'`. Kept exported so external tooling can match
// against them without re-encoding the spelling.
const (
	FeatureORM           FeatureName = "orm"
	FeatureCodegen       FeatureName = "codegen"
	FeatureMigrations    FeatureName = "migrations"
	FeatureCI            FeatureName = "ci"
	FeatureBuild         FeatureName = "build"
	FeatureDeploy        FeatureName = "deploy"
	FeatureContracts     FeatureName = "contracts"
	FeatureDocs          FeatureName = "docs"
	FeatureFrontend      FeatureName = "frontend"
	FeatureObservability FeatureName = "observability"
	FeatureHotReload     FeatureName = "hot_reload"
	FeaturePacks         FeatureName = "packs"
	FeatureStarters      FeatureName = "starters"
)

// EffectiveFeatures projects the resolved enabled/disabled state of
// every feature into a stable name→bool map. Used by `forge audit` to
// surface the project's feature configuration at a glance, and by tests
// to assert per-kind scaffold defaults. The map is keyed by Feature*
// constants and is safe to JSON-marshal directly.
func (f FeaturesConfig) EffectiveFeatures() map[string]bool {
	return map[string]bool{
		FeatureORM:           f.ORMEnabled(),
		FeatureCodegen:       f.CodegenEnabled(),
		FeatureMigrations:    f.MigrationsEnabled(),
		FeatureCI:            f.CIEnabled(),
		FeatureBuild:         f.BuildEnabled(),
		FeatureDeploy:        f.DeployEnabled(),
		FeatureContracts:     f.ContractsEnabled(),
		FeatureDocs:          f.DocsEnabled(),
		FeatureFrontend:      f.FrontendEnabled(),
		FeatureObservability: f.ObservabilityEnabled(),
		FeatureHotReload:     f.HotReloadEnabled(),
		FeaturePacks:         f.PacksEnabled(),
		FeatureStarters:      f.StartersEnabled(),
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
	if f.StrictWiring != nil && *f.StrictWiring {
		return true
	}
	return f.Diagnostics != nil && *f.Diagnostics
}

// StrictWiringEnabled reports whether the diagnostics strict-mode
// exit is wired by bootstrap (default: OFF). Used in tandem with
// DiagnosticsEnabled — strict-mode wraps the LogEmitter with
// StrictEmitter so any registered diagnostic terminates the process
// after the summary line.
func (f FeaturesConfig) StrictWiringEnabled() bool {
	return f.StrictWiring != nil && *f.StrictWiring
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
	Driver string `yaml:"driver,omitempty"` // "postgres" (default), "sqlite", "mysql", "none"
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

// EffectiveDatabaseDriver returns the database driver, defaulting to "postgres".
func (s StackConfig) EffectiveDatabaseDriver() string {
	if s.Database.Driver != "" {
		return s.Database.Driver
	}
	return "postgres"
}

// IsProtoEnabled returns whether the proto toolchain is enabled (default: true).
func (s StackConfig) IsProtoEnabled() bool {
	return featureEnabled(s.Proto.Enabled)
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
