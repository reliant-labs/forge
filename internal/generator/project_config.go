package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
)

func (g *ProjectGenerator) writeProjectConfig() error {
	frontendFramework := "none"
	if g.FrontendName != "" {
		frontendFramework = "nextjs"
	}

	// If frontend was explicitly disabled, override the framework to "none".
	if g.Features.Frontend != nil && !*g.Features.Frontend {
		frontendFramework = "none"
	}

	// Stack/database/deploy fields are service-shaped. CLI/library kinds
	// emit a leaner forge.yaml without the k8s deploy + database blocks
	// so it accurately reflects the project they're describing.
	stack := config.StackConfig{
		Backend:  config.StackBackend{Language: "go"},
		Frontend: config.StackFrontend{Framework: frontendFramework},
		Database: config.StackDatabase{Driver: "postgres"},
		Proto:    config.StackProto{Provider: "buf"},
		Deploy:   config.StackDeploy{Target: "k8s", Provider: "k3d", Registry: "ghcr.io"},
		CI:       config.StackCI{Provider: "github"},
	}
	if !g.isService() {
		// CLI/library: drop deploy + database from the stack — the project
		// has no server to deploy and no DB layer.
		stack.Database = config.StackDatabase{Driver: "none"}
		stack.Deploy = config.StackDeploy{Target: "none"}
		stack.Frontend = config.StackFrontend{Framework: "none"}
	}

	// Persist `binary:` only when explicitly opted-in (shared) so existing
	// forge.yaml files keep their cleaner shape and the field is omitted by
	// default. EffectiveBinary on the read-side defaults this to per-service.
	binaryYAML := ""
	if g.isBinaryShared() {
		binaryYAML = config.ProjectBinaryShared
	}

	cfg := config.ProjectConfig{
		Name:         g.Name,
		ModulePath:   g.ModulePath,
		Kind:         g.effectiveKind(),
		Binary:       binaryYAML,
		Version:      "0.1.0",
		ForgeVersion: buildinfo.Version(),
		HotReload:    g.isService(), // hot-reload only meaningful for long-running servers
		Features:     g.buildFeaturesConfig(),
		Stack:        stack,
		Envs: []config.EnvironmentConfig{
			{
				Name: "dev",
				Type: "local",
				Config: map[string]any{
					"log_level":   "debug",
					"log_format":  "text",
					"environment": "development",
				},
			},
			{
				Name: "staging",
				Type: "cloud",
				Config: map[string]any{
					"log_level":   "info",
					"log_format":  "json",
					"environment": "production",
				},
			},
			{
				Name: "prod",
				Type: "cloud",
				Config: map[string]any{
					"log_level":   "warn",
					"log_format":  "json",
					"environment": "production",
				},
			},
		},
		Database: config.DatabaseConfig{
			Driver:        "postgres",
			MigrationsDir: "db/migrations",
			SQLCEnabled:   false,
			MigrationSafety: config.MigrationSafetyConfig{
				Enabled:           boolPtr(true),
				UnsafeAddColumn:   "error",
				DestructiveChange: "error",
				VolatileDefault:   "warn",
			},
		},
		CI: config.CIConfig{
			Provider: "github",
			Lint: config.CILintConfig{
				Golangci:        true,
				Buf:             true,
				BufBreaking:     true,
				Frontend:        g.FrontendName != "",
				MigrationSafety: true,
			},
			Test: config.CITestConfig{
				Race:     true,
				Coverage: false,
			},
			VulnScan: config.CIVulnConfig{
				Go:     true,
				Docker: true,
				NPM:    g.FrontendName != "",
			},
		},
		Deploy: config.DeployConfig{
			Provider: "github",
			// Zero-value DeployConcurrency means enabled
		},
		Docker: config.DockerConfig{
			Registry: "ghcr.io",
		},
		K8s: config.K8sConfig{
			Provider: "k3d",
			KCLDir:   "deploy/kcl",
		},
		Lint: config.LintConfig{
			Contract: true,
			Frontend: config.FrontendLintConfig{
				CSSHealth:      g.FrontendName != "",
				NoImportant:    "warn",
				NoInlineStyles: "warn",
			},
		},
		// Contracts: strict-by-default — every internal/<pkg>/ that exposes
		// behavior must declare an interface in contract.go. Exported package
		// vars and free funcs are both rejected so all surface goes through
		// the test-seam interface. See MIGRATION_TIPS.md "Contracts-first
		// migration" for why we lock this in at scaffold time rather than
		// asking users to flip it after the fact.
		Contracts: config.ContractsConfig{
			Strict:             true,
			AllowExportedVars:  false,
			AllowExportedFuncs: false,
			Exclude:            []string{},
		},
		Auth: config.AuthConfig{
			Provider: "none",
		},
	}

	if g.ServiceName != "" && g.isService() {
		cfg.Services = []config.ServiceConfig{
			{
				Name: g.ServiceName,
				Type: "go_service",
				Path: fmt.Sprintf("handlers/%s", ServicePackageName(g.ServiceName)),
				Port: g.ServicePort,
			},
		}
		// In binary=shared, write ALL services to forge.yaml at scaffold
		// time so the generated bootstrap.go and per-service cobra
		// subcommands see them immediately. In per-service mode this is
		// done post-scaffold via AppendServiceToConfig (preserves the
		// existing additive flow + log output).
		if g.isBinaryShared() {
			for i, svcName := range g.AdditionalServices {
				cfg.Services = append(cfg.Services, config.ServiceConfig{
					Name: svcName,
					Type: "go_service",
					Path: fmt.Sprintf("handlers/%s", ServicePackageName(svcName)),
					Port: g.ServicePort + i + 1,
				})
			}
		}
	}

	// Strip server-shaped sections from non-service forge.yaml so the
	// emitted file describes the actual project layout.
	if !g.isService() {
		cfg.Database = config.DatabaseConfig{}
		cfg.Deploy = config.DeployConfig{}
		cfg.Docker = config.DockerConfig{}
		cfg.K8s = config.K8sConfig{}
		cfg.CI.Lint.Buf = false
		cfg.CI.Lint.BufBreaking = false
		cfg.CI.Lint.MigrationSafety = false
		cfg.CI.VulnScan.Docker = false
	}

	if g.FrontendName != "" {
		cfg.Frontends = []config.FrontendConfig{
			{
				Name: g.FrontendName,
				Type: "nextjs",
				Path: fmt.Sprintf("frontends/%s", g.FrontendName),
				Port: g.FrontendPort,
			},
		}
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}

	// Prepend a header explaining the shape of this file. In particular the
	// `database:` block is declared unconditionally even when no entity
	// protos exist yet — downstream codegen (`protoc-gen-forge-orm`) reads
	// it when proto/db/*.proto are added later. Until then it's a no-op.
	header := []byte("# Forge project manifest — see https://github.com/reliant-labs/forge.\n" +
		"# `database:` is declared here even if you haven't added any\n" +
		"# proto/db/*.proto entities yet; protoc-gen-forge-orm consults it\n" +
		"# once you do. Leave the defaults in place if you're unsure.\n\n")
	data = append(header, data...)

	destPath := filepath.Join(g.Path, "forge.yaml")
	return os.WriteFile(destPath, data, 0644)
}

