package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator/contract"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()
	r.Register(&APIGenerator{})
	r.Register(&ArchitectureGenerator{})

	if g := r.Get("api"); g == nil {
		t.Fatal("expected api generator to be registered")
	}
	if g := r.Get("architecture"); g == nil {
		t.Fatal("expected architecture generator to be registered")
	}
	if g := r.Get("nonexistent"); g != nil {
		t.Fatal("expected nil for unregistered generator")
	}

	names := r.Names()
	if len(names) != 2 || names[0] != "api" || names[1] != "architecture" {
		t.Fatalf("unexpected names: %v", names)
	}
}

func TestDefaultRegistryHasAllGenerators(t *testing.T) {
	r := DefaultRegistry()
	expected := []string{"api", "architecture", "config", "contracts"}
	names := r.Names()
	if len(names) != len(expected) {
		t.Fatalf("expected %d generators, got %d: %v", len(expected), len(names), names)
	}
	for i, name := range expected {
		if names[i] != name {
			t.Errorf("expected generator[%d] = %q, got %q", i, name, names[i])
		}
	}
}

func TestResolveGeneratorsAll(t *testing.T) {
	r := DefaultRegistry()
	generators := resolveGenerators(r, nil)
	if len(generators) != 4 {
		t.Fatalf("expected 4 generators, got %d", len(generators))
	}
}

func TestResolveGeneratorsFiltered(t *testing.T) {
	r := DefaultRegistry()
	generators := resolveGenerators(r, []string{"api", "config"})
	if len(generators) != 2 {
		t.Fatalf("expected 2 generators, got %d", len(generators))
	}
	if generators[0].Name() != "api" || generators[1].Name() != "config" {
		t.Fatalf("unexpected generators: %v, %v", generators[0].Name(), generators[1].Name())
	}
}

func TestDocsConfigDefaults(t *testing.T) {
	cfg := config.DocsConfig{}

	if !cfg.IsEnabled() {
		t.Error("expected enabled by default (nil pointer)")
	}
	if cfg.EffectiveOutputDir() != "docs/generated" {
		t.Errorf("expected default output dir, got %q", cfg.EffectiveOutputDir())
	}
	if cfg.EffectiveFormat() != "markdown" {
		t.Errorf("expected default format, got %q", cfg.EffectiveFormat())
	}
}

func TestDocsConfigDisabled(t *testing.T) {
	f := false
	cfg := config.DocsConfig{Enabled: &f}
	if cfg.IsEnabled() {
		t.Error("expected disabled when Enabled is false")
	}
}

func TestOverridesApply(t *testing.T) {
	cfg := config.DocsConfig{}
	overrides := &Overrides{
		OutputDir:  "custom/docs",
		Format:     "hugo",
		Generators: []string{"api"},
	}
	overrides.Apply(&cfg)

	if cfg.OutputDir != "custom/docs" {
		t.Errorf("expected custom output dir, got %q", cfg.OutputDir)
	}
	if cfg.Format != "hugo" {
		t.Errorf("expected hugo format, got %q", cfg.Format)
	}
	if len(cfg.Generators) != 1 || cfg.Generators[0] != "api" {
		t.Errorf("expected [api] generators, got %v", cfg.Generators)
	}
}

