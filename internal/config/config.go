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
	Features   FeaturesConfig      `yaml:"features,omitempty"`
	Stack      StackConfig         `yaml:"stack,omitempty"`
	Packs      []string            `yaml:"packs,omitempty"`
}

// ServiceConfig represents a Go service definition.
type ServiceConfig struct {
	Name          string          `yaml:"name"`
	Type          string          `yaml:"type"`           // "go_service", "worker", "operator"
	Kind          string          `yaml:"kind,omitempty"` // sub-type: worker kind ("cron"), empty = default
	Path          string          `yaml:"path"`
	Port          int             `yaml:"port,omitempty"`
	Schedule      string          `yaml:"schedule,omitempty"` // cron expression for kind=cron workers
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

// FrontendConfig defines a frontend application (e.g. Next.js, React Native).
type FrontendConfig struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`           // "nextjs", "react-native"
	Kind string `yaml:"kind,omitempty"` // "web" (default/Next.js), "mobile" (React Native)
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
	Driver          string                `yaml:"driver"` // "postgres", "sqlite"
	MigrationsDir   string                `yaml:"migrations_dir"`
	SQLCEnabled     bool                  `yaml:"sqlc_enabled"`
	MigrationSafety MigrationSafetyConfig `yaml:"migration_safety,omitempty"`
}

type MigrationSafetyConfig struct {
	Enabled            *bool    `yaml:"enabled,omitempty"`             // nil = enabled
	UnsafeAddColumn    string   `yaml:"unsafe_add_column,omitempty"`   // error, warn, off
	DestructiveChange  string   `yaml:"destructive_change,omitempty"`  // error, warn, off
	VolatileDefault    string   `yaml:"volatile_default,omitempty"`    // warn, error, off
	AllowedDestructive []string `yaml:"allowed_destructive,omitempty"` // file globs that may contain destructive changes
}

func (c MigrationSafetyConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c MigrationSafetyConfig) EffectiveUnsafeAddColumn() string {
	return effectiveSeverity(c.UnsafeAddColumn, "error")
}

func (c MigrationSafetyConfig) EffectiveDestructiveChange() string {
	return effectiveSeverity(c.DestructiveChange, "error")
}

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
	Contract bool               `yaml:"contract"`
	Frontend FrontendLintConfig `yaml:"frontend,omitempty"`
}

type FrontendLintConfig struct {
	CSSHealth      bool   `yaml:"css_health,omitempty"`       // enable stylelint-backed CSS health checks
	NoImportant    string `yaml:"no_important,omitempty"`     // error, warn, off
	NoInlineStyles string `yaml:"no_inline_styles,omitempty"` // error, warn, off
}

func (c FrontendLintConfig) EffectiveNoImportant() string {
	return effectiveSeverity(c.NoImportant, "warn")
}

func (c FrontendLintConfig) EffectiveNoInlineStyles() string {
	return effectiveSeverity(c.NoInlineStyles, "warn")
}

// ContractsConfig controls contract enforcement linter behavior.
type ContractsConfig struct {
	Strict             bool     `yaml:"strict"`               // require contract.go for all internal packages with exported methods (default: true)
	AllowExportedVars  bool     `yaml:"allow_exported_vars"`  // allow exported package vars (default: false)
	AllowExportedFuncs bool     `yaml:"allow_exported_funcs"` // allow exported funcs without contract (default: true)
	Exclude            []string `yaml:"exclude"`              // packages that opt out
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

// FeaturesConfig controls which forge features are active.
// All fields are *bool so that nil means "enabled" (backwards compat).
// Explicitly set to false to disable a feature.
type FeaturesConfig struct {
	ORM           *bool `yaml:"orm,omitempty"`           // protoc-gen-forge-orm codegen
	Codegen       *bool `yaml:"codegen,omitempty"`       // service/handler codegen from protos
	Migrations    *bool `yaml:"migrations,omitempty"`    // auto-generate SQL migrations
	CI            *bool `yaml:"ci,omitempty"`            // generate CI/CD workflows
	Deploy        *bool `yaml:"deploy,omitempty"`        // generate deploy manifests (KCL, Dockerfiles)
	Contracts     *bool `yaml:"contracts,omitempty"`     // contract linter enforcement
	Docs          *bool `yaml:"docs,omitempty"`          // documentation generation
	Frontend      *bool `yaml:"frontend,omitempty"`      // frontend scaffolding + codegen
	Observability *bool `yaml:"observability,omitempty"` // alloy, grafana dashboards, otel wiring
	HotReload     *bool `yaml:"hot_reload,omitempty"`    // air config generation
}

// featureEnabled returns true if the *bool is nil (default) or explicitly true.
func featureEnabled(b *bool) bool {
	return b == nil || *b
}

func (f FeaturesConfig) ORMEnabled() bool           { return featureEnabled(f.ORM) }
func (f FeaturesConfig) CodegenEnabled() bool       { return featureEnabled(f.Codegen) }
func (f FeaturesConfig) MigrationsEnabled() bool    { return featureEnabled(f.Migrations) }
func (f FeaturesConfig) CIEnabled() bool            { return featureEnabled(f.CI) }
func (f FeaturesConfig) DeployEnabled() bool        { return featureEnabled(f.Deploy) }
func (f FeaturesConfig) ContractsEnabled() bool     { return featureEnabled(f.Contracts) }
func (f FeaturesConfig) DocsEnabled() bool          { return featureEnabled(f.Docs) }
func (f FeaturesConfig) FrontendEnabled() bool      { return featureEnabled(f.Frontend) }
func (f FeaturesConfig) ObservabilityEnabled() bool { return featureEnabled(f.Observability) }
func (f FeaturesConfig) HotReloadEnabled() bool     { return featureEnabled(f.HotReload) }

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
