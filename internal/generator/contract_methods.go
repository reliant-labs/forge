package generator

import "github.com/reliant-labs/forge/internal/config"

// This file wires the Service / ConfigService contract methods on *svc /
// *configSvc into the package's free-function implementations. Keeping
// the file-emission helpers as package-level funcs preserves the
// internal call graph (project.go calls GenerateServiceFiles directly,
// upgrade.go calls RegenerateInfraFiles, etc.) without forcing every
// intra-package caller through a Service handle.

// --- Service ---

func (s *svc) GeneratePlanDBTypes(root, modulePath, serviceName string, entityNames []string) error {
	return GeneratePlanDBTypes(root, modulePath, serviceName, entityNames)
}

func (s *svc) GeneratePlanMigrations(root string, entities []config.PlanEntity) error {
	return GeneratePlanMigrations(root, entities)
}

func (s *svc) GeneratePlanORM(root, modulePath, serviceName string, entities []config.PlanEntity) error {
	return GeneratePlanORM(root, modulePath, serviceName, entities)
}

func (s *svc) GeneratePlanProtoFile(root, modulePath, serviceName string, rpcs []config.PlanRPC, entities []config.PlanEntity) error {
	return GeneratePlanProtoFile(root, modulePath, serviceName, rpcs, entities)
}

func (s *svc) GenerateServiceFiles(root, modulePath, serviceName, projectName string, port int) error {
	return GenerateServiceFiles(root, modulePath, serviceName, projectName, port)
}

func (s *svc) GenerateWorkerFiles(root, modulePath, workerName, kind, schedule string) error {
	return GenerateWorkerFiles(root, modulePath, workerName, kind, schedule)
}

func (s *svc) GenerateOperatorFiles(root, modulePath, name, group, version string) error {
	return GenerateOperatorFiles(root, modulePath, name, group, version)
}

func (s *svc) GenerateWebhookFiles(root, modulePath, serviceName, webhookName string) error {
	return GenerateWebhookFiles(root, modulePath, serviceName, webhookName)
}

func (s *svc) GenerateFrontendFiles(root, modulePath, projectName, frontendName string, apiPort int, kind string) error {
	return GenerateFrontendFiles(root, modulePath, projectName, frontendName, apiPort, kind)
}

func (s *svc) EnsureCoreComponents(frontendDir string) error {
	return EnsureCoreComponents(frontendDir)
}

func (s *svc) GenerateE2ETests(projectDir, serviceName, modulePath, projectName string, methods []E2EMethodInfo) error {
	return GenerateE2ETests(projectDir, serviceName, modulePath, projectName, methods)
}

func (s *svc) GenerateEntitySeeds(entities []SeedEntity, outputDir string) error {
	return GenerateEntitySeeds(entities, outputDir)
}

func (s *svc) GenerateGrafanaDashboards(projectName, projectDir string) error {
	return GenerateGrafanaDashboards(projectName, projectDir)
}

func (s *svc) RegenerateInfraFiles(projectDir string, cfg *config.ProjectConfig) error {
	return RegenerateInfraFiles(projectDir, cfg)
}

func (s *svc) Upgrade(projectDir string, cfg *config.ProjectConfig, force bool, checkOnly bool) ([]UpgradeResult, error) {
	return Upgrade(projectDir, cfg, force, checkOnly)
}

func (s *svc) WriteGeneratedFile(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	return WriteGeneratedFile(root, relPath, content, cs, force)
}

func (s *svc) HashContent(content []byte) string {
	return HashContent(content)
}

func (s *svc) LoadChecksums(root string) (*FileChecksums, error) {
	return LoadChecksums(root)
}

func (s *svc) SaveChecksums(root string, cs *FileChecksums) error {
	return SaveChecksums(root, cs)
}

// --- ConfigService ---

func (c *configSvc) ReadProjectConfig(path string) (*config.ProjectConfig, error) {
	return ReadProjectConfig(path)
}

func (c *configSvc) WriteProjectConfigFile(cfg *config.ProjectConfig, path string) error {
	return WriteProjectConfigFile(cfg, path)
}

func (c *configSvc) AppendServiceToConfig(projectRoot, serviceName string, port int) error {
	return AppendServiceToConfig(projectRoot, serviceName, port)
}

func (c *configSvc) AppendFrontendToConfig(projectRoot, frontendName string, port int) error {
	return AppendFrontendToConfig(projectRoot, frontendName, port)
}

func (c *configSvc) AppendFrontendToConfigWithKind(projectRoot, frontendName string, port int, kind string) error {
	return AppendFrontendToConfigWithKind(projectRoot, frontendName, port, kind)
}

var (
	_ Service       = (*svc)(nil)
	_ ConfigService = (*configSvc)(nil)
)