func TestAPIGeneratorProducesIndexAndServiceDocs(t *testing.T) {
	ctx := &Context{
		ProjectConfig: &config.ProjectConfig{Name: "test-project"},
		Format:        "markdown",
		Services: []codegen.ServiceDef{
			{
				Name:      "UserService",
				Package:   "user.v1",
				ProtoFile: "proto/services/user/v1/user.proto",
				Methods: []codegen.Method{
					{Name: "GetUser", InputType: "GetUserRequest", OutputType: "GetUserResponse"},
					{Name: "ListUsers", InputType: "ListUsersRequest", OutputType: "ListUsersResponse", ServerStreaming: true},
				},
			},
		},
	}

	g := &APIGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	if len(docs) != 2 {
		t.Fatalf("expected 2 docs (index + 1 service), got %d", len(docs))
	}

	// Check index
	if docs[0].Path != "api/index.md" {
		t.Errorf("expected index path, got %q", docs[0].Path)
	}
	indexContent := string(docs[0].Content)
	if !strings.Contains(indexContent, "UserService") {
		t.Error("expected index to contain UserService")
	}
	if !strings.Contains(indexContent, "user.v1") {
		t.Error("expected index to contain package name")
	}

	// Check service doc
	if docs[1].Path != "api/userservice.md" {
		t.Errorf("expected service path, got %q", docs[1].Path)
	}
	svcContent := string(docs[1].Content)
	if !strings.Contains(svcContent, "GetUser") {
		t.Error("expected service doc to contain GetUser method")
	}
	if !strings.Contains(svcContent, "Server Streaming") {
		t.Error("expected service doc to mention Server Streaming for ListUsers")
	}
	if !strings.Contains(svcContent, "Unary") {
		t.Error("expected service doc to mention Unary for GetUser")
	}
}

func TestAPIGeneratorNoServices(t *testing.T) {
	ctx := &Context{Format: "markdown"}
	g := &APIGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if docs != nil {
		t.Fatalf("expected nil docs for no services, got %d", len(docs))
	}
}

func TestAPIGeneratorHugoFormat(t *testing.T) {
	ctx := &Context{
		ProjectConfig: &config.ProjectConfig{Name: "test"},
		Format:        "hugo",
		Services: []codegen.ServiceDef{
			{Name: "EchoService", Package: "echo.v1", ProtoFile: "echo.proto"},
		},
	}

	g := &APIGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	indexContent := string(docs[0].Content)
	if !strings.Contains(indexContent, "title: \"API Reference\"") {
		t.Error("expected Hugo front matter in index")
	}
}

func TestArchitectureGeneratorProducesMermaid(t *testing.T) {
	ctx := &Context{
		ProjectConfig: &config.ProjectConfig{
			Name: "test-project",
			Services: []config.ServiceConfig{
				{Name: "api-gateway", Type: "go_service", Port: 8080},
				{Name: "user-service", Type: "go_service", Port: 8081},
			},
			Frontends: []config.FrontendConfig{
				{Name: "web", Type: "nextjs", Port: 3000},
			},
			Database: config.DatabaseConfig{Driver: "postgres"},
		},
		Format: "markdown",
	}

	g := &ArchitectureGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	content := string(docs[0].Content)
	if !strings.Contains(content, "mermaid") {
		t.Error("expected Mermaid diagram block")
	}
	if !strings.Contains(content, "api-gateway") {
		t.Error("expected service in diagram")
	}
	if !strings.Contains(content, "web") {
		t.Error("expected frontend in diagram")
	}
}

func TestArchitectureGeneratorNilConfig(t *testing.T) {
	ctx := &Context{Format: "markdown"}
	g := &ArchitectureGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if docs != nil {
		t.Fatal("expected nil docs for nil config")
	}
}

func TestConfigGeneratorProducesReference(t *testing.T) {
	ctx := &Context{
		ProjectConfig: &config.ProjectConfig{Name: "test"},
		Format:        "markdown",
		ConfigMessages: []codegen.ConfigMessage{
			{
				Name: "AppConfig",
				Fields: []codegen.ConfigField{
					{
						Name:         "database_url",
						GoName:       "DatabaseURL",
						GoType:       "string",
						EnvVar:       "DATABASE_URL",
						Flag:         "database-url",
						DefaultValue: "postgres://localhost:5432/app",
						Required:     true,
						Description:  "PostgreSQL connection string",
					},
					{
						Name:    "port",
						GoName:  "Port",
						GoType:  "int32",
						EnvVar:  "PORT",
						Flag:    "port",
						Required: false,
						DefaultValue: "8080",
						Description:  "Server listen port",
					},
				},
			},
		},
	}

	g := &ConfigGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	content := string(docs[0].Content)
	if !strings.Contains(content, "DATABASE_URL") {
		t.Error("expected DATABASE_URL in config reference")
	}
	if !strings.Contains(content, "AppConfig") {
		t.Error("expected AppConfig section")
	}
	if !strings.Contains(content, "--port") {
		t.Error("expected --port flag")
	}
}

