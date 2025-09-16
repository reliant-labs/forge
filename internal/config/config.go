// Package config defines the canonical forge.project.yaml types shared by
// both the CLI (read) and the generator (write) packages.
package config

// ProjectConfig represents the forge.project.yaml file.
// Fields align with proto/forge/project/v1/project.proto.
type ProjectConfig struct {
	Name       string              `yaml:"name"`
	ModulePath string              `yaml:"module_path"`
	Version    string              `yaml:"version"`
	HotReload  bool                `yaml:"hot_reload"`
	Services   []ServiceConfig     `yaml:"services"`
	Packages   []PackageConfig     `yaml:"packages,omitempty"`
	Frontends  []FrontendConfig    `yaml:"frontends,omitempty"`
	Envs       []EnvironmentConfig `yaml:"environments"`
	Database   DatabaseConfig      `yaml:"database"`
	CI         CIConfig            `yaml:"ci"`
	Docker     DockerConfig        `yaml:"docker"`
	K8s        K8sConfig           `yaml:"k8s"`
	Lint       LintConfig          `yaml:"lint"`
	Auth       AuthConfig          `yaml:"auth"`
	Docs       DocsConfig          `yaml:"docs"`
}

// ServiceConfig represents a Go service definition.
type ServiceConfig struct {
	Name          string   `yaml:"name"`
	Type          string   `yaml:"type"` // "go_service", "worker"
	Path          string   `yaml:"path"`
	Port          int      `yaml:"port"`
	ProtoPackages []string `yaml:"proto_packages,omitempty"`
}

// PackageConfig represents an internal package with a Go interface contract.
type PackageConfig struct {
	Name string `yaml:"name"`
}

// FrontendConfig defines a frontend application (e.g. Next.js).
type FrontendConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"` // "nextjs"
	Path string `yaml:"path"`
	Port int    `yaml:"port"`
}

// EnvironmentConfig represents a deployment environment.
type EnvironmentConfig struct {
	Name      string   `yaml:"name"` // dev, staging, prod
	Type      string   `yaml:"type"` // "local", "cloud"
	Services  []string `yaml:"services,omitempty"`
	Registry  string   `yaml:"registry,omitempty"`
	Namespace string   `yaml:"namespace,omitempty"`
	Domain    string   `yaml:"domain,omitempty"`
}

// DatabaseConfig holds database-related settings.
type DatabaseConfig struct {
	Driver        string `yaml:"driver"` // "postgres", "sqlite"
	MigrationsDir string `yaml:"migrations_dir"`
	SQLCEnabled   bool   `yaml:"sqlc_enabled"`
}

// CIConfig holds CI/CD settings.
type CIConfig struct {
	Provider string `yaml:"provider"` // "github"
	Lint     bool   `yaml:"lint"`
	Test     bool   `yaml:"test"`
	Build    bool   `yaml:"build"`
	Deploy   bool   `yaml:"deploy"`
	VulnScan bool   `yaml:"vuln_scan"`
}

// DockerConfig holds Docker registry configuration.
type DockerConfig struct {
	Registry   string            `yaml:"registry"`
	BaseImages map[string]string `yaml:"base_images,omitempty"`
}

// LintConfig holds lint-related settings.
type LintConfig struct {
	ProtoMethod bool `yaml:"proto_method"`
	Contract    bool `yaml:"contract"`
}

// AuthConfig holds authentication provider settings.
type AuthConfig struct {
	Provider string `yaml:"provider"` // "supabase", "firebase", "auth0", "none"
}

// K8sConfig holds Kubernetes configuration.
type K8sConfig struct {
	Provider string `yaml:"provider"` // "k3d", "gke", "eks"
	KCLDir   string `yaml:"kcl_dir"`
}

// DocsConfig holds documentation generation settings.
type DocsConfig struct {
	Enabled           *bool    `yaml:"enabled,omitempty"`            // nil = true (enabled by default)
	OutputDir         string   `yaml:"output_dir,omitempty"`         // default: "docs/generated"
	Format            string   `yaml:"format,omitempty"`             // "markdown" (default) or "hugo"
	Generators        []string `yaml:"generators,omitempty"`         // e.g. ["api", "architecture", "config", "contracts"]
	CustomTemplatesDir string  `yaml:"custom_templates_dir,omitempty"` // user template overrides
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