package codegen

import "github.com/reliant-labs/forge/internal/config"

// Parser reads and parses proto files and source directories to extract
// service, entity, and configuration definitions.
type Parser interface {
	ParseServicesFromProtos(dir string, projectDir string) ([]ServiceDef, error)
	GetModulePath(dir string) (string, error)
	ParseEntityProtos(projectDir string) ([]EntityDef, error)
	ParseConfigProto(protoPath string) ([]ConfigMessage, error)
	ParseConfigProtosFromDir(dir string) ([]ConfigMessage, error)
	DetectFallibleConstructor(dir string) (bool, error)
}

// Generator generates code files — service stubs, mocks, bootstrap wiring,
// middleware, CRUD handlers, and more.
type Generator interface {
	GenerateServiceStub(svc ServiceDef, targetDir string) error
	RegenerateServiceFile(svc ServiceDef, targetDir string) error
	GenerateMock(svc ServiceDef, mockDir string) (written bool, err error)
	GenerateBootstrap(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, hasDatabase bool, ormEnabled bool, projectDir string) error
	GenerateBootstrapTesting(services []ServiceDef, packages []BootstrapPackageData, workers []BootstrapWorkerData, operators []BootstrapOperatorData, modulePath string, multiTenantEnabled bool, projectDir string) error
	GenerateMigrate(targetDir string, modulePath string, hasMigrations bool) error
	GenerateMissingHandlerStubs(svc ServiceDef, targetDir string, crudMethodNames map[string]bool) (*MissingHandlerResult, error)
	GenerateSetup(modulePath string, databaseDriver string, ormEnabled bool, targetDir string) error
	GenerateConfigLoader(messages []ConfigMessage, targetDir string) error
	GenerateAuthMiddleware(cfg *config.AuthConfig, modulePath string, skipMethods []string, targetDir string) error
	GenerateTenantMiddleware(mt *config.MultiTenantConfig, targetDir string) error
	GenerateCRUDHandlers(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string) error
	GenerateCRUDTests(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string) error
}