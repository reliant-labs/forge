// Package projectstore is the single read+mutate surface for a forge
// project's state. Every consumer outside the config loader reads project
// metadata, components, and feature flags through a [ProjectStore] rather
// than touching *config.ProjectConfig directly.
//
// Why the indirection: the project + component + feature state is the part
// of forge.yaml that a future revision (the "Phase 2" source swap) wants to
// relocate — out of the hand-edited forge.yaml and into a denormalized,
// generated backing store. Routing every consumer through this interface
// localizes that swap to one implementation ([yamlStore]); nothing else in
// the tree assumes the state lives in a *config.ProjectConfig.
//
// The other ~40 forge.yaml sections (deploy, ci, docker, k8s, lint,
// contracts, auth, docs, stack, api, database, packages, frontends, packs)
// are NOT swap targets — they stay hand-edited config. The store exposes
// them via section accessors that return the existing config sub-types, so
// consumers can read them without holding the top-level *config.ProjectConfig.
package projectstore

import "github.com/reliant-labs/forge/internal/config"

// ProjectStore is the handle a loaded forge project is read and mutated
// through. The yamlStore implementation wraps today's
// LoadStrict/ProjectConfig/ApplyDerivedDefaults machinery; a Phase-2
// implementation can back the project/component/feature surface with a
// generated store while leaving the section accessors reading forge.yaml.
//
// Method groups:
//
//   - Project metadata: [ProjectStore.Meta] — name, module path, kind,
//     binary mode, versions, hot-reload. The derived/effective forms
//     (EffectiveKind etc.) live on the returned [ProjectMeta].
//   - Components: [ProjectStore.Components] plus the kind filters
//     ([ProjectStore.Servers] … [ProjectStore.BinaryComponents]). These
//     return the [Component] view type, not config.ComponentConfig, so a
//     Phase-2 backing can synthesize them.
//   - Features: [ProjectStore.Features] returns a [FeatureSet] — the
//     resolved (derived + explicit) enabled/disabled state.
//   - Mutation: [ProjectStore.AppendComponent], [ProjectStore.AppendWebhook],
//     [ProjectStore.SetPacks] — the `forge add` / pack write paths, kept
//     explicit so raw component appends don't scatter through the tree.
//   - Section access: [ProjectStore.Database] … [ProjectStore.PackOverrides]
//     expose the non-swap forge.yaml sections by their existing config types.
//   - [ProjectStore.Config] is the escape hatch returning the underlying
//     *config.ProjectConfig for the write/marshal path and the handful of
//     whole-config consumers (generate pipeline context, docs builder,
//     NormalizeForWrite). It is the ONE seam Phase 2 must reconcile; every
//     other consumer reads through the typed accessors above.
type ProjectStore interface {
	// Meta returns project-level metadata (name, module, kind, versions).
	Meta() ProjectMeta

	// Components returns every component in declaration order.
	Components() []Component
	// Servers returns the server-kind components.
	Servers() []Component
	// Workers returns the worker-kind components.
	Workers() []Component
	// Crons returns the cron-kind components.
	Crons() []Component
	// Operators returns the operator-kind components.
	Operators() []Component
	// BinaryComponents returns the binary-kind components.
	BinaryComponents() []Component

	// Features returns the resolved feature set.
	Features() FeatureSet

	// AppendComponent appends a component to the project (the `forge add`
	// server/worker/cron/operator/binary write path).
	AppendComponent(c config.ComponentConfig)
	// AppendWebhook appends a webhook to the named component (the
	// `forge add webhook` write path). Returns false if no component with
	// that name exists.
	AppendWebhook(componentName string, w config.WebhookConfig) bool
	// SetPacks replaces the installed-packs list (the pack install/remove
	// write path).
	SetPacks(packs []string)

	// Section accessors — the non-swap forge.yaml sections, by their
	// existing config types.
	Packages() []config.PackageConfig
	Frontends() []config.FrontendConfig
	FrontendProject() config.FrontendProjectConfig
	Database() config.DatabaseConfig
	CI() config.CIConfig
	Deploy() config.DeployConfig
	Docker() config.DockerConfig
	K8s() config.K8sConfig
	Lint() config.LintConfig
	Contracts() config.ContractsConfig
	Auth() config.AuthConfig
	Docs() config.DocsConfig
	Stack() config.StackConfig
	API() config.APIConfig
	Packs() []string
	PackOverrides() map[string]config.PackOverride

	// Config returns the underlying project config. This is the
	// implementation seam — only the write/marshal path and the few
	// whole-config consumers reach for it; Phase 2 reconciles exactly this
	// method.
	Config() *config.ProjectConfig
}