func (g *ProjectGenerator) buildFeaturesConfig() config.FeaturesConfig {
	t := boolPtr(true)
	orDefault := func(v *bool) *bool {
		if v != nil {
			return v
		}
		return t
	}
	return config.FeaturesConfig{
		ORM:        orDefault(g.Features.ORM),
		Codegen:    orDefault(g.Features.Codegen),
		Migrations: orDefault(g.Features.Migrations),
		CI:         orDefault(g.Features.CI),
		Deploy:     orDefault(g.Features.Deploy),
		Contracts:  orDefault(g.Features.Contracts),
		Docs:       orDefault(g.Features.Docs),
		Frontend: func() *bool {
			if g.Features.Frontend != nil {
				return g.Features.Frontend
			}
			return boolPtr(g.FrontendName != "")
		}(),
		Observability: orDefault(g.Features.Observability),
		HotReload:     orDefault(g.Features.HotReload),
	}
}

// ReadProjectConfig reads a forge.yaml from the given path with strict
// validation: unknown keys, missing required fields, and type mismatches
// are surfaced together via config.ValidationError rather than failing
// fast on the first issue. See config.LoadStrict for the full semantics.
func ReadProjectConfig(path string) (*config.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project config: %w", err)
	}
	return config.LoadStrict(data, path)
}

