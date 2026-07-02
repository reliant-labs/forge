// Package projectstore is the single read+mutate surface for a forge
// project's state. Every consumer outside the config loader reads project
// metadata, components, and feature flags through a [Store] rather than
// touching *config.ProjectConfig directly.
//
// Why the indirection: the project + component + feature state is the part
// of forge.yaml that a future revision (the "Phase 2" source swap) wants to
// relocate — out of the hand-edited forge.yaml and into a denormalized,
// generated backing store. Routing every consumer through this one type
// localizes that swap to one implementation; nothing else in the tree
// assumes the state lives in a *config.ProjectConfig.
//
// Interfaces at the consumer, not here. [New] returns a concrete *[Store]
// (accept interfaces, return structs). Callers that take the store as a
// dependency declare the narrow interface they actually use next to
// themselves — e.g. `type featureReader interface { Features() FeatureSet }`
// for the feature-gate helpers, `type metaReader interface { Meta()
// ProjectMeta }` for the namespace resolvers. A wide interface declared here
// that each caller used a slice of was interface bloat: 16 of its 29 methods
// had zero callers. The store therefore exposes only the accessors that are
// actually read; add a method when a consumer needs it, not before.
//
// forge:exclude-contract
// projectstore is the project-state persistence store (a concrete *Store
// over a *config.ProjectConfig), not a bootstrap-wired Connect service. It
// has no Service/Deps/New contract shape, so opt out of the require-contract
// rule.
package projectstore

import "github.com/reliant-labs/forge/internal/config"

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
