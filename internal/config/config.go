// Package config defines the canonical forge.yaml types shared by
// both the CLI (read) and the generator (write) packages.
package config

import "strings"

// ProjectConfig represents the forge.yaml file.
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
	Deploy     DeployConfig        `yaml:"deploy,omitempty"`
	Docker     DockerConfig        `yaml:"docker"`
	K8s        K8sConfig           `yaml:"k8s"`
	Lint       LintConfig          `yaml:"lint"`
	Contracts  ContractsConfig     `yaml:"contracts"`
	Auth       AuthConfig          `yaml:"auth"`
	Docs       DocsConfig          `yaml:"docs"`
	Packs      []string            `yaml:"packs,omitempty"`
}

// ServiceConfig represents a Go service definition.
type ServiceConfig struct {
	Name          string          `yaml:"name"`
	Type          string          `yaml:"type"` // "go_service", "worker", "operator"
	Path          string          `yaml:"path"`
	Port          int             `yaml:"port"`
	ProtoPackages []string        `yaml:"proto_packages,omitempty"`
	Webhooks      []WebhookConfig `yaml:"webhooks,omitempty"`
}

// WebhookConfig represents a webhook endpoint within a service.
type WebhookConfig struct {
	Name string `yaml:"name"` // e.g. "stripe", "github"
}

// PackageConfig represents an internal package with a Go interface contract.
type PackageConfig struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind,omitempty"` // "" (default/generic), "client", "eventbus"
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
	Golangci bool `yaml:"golangci"` // default true
	Buf      bool `yaml:"buf"`      // default true
	Frontend bool `yaml:"frontend"` // default true
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
	return c.Lint == (CILintConfig{}) || c.Lint.Golangci || c.Lint.Buf || c.Lint.Frontend
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
	Provider       string              `yaml:"provider"`                  // "github"
	Registry       string              `yaml:"registry,omitempty"`        // "ghcr" (default), "gar", "ecr"
	Environments   []DeployEnvConfig   `yaml:"environments,omitempty"`
	Concurrency    DeployConcurrency   `yaml:"concurrency,omitempty"`
	FrontendDeploy string              `yaml:"frontend_deploy,omitempty"` // "firebase", "vercel", "none"
	MigrationTest  bool                `yaml:"migration_test,omitempty"` // test migrations before deploy
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
}

// LintConfig holds lint-related settings.
type LintConfig struct {
	Contract bool `yaml:"contract"`
}

// ContractsConfig controls contract enforcement linter behavior.
type ContractsConfig struct {
	Strict            bool     `yaml:"strict"`              // require contract.go for all internal packages with exported methods (default: true)
	AllowExportedVars bool     `yaml:"allow_exported_vars"` // allow exported package vars (default: false)
	AllowExportedFuncs bool    `yaml:"allow_exported_funcs"` // allow exported funcs without contract (default: true)
	Exclude           []string `yaml:"exclude"`             // packages that opt out
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

// IsExcluded returns true if the given package path matches any exclude pattern.
func (c ContractsConfig) IsExcluded(pkgPath string) bool {
	for _, pattern := range c.Exclude {
		if pattern == pkgPath || strings.HasSuffix(pkgPath, "/"+pattern) || strings.Contains(pkgPath, pattern) {
			return true
		}
	}
	return false
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