// WriteProjectConfig writes a config.ProjectConfig to the given path.
func WriteProjectConfigFile(cfg *config.ProjectConfig, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	return os.WriteFile(path, data, 0644)
}

// AppendServiceToConfig reads the project config at the given project root,
// appends a new service entry, and writes it back. It uses yaml.Node
// round-tripping so that unknown keys, comments, and field ordering added
// by the user are preserved.
func AppendServiceToConfig(projectRoot, serviceName string, port int) error {
	configPath := filepath.Join(projectRoot, "forge.yaml")
	entry := config.ServiceConfig{
		Name: serviceName,
		Type: "go_service",
		Path: fmt.Sprintf("handlers/%s", ServicePackageName(serviceName)),
		Port: port,
	}
	return appendToProjectConfigSequence(configPath, "services", entry)
}

// AppendFrontendToConfig reads the project config at the given project root,
// appends a new frontend entry, and writes it back. It uses yaml.Node
// round-tripping so that unknown keys, comments, and field ordering added
// by the user are preserved.
func AppendFrontendToConfig(projectRoot, frontendName string, port int) error {
	return AppendFrontendToConfigWithKind(projectRoot, frontendName, port, "")
}

// AppendFrontendToConfigWithKind is like AppendFrontendToConfig but accepts a
// kind parameter ("web" or "mobile") to select the frontend type.
func AppendFrontendToConfigWithKind(projectRoot, frontendName string, port int, kind string) error {
	configPath := filepath.Join(projectRoot, "forge.yaml")
	feType := "nextjs"
	if kind == "mobile" {
		feType = "react-native"
	}
	entry := config.FrontendConfig{
		Name: frontendName,
		Type: feType,
		Kind: kind,
		Path: fmt.Sprintf("frontends/%s", frontendName),
		Port: port,
	}
	return appendToProjectConfigSequence(configPath, "frontends", entry)
}

func boolPtr(b bool) *bool { return &b }

// appendToProjectConfigSequence appends entry to the YAML sequence at the
// top-level key on the project config at configPath, preserving any keys,
// comments, and ordering the user added that are not part of the Go struct.
// If the key does not exist, it is created.
func appendToProjectConfigSequence(configPath, key string, entry interface{}) error {
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse project config: %w", err)
	}

	// The document node wraps a single mapping node.
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("project config %s: expected a YAML document", configPath)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("project config %s: expected top-level mapping", configPath)
	}

	// Build the node for the new entry via round-tripping through yaml.Node.
	entryBytes, err := yaml.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal new %s entry: %w", key, err)
	}
	var entryDoc yaml.Node
	if err := yaml.Unmarshal(entryBytes, &entryDoc); err != nil {
		return fmt.Errorf("parse new %s entry: %w", key, err)
	}
	if entryDoc.Kind != yaml.DocumentNode || len(entryDoc.Content) == 0 {
		return fmt.Errorf("unexpected YAML shape for new %s entry", key)
	}
	entryNode := entryDoc.Content[0]

	// Find the sequence node for `key` in the top-level mapping. Mapping
	// nodes store keys and values as alternating children.
	var seq *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		k := root.Content[i]
		v := root.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			seq = v
			break
		}
	}

	if seq == nil {
		// Key does not exist — create an empty sequence and append it.
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			seq,
		)
	} else if seq.Kind != yaml.SequenceNode {
		// The key is present but set to null/empty — replace with a sequence.
		seq.Kind = yaml.SequenceNode
		seq.Tag = "!!seq"
		seq.Value = ""
	}

	seq.Content = append(seq.Content, entryNode)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}
	return os.WriteFile(configPath, out, 0644)
}
