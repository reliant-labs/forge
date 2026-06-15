package projectstore

import "github.com/reliant-labs/forge/internal/config"

// yamlStore is the forge.yaml-backed [ProjectStore]. It wraps a
// *config.ProjectConfig that has already been through the loader
// (LoadStrict → ApplyDerivedDefaults → path/kind normalization) and
// projects its read surface into the interface's view types.
//
// This is the ONLY type that holds a *config.ProjectConfig. Phase 2's
// source swap replaces (or supplements) this implementation; every consumer
// reads through the ProjectStore interface, so the swap is invisible to them.
type yamlStore struct {
	cfg *config.ProjectConfig
}

// New wraps an already-loaded, already-derived project config in a
// ProjectStore. The caller owns loading + normalization (the cli/generator
// loaders); New takes the resulting config and exposes it through the
// interface. The pointer is retained (not copied) so mutation methods and
// Config() observe the same underlying config the loader produced.
func New(cfg *config.ProjectConfig) ProjectStore {
	return &yamlStore{cfg: cfg}
}

func (s *yamlStore) Meta() ProjectMeta {
	return ProjectMeta{
		Name:         s.cfg.Name,
		ModulePath:   s.cfg.ModulePath,
		Kind:         s.cfg.Kind,
		Binary:       s.cfg.Binary,
		Version:      s.cfg.Version,
		ForgeVersion: s.cfg.ForgeVersion,
	}
}

func (s *yamlStore) Components() []Component {
	return toComponents(s.cfg.Components)
}

func (s *yamlStore) Servers() []Component   { return toComponents(s.cfg.Servers()) }
func (s *yamlStore) Workers() []Component   { return toComponents(s.cfg.Workers()) }
func (s *yamlStore) Crons() []Component     { return toComponents(s.cfg.Crons()) }
func (s *yamlStore) Operators() []Component { return toComponents(s.cfg.Operators()) }
func (s *yamlStore) BinaryComponents() []Component {
	return toComponents(s.cfg.BinaryComponents())
}

func (s *yamlStore) Features() FeatureSet { return s.cfg.Features }

func (s *yamlStore) AppendComponent(c config.ComponentConfig) {
	s.cfg.Components = append(s.cfg.Components, c)
}

func (s *yamlStore) AppendWebhook(componentName string, w config.WebhookConfig) bool {
	for i := range s.cfg.Components {
		if s.cfg.Components[i].Name == componentName {
			s.cfg.Components[i].Webhooks = append(s.cfg.Components[i].Webhooks, w)
			return true
		}
	}
	return false
}

func (s *yamlStore) SetPacks(packs []string) { s.cfg.Packs = packs }

func (s *yamlStore) Packages() []config.PackageConfig              { return s.cfg.Packages }
func (s *yamlStore) Frontends() []config.FrontendConfig            { return s.cfg.Frontends }
func (s *yamlStore) FrontendProject() config.FrontendProjectConfig { return s.cfg.Frontend }
func (s *yamlStore) Database() config.DatabaseConfig               { return s.cfg.Database }
func (s *yamlStore) CI() config.CIConfig                           { return s.cfg.CI }
func (s *yamlStore) Deploy() config.DeployConfig                   { return s.cfg.Deploy }
func (s *yamlStore) Docker() config.DockerConfig                   { return s.cfg.Docker }
func (s *yamlStore) K8s() config.K8sConfig                         { return s.cfg.K8s }
func (s *yamlStore) Lint() config.LintConfig                       { return s.cfg.Lint }
func (s *yamlStore) Contracts() config.ContractsConfig             { return s.cfg.Contracts }
func (s *yamlStore) Auth() config.AuthConfig                       { return s.cfg.Auth }
func (s *yamlStore) Docs() config.DocsConfig                       { return s.cfg.Docs }
func (s *yamlStore) Stack() config.StackConfig                     { return s.cfg.Stack }
func (s *yamlStore) API() config.APIConfig                         { return s.cfg.API }
func (s *yamlStore) Packs() []string                               { return s.cfg.Packs }
func (s *yamlStore) PackOverrides() map[string]config.PackOverride {
	return s.cfg.PackOverrides
}

func (s *yamlStore) Config() *config.ProjectConfig { return s.cfg }

// toComponent projects one config.ComponentConfig into the view type.
func toComponent(c config.ComponentConfig) Component {
	return Component{
		Name:          c.Name,
		Kind:          c.Kind,
		Path:          c.Path,
		Ports:         c.Ports,
		Schedule:      c.Schedule,
		ProtoPackages: c.ProtoPackages,
		Webhooks:      c.Webhooks,
		Group:         c.Group,
		Version:       c.Version,
		CRDs:          c.CRDs,
	}
}

func toComponents(in []config.ComponentConfig) []Component {
	if in == nil {
		return nil
	}
	out := make([]Component, len(in))
	for i, c := range in {
		out[i] = toComponent(c)
	}
	return out
}