// ProjectMeta is the project-level metadata view: identity, kind, binary
// mode, and the pinned versions. The Effective*/Is* accessors mirror the
// config helpers so consumers get the derived forms without re-deriving.
type ProjectMeta struct {
	Name         string
	ModulePath   string
	Kind         string // raw kind: "" | service | cli | library
	Binary       string // raw binary mode: "" | per-service | shared
	Version      string
	ForgeVersion string
}

// EffectiveKind returns the project kind, defaulting to "service".
func (m ProjectMeta) EffectiveKind() string { return config.EffectiveProjectKind(m.Kind) }

// EffectiveBinary returns the binary mode, defaulting to "per-service".
func (m ProjectMeta) EffectiveBinary() string { return config.EffectiveProjectBinary(m.Binary) }

// IsBinaryShared reports whether the project uses the shared-binary mode.
func (m ProjectMeta) IsBinaryShared() bool {
	return m.EffectiveBinary() == config.ProjectBinaryShared
}

// EffectiveForgeVersion returns the pinned forge version, defaulting to
// "0.0.0" for projects predating the field.
func (m ProjectMeta) EffectiveForgeVersion() string {
	if v := trim(m.ForgeVersion); v == "" {
		return "0.0.0"
	}
	return m.ForgeVersion
}

// IsCLIKind reports whether the project is a CLI binary.
func (m ProjectMeta) IsCLIKind() bool { return m.EffectiveKind() == config.ProjectKindCLI }

// IsLibraryKind reports whether the project is a pure Go library.
func (m ProjectMeta) IsLibraryKind() bool { return m.EffectiveKind() == config.ProjectKindLibrary }

// IsServiceKind reports whether the project is a Connect-RPC service.
func (m ProjectMeta) IsServiceKind() bool { return m.EffectiveKind() == config.ProjectKindService }

// Component is the per-component view: name, kind, ports, schedule, and the
// kind-specific fields. It mirrors config.ComponentConfig's read surface so
// a Phase-2 backing can synthesize components without a config struct.
type Component struct {
	Name          string
	Kind          string // raw kind: "" | server | worker | cron | operator | binary
	Path          string
	Ports         map[string]config.PortSpec
	Schedule      string
	ProtoPackages []string
	Webhooks      []config.WebhookConfig
	Group         string
	Version       string
	CRDs          []config.CRDConfig
}

// EffectiveKind returns the lowercased kind, defaulting to "server".
func (c Component) EffectiveKind() string {
	if k := lowerTrim(c.Kind); k != "" {
		return k
	}
	return config.ComponentKindServer
}

// IsServer reports whether the component is a Connect-RPC server.
func (c Component) IsServer() bool { return c.EffectiveKind() == config.ComponentKindServer }

// IsWorker reports whether the component is an in-process worker.
func (c Component) IsWorker() bool { return c.EffectiveKind() == config.ComponentKindWorker }

// IsCron reports whether the component is a scheduled cron job.
func (c Component) IsCron() bool { return c.EffectiveKind() == config.ComponentKindCron }

// IsOperator reports whether the component is a controller-runtime operator.
func (c Component) IsOperator() bool { return c.EffectiveKind() == config.ComponentKindOperator }

// IsBinary reports whether the component is a standalone binary subcommand.
func (c Component) IsBinary() bool { return c.EffectiveKind() == config.ComponentKindBinary }

// PrimaryPort returns the component's primary HTTP port (ports.http), or 0.
func (c Component) PrimaryPort() int {
	if c.Ports == nil {
		return 0
	}
	return c.Ports[config.HTTPPortName].Port
}

// FeatureSet is the resolved feature state of a project — the same
// derived+explicit resolution config.FeaturesConfig performs. It is a thin
// alias today (yamlStore returns the config block directly) so every
// existing *Enabled() accessor keeps working; the type exists so the
// interface advertises features as a first-class surface for Phase 2.
type FeatureSet = config.FeaturesConfig
