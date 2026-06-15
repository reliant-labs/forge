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

	// kind is no longer written to forge.yaml — it DERIVES from the
	// components.json (and its presence). The scaffold writes the components
	// (or, for a library, omits the file) so the derived kind matches the
	// requested --kind. See writeComponentsFile below.

	// The scaffolded forge.yaml is MINIMAL and GLOBAL-only: identity
	// (name/module), the forge version pin, frontends, and explicit feature
	// overrides. The per-service component entities live in components.json
	// (written below); everything else — the features: block and the
	// database/ci/lint/contracts/auth/deploy/docker/k8s sections — is derived
	// from the project shape at load time (config.ApplyDerivedDefaults). Any
	// of those keys remain valid in forge.yaml as overrides; they're just not
	// required boilerplate. Explicit user choices (e.g. `forge new --disable
	// ci`) are recorded in Features below and survive the write-time
	// normalization because they differ from the derived default.
	cfg := config.ProjectConfig{
		Name:         g.Name,
		ModulePath:   g.ModulePath,
		Binary:       binaryYAML,
		ForgeVersion: buildinfo.Version(),
		Features:     g.Features,
	}

	// Build the components, then write them to components.json. Kind sync:
	// cfg.Kind is set from the requested kind so NormalizeForWrite's feature
	// derivation (which reads kind) drops the right kind-default falses.
	cfg.Kind = g.effectiveKind()
	var components []config.ComponentConfig
	if g.ServiceName != "" && g.isService() {
		components = append(components, config.ComponentConfig{
			Name:  g.ServiceName,
			Kind:  config.ComponentKindServer,
			Path:  fmt.Sprintf("handlers/%s", naming.ServicePackage(g.ServiceName)),
			Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: g.ServicePort}},
		})
		// In binary=shared, write ALL server components at scaffold time so
		// the generated bootstrap.go and per-service cobra subcommands see
		// them immediately. In per-service mode this is done post-scaffold
		// via AppendServiceToConfig (preserves the additive flow + log output).
		if g.isBinaryShared() {
			for i, svcName := range g.AdditionalServices {
				components = append(components, config.ComponentConfig{
					Name:  svcName,
					Kind:  config.ComponentKindServer,
					Path:  fmt.Sprintf("handlers/%s", naming.ServicePackage(svcName)),
					Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: g.ServicePort + i + 1}},
				})
			}
		}
	}
	// A cli project's cobra main IS a binary-kind component: it's what makes
	// the project derive to "cli" (binary-only) rather than "library" (no
	// components) on reload. Named after the project, pointing at its cmd
	// main. Library projects deliberately get NO components — the absence of
	// components.json is the library signal.
	if g.isCLI() {
		components = append(components, config.ComponentConfig{
			Name: naming.ServicePackage(g.Name),
			Kind: config.ComponentKindBinary,
			// The cli main is a standalone cobra binary at cmd/<name>/main.go
			// (distinct from a `forge add binary` subcommand at cmd/<name>.go).
			Path: fmt.Sprintf("cmd/%s", naming.ServicePackage(g.Name)),
		})
	}
	cfg.Components = components

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
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return err
	}

	// Write the per-component source of truth, components.json. Library
	// projects (no components) deliberately get NO file: its absence is the
	// "library" kind signal on reload. Service shells write an empty
	// `{"components": []}` (file present, zero entries) so the project still
	// derives to "service"; cli/service-with-components write their entries.
	if !g.isLibrary() {
		if err := WriteComponentsFile(g.Path, cfg.Components); err != nil {
			return fmt.Errorf("write %s: %w", config.ComponentsFileName, err)
		}
	}
	return nil
}

// ReadProjectConfig reads a forge.yaml from the given path with strict
// validation: unknown keys, missing required fields, and type mismatches
// are surfaced together via config.ValidationError rather than failing
// fast on the first issue. The per-component entities are read from the
// project-root components.json sibling of forge.yaml and the project kind
// is derived from them (see config.LoadProject). See config.LoadStrict for
// the full validation semantics.
func ReadProjectConfig(path string) (*config.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project config: %w", err)
	}
	componentsJSON, err := readComponentsJSON(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return config.LoadProject(data, componentsJSON, path)
}

// readComponentsJSON reads the project-root components.json from dir (the
// directory holding forge.yaml). A missing file is not an error: it returns
// nil bytes, treated by config.LoadProject as "no components".
func readComponentsJSON(dir string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, config.ComponentsFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", config.ComponentsFileName, err)
	}
	return data, nil
}

// WriteComponentsFile writes the project's components to the project-root
// components.json (the authored per-service source of truth). Components are
// sorted by name for a stable, diff-friendly file. This is the write path
// for `forge add service|worker|cron|operator|binary` and `forge new`.
func WriteComponentsFile(projectRoot string, components []config.ComponentConfig) error {
	data, err := config.MarshalComponentsJSON(config.SortComponentsForWrite(components))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(projectRoot, config.ComponentsFileName), data, 0o644)
}

// AppendComponentToFile reads components.json at projectRoot, appends the
// entry, and writes it back. Used by the `forge add` write paths so a new
// component lands in components.json (NOT forge.yaml, which is global-only).
func AppendComponentToFile(projectRoot string, entry config.ComponentConfig) error {
	data, err := readComponentsJSON(projectRoot)
	if err != nil {
		return err
	}
	existing, err := config.ParseComponentsJSON(data)
	if err != nil {
		return err
	}
	return WriteComponentsFile(projectRoot, append(existing, entry))
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

// AppendServiceToConfig appends a new server component to the project-root
// components.json (the authored per-service source of truth). Components
// moved out of forge.yaml in the ProjectStore per-service data move, so this
// no longer touches forge.yaml.
func AppendServiceToConfig(projectRoot, serviceName string, port int) error {
	return AppendComponentToFile(projectRoot, config.ComponentConfig{
		Name:  serviceName,
		Kind:  config.ComponentKindServer,
		Path:  fmt.Sprintf("handlers/%s", naming.ServicePackage(serviceName)),
		Ports: map[string]config.PortSpec{config.HTTPPortName: {Port: port}},
	})
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
