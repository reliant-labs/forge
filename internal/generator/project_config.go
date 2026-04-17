package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/config"
)

func (g *ProjectGenerator) writeProjectConfig() error {
	cfg := config.ProjectConfig{
		Name:       g.Name,
		ModulePath: g.ModulePath,
		Version:    "0.1.0",
		HotReload:  true,
		Envs: []config.EnvironmentConfig{
			{Name: "dev", Type: "local"},
			{Name: "staging", Type: "cloud"},
			{Name: "prod", Type: "cloud"},
		},
		Database: config.DatabaseConfig{
			Driver:        "postgres",
			MigrationsDir: "db/migrations",
			SQLCEnabled:   false,
		},
		CI: config.CIConfig{
			Provider: "github",
			Lint: config.CILintConfig{
				Golangci: true,
				Buf:      true,
				Frontend: g.FrontendName != "",
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
			ProtoMethod: true,
			Contract:    true,
		},
		Auth: config.AuthConfig{
			Provider: "none",
		},
	}

	if g.ServiceName != "" {
		cfg.Services = []config.ServiceConfig{
			{
				Name: g.ServiceName,
				Type: "go_service",
				Path: fmt.Sprintf("handlers/%s", g.ServiceName),
				Port: g.ServicePort,
			},
		}
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

	destPath := filepath.Join(g.Path, "forge.project.yaml")
	return os.WriteFile(destPath, data, 0644)
}

// ReadProjectConfig reads a forge.project.yaml from the given path.
func ReadProjectConfig(path string) (*config.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project config: %w", err)
	}
	var cfg config.ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse project config: %w", err)
	}
	return &cfg, nil
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
	configPath := filepath.Join(projectRoot, "forge.project.yaml")
	entry := config.ServiceConfig{
		Name: serviceName,
		Type: "go_service",
		Path: fmt.Sprintf("handlers/%s", serviceName),
		Port: port,
	}
	return appendToProjectConfigSequence(configPath, "services", entry)
}

// AppendFrontendToConfig reads the project config at the given project root,
// appends a new frontend entry, and writes it back. It uses yaml.Node
// round-tripping so that unknown keys, comments, and field ordering added
// by the user are preserved.
func AppendFrontendToConfig(projectRoot, frontendName string, port int) error {
	configPath := filepath.Join(projectRoot, "forge.project.yaml")
	entry := config.FrontendConfig{
		Name: frontendName,
		Type: "nextjs",
		Path: fmt.Sprintf("frontends/%s", frontendName),
		Port: port,
	}
	return appendToProjectConfigSequence(configPath, "frontends", entry)
}

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