func TestConfigGeneratorNoMessages(t *testing.T) {
	ctx := &Context{Format: "markdown"}
	g := &ConfigGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if docs != nil {
		t.Fatal("expected nil docs for no config messages")
	}
}

func TestContractGeneratorProducesDocs(t *testing.T) {
	ctx := &Context{
		ProjectConfig: &config.ProjectConfig{Name: "test"},
		Format:        "markdown",
		Contracts: []*ContractInfo{
			{
				PackageName: "auth",
				Contract: &contract.ContractFile{
					Package: "auth",
					Interfaces: []contract.InterfaceDef{
						{
							Name: "Service",
							Methods: []contract.MethodDef{
								{
									Name: "Authenticate",
									Params: []contract.ParamDef{
										{Name: "ctx", TypeExpr: "context.Context"},
										{Name: "token", TypeExpr: "string"},
									},
									Results: []contract.ParamDef{
										{TypeExpr: "*User"},
										{TypeExpr: "error"},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	g := &ContractGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	if len(docs) != 1 {
		t.Fatalf("expected 1 doc, got %d", len(docs))
	}

	content := string(docs[0].Content)
	if !strings.Contains(content, "auth") {
		t.Error("expected auth package name")
	}
	if !strings.Contains(content, "Authenticate") {
		t.Error("expected Authenticate method")
	}
	if !strings.Contains(content, "context.Context") {
		t.Error("expected context.Context param")
	}
}

func TestContractGeneratorNoContracts(t *testing.T) {
	ctx := &Context{Format: "markdown"}
	g := &ContractGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if docs != nil {
		t.Fatal("expected nil docs for no contracts")
	}
}

func TestWriteDocs(t *testing.T) {
	dir := t.TempDir()

	docs := []GeneratedDoc{
		{Path: "api/index.md", Content: []byte("# API\n")},
		{Path: "architecture.md", Content: []byte("# Arch\n")},
		{Path: "nested/deep/file.md", Content: []byte("# Deep\n")},
	}

	if err := writeDocs(docs, dir); err != nil {
		t.Fatalf("writeDocs() error: %v", err)
	}

	for _, doc := range docs {
		path := filepath.Join(dir, doc.Path)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("could not read %s: %v", doc.Path, err)
		}
		if string(data) != string(doc.Content) {
			t.Errorf("%s: content mismatch", doc.Path)
		}
	}
}

func TestStreamingType(t *testing.T) {
	tests := []struct {
		client, server bool
		want           string
	}{
		{false, false, "Unary"},
		{true, false, "Client Streaming"},
		{false, true, "Server Streaming"},
		{true, true, "Bidirectional Streaming"},
	}

	for _, tt := range tests {
		got := streamingType(tt.client, tt.server)
		if got != tt.want {
			t.Errorf("streamingType(%v, %v) = %q, want %q", tt.client, tt.server, got, tt.want)
		}
	}
}

func TestCustomTemplateOverride(t *testing.T) {
	// Create a custom template dir with an override
	customDir := t.TempDir()
	customContent := `# Custom API Index for {{ .ProjectName }}`
	if err := os.WriteFile(filepath.Join(customDir, "api_index.md.tmpl"), []byte(customContent), 0644); err != nil {
		t.Fatal(err)
	}

	ctx := &Context{
		ProjectConfig: &config.ProjectConfig{
			Name: "test",
			Docs: config.DocsConfig{CustomTemplatesDir: customDir},
		},
		Format: "markdown",
		Services: []codegen.ServiceDef{
			{Name: "TestService", Package: "test.v1", ProtoFile: "test.proto"},
		},
	}

	g := &APIGenerator{}
	docs, err := g.Generate(ctx)
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	indexContent := string(docs[0].Content)
	if !strings.Contains(indexContent, "Custom API Index for test") {
		t.Errorf("expected custom template output, got: %s", indexContent)
	}
}
