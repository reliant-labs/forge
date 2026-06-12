package generator

import (
	"fmt"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

func (g *ProjectGenerator) writeProjectConfig() error {
	// Persist `binary:` only when explicitly opted-in (shared) so existing
	// forge.yaml files keep their cleaner shape and the field is omitted by
	// default. EffectiveBinary on the read-side defaults this to per-service.
	binaryYAML := ""
	if g.isBinaryShared() {
		binaryYAML = config.ProjectBinaryShared
	}

	// `kind:` is only written for the non-default kinds — absent means
	// "service" via EffectiveProjectKind, same as before.
	kindYAML := ""
	if g.effectiveKind() != config.ProjectKindService {
		kindYAML = g.effectiveKind()
	}

	// The scaffolded forge.yaml is MINIMAL: identity (name/module/kind),
	// the forge version pin, and the component lists (services/frontends).
	// Everything else — the features: block and the database/ci/lint/
	// contracts/auth/deploy/docker/k8s sections — is derived from this
	// shape at load time (config.ApplyDerivedDefaults) with exactly the
	// values the scaffold used to write out explicitly. Any of those keys
	// remain valid in forge.yaml as overrides; they're just not required
	// boilerplate. Explicit user choices (e.g. `forge new --disable ci`)
	// are recorded in Features below and survive the write-time
	// normalization because they differ from the derived default.
	cfg := config.ProjectConfig{
		Name:         g.Name,
		ModulePath:   g.ModulePath,
		Kind:         kindYAML,
		Binary:       binaryYAML,
		ForgeVersion: buildinfo.Version(),
		Features:     g.Features,
	}

	if g.ServiceName != "" && g.isService() {
		cfg.Services = []config.ServiceConfig{
			{
				Name: g.ServiceName,
				Type: "go_service",
				Path: fmt.Sprintf("handlers/%s", naming.ServicePackage(g.ServiceName)),
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
					Path: fmt.Sprintf("handlers/%s", naming.ServicePackage(svcName)),
					Port: g.ServicePort + i + 1,
				})
			}
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

	// Persist the project-level frontend.workspaces flag when opted-in
	// so subsequent `forge generate` runs know to maintain the pnpm-
	// workspace layout. When false the field is omitted thanks to
	// `omitempty` on FrontendProjectConfig.Workspaces — keeps forge.yaml
	// byte-identical to projects scaffolded before the flag existed.
	if g.FrontendWorkspaces {
		cfg.Frontend = config.FrontendProjectConfig{Workspaces: true}
	}

	// Normalize before marshalling: feature flags and section values that
	// match the shape-derived defaults are dropped, so what hits disk is
	// only identity + components + explicit user choices. Kind-default
	// feature falses set by ApplyKindFeatureDefaults (cli/library) match
	// derivation and disappear; `--disable` choices differ and survive.
	data, err := yaml.Marshal(config.NormalizeForWrite(&cfg))
	if err != nil {
		return fmt.Errorf("marshal project config: %w", err)
	}

	// Prepend a short header. The file is intentionally minimal — the
	// database/ci/lint/contracts/auth sections and the features: block
	// are derived from the project shape at load time and only need to
	// appear here when overriding a default.
	header := []byte("# Forge project manifest — see https://github.com/reliant-labs/forge.\n" +
		"# This file is minimal on purpose: database, ci, lint, contracts,\n" +
		"# auth and the features: block are derived from the project shape.\n" +
		"# Add any of those keys only to override a derived default\n" +
		"# (`forge skill load forge` documents the full schema).\n\n")
	data = append(header, data...)

	destPath := filepath.Join(g.Path, "forge.yaml")
	return os.WriteFile(destPath, data, 0644)
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
// The config is normalized first (config.NormalizeForWrite): values that
// match their shape-derived defaults are dropped so load → mutate →
// write round-trips keep forge.yaml minimal instead of materializing
// every derived default back into the file. Explicit overrides (values
// differing from derivation) always survive.
func WriteProjectConfigFile(cfg *config.ProjectConfig, path string) error {
	data, err := yaml.Marshal(config.NormalizeForWrite(cfg))
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
		Path: fmt.Sprintf("handlers/%s", naming.ServicePackage(serviceName)),
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
func appendToProjectConfigSequence(configPath, key string, entry any) error {
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
