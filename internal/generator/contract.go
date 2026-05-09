// Package generator orchestrates project scaffolding: it lays down a
// new project, scaffolds individual services / workers / operators /
// webhooks / frontends, regenerates infra files on upgrade, and emits
// the plan-mode entity / proto / ORM / migration files.
//
// The behavioural surface is split into two narrow Services:
//
//   - Service       — the file-emission orchestrator (every file the
//                     scaffolder writes into the user project).
//   - ConfigService — read / write / mutate forge.yaml on disk.
//
// Data carriers (FileChecksums, MemoryFormat, ProjectGenerator,
// SeedEntity, E2EMethodInfo, UpgradeResult, ServiceInfo, *TemplateData
// structs) remain plain types — they have no behavioural seam to mock.
package generator

import "github.com/reliant-labs/forge/internal/config"

// Service is the file-emission surface of the generator package.
// Methods write into the user-project working directory; callers wanting
// finer-grained mocking can stub one method at a time.
type Service interface {
	// Plan-mode generators.
	GeneratePlanDBTypes(root, modulePath, serviceName string, entityNames []string) error
	GeneratePlanMigrations(root string, entities []config.PlanEntity) error
	GeneratePlanORM(root, modulePath, serviceName string, entities []config.PlanEntity) error
	GeneratePlanProtoFile(root, modulePath, serviceName string, rpcs []config.PlanRPC, entities []config.PlanEntity) error

	// Component scaffolders.
	GenerateServiceFiles(root, modulePath, serviceName, projectName string, port int) error
	GenerateWorkerFiles(root, modulePath, workerName, kind, schedule string) error
	GenerateOperatorFiles(root, modulePath, name, group, version string) error
	GenerateWebhookFiles(root, modulePath, serviceName, webhookName string) error
	GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int, kind string) error
	EnsureCoreComponents(frontendDir string) error
	GenerateE2ETests(projectDir, serviceName, modulePath, projectName string, methods []E2EMethodInfo) error
	GenerateEntitySeeds(entities []SeedEntity, outputDir string) error
	GenerateGrafanaDashboards(projectName, projectDir string) error

	// Project-level upgrade / regeneration.
	RegenerateInfraFiles(projectDir string, cfg *config.ProjectConfig) error
	Upgrade(projectDir string, cfg *config.ProjectConfig, force bool, checkOnly bool) ([]UpgradeResult, error)

	// Checksum-aware file writes (used by the bootstrap orchestrator).
	WriteGeneratedFile(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error)
	HashContent(content []byte) string
	LoadChecksums(root string) (*FileChecksums, error)
	SaveChecksums(root string, cs *FileChecksums) error
}

// ConfigService loads, writes, and mutates the on-disk forge.yaml.
type ConfigService interface {
	ReadProjectConfig(path string) (*config.ProjectConfig, error)
	WriteProjectConfigFile(cfg *config.ProjectConfig, path string) error
	AppendServiceToConfig(projectRoot, serviceName string, port int) error
	AppendFrontendToConfig(projectRoot, frontendName string, port int) error
	AppendFrontendToConfigWithKind(projectRoot, frontendName string, port int, kind string) error
}

// Deps wires generator's cross-package collaborators. Empty today; the
// scaffolders use codegen + templates via package-level helpers. When
// internal/cli is ported, the Deps struct will gain Codegen / Templates
// fields wired through the New constructor.
type Deps struct{}

// New constructs the file-emission Service.
func New(_ Deps) Service { return &svc{} }

// NewConfigService constructs the forge.yaml read/write surface.
func NewConfigService(_ Deps) ConfigService { return &configSvc{} }

// svc satisfies Service. Methods delegate to the package-level
// helpers in their original files.
type svc struct{}

// configSvc satisfies ConfigService. Methods delegate to project_config.go.
type configSvc struct{}
