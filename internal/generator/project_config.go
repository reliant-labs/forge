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
	// Persist `binary:` only when explicitly opted-in (shared) so existing
	// forge.yaml files keep their cleaner shape and the field is omitted by
	// default. EffectiveBinary on the read-side defaults this to per-service.
	binaryYAML := ""
	if g.isBinaryShared() {
		binaryYAML = config.ProjectBinaryShared
	}

	// The scaffolded forge.yaml is MINIMAL and PROJECT-GLOBAL only: identity
	// (name/module), the forge version pin, frontends, and explicit feature
	// overrides. What the project CONTAINS is not written here at all — the
	// scaffold writes the code (protos, handlers, cmd/) and every later read
	// discovers the components from it. `kind:` is not written either; it
	// derives from that same tree on load. Everything else — the features:
	// block and the database/ci/lint/contracts/auth/deploy/docker/k8s
	// sections — is derived from the project shape at load time
	// (config.ApplyDerivedDefaults). Any of those keys remain valid in
	// forge.yaml as overrides; they're just not required boilerplate.
	// Explicit user choices (e.g. `forge project new --disable ci`) are
	// recorded in Features below and survive the write-time normalization
	// because they differ from the derived default.
	cfg := config.ProjectConfig{
		Name:         g.Name,
		ModulePath:   g.ModulePath,
		Binary:       binaryYAML,
		ForgeVersion: buildinfo.Version(),
		Features:     g.Features,
		// Greenfield projects have no legacy env-reading debt, so scaffold
		// the typed-access guardrail in its strict, gating form. NOTE: this
		// is DIFFERENT from the schema default for an ABSENT key, which is
		// "warn" (so existing projects upgrade without a flag-day). The
		// explicit "error" survives NormalizeForWrite (it is not a section
		// default) and renders the `config:` block into forge.yaml.
		Config: config.ConfigGuardConfig{
			EnforceTypedAccess: config.EnforceTypedAccessError,
		},
	}

	// Kind sync: cfg.Kind is set from the requested kind so
	// NormalizeForWrite's feature derivation (which reads kind) drops the
	// right kind-default falses. It is not serialized — the reader derives
	// kind from the tree this scaffold is writing.
	cfg.Kind = g.effectiveKind()

	if g.FrontendName != "" {
		cfg.Frontends = []config.FrontendConfig{
			{
				Name: g.FrontendName,
				Type: "nextjs",
				Path: fmt.Sprintf("frontends/%s", g.FrontendName),
				// g.FrontendPort is 0 for a fresh scaffold (ephemeral): with
				// FrontendConfig.Port omitempty this writes NO `port:` line, so
				// `forge run`/`up` allocate a free port at launch and report it
				// — two dev stacks never fight for the frontend port. An
				// explicit override (>0) is serialized verbatim.
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
	// database/ci/lint/contracts sections and the features: block are
	// derived from the project shape at load time and only need to appear
	// here when overriding a default. Authentication is OWNED CODE, not
	// config: it is internal/app/auth.go's SetupAuth.
	header := []byte("# Forge project manifest — see https://github.com/reliant-labs/forge.\n" +
		"# This file is minimal on purpose: database, ci, lint, contracts and\n" +
		"# the features: block are derived from the project shape. Authentication\n" +
		"# is owned code, not config — it is internal/app/auth.go's SetupAuth.\n" +
		"# Add a derived key only to override a default\n" +
		"# (`forge skill load architecture` documents the schema;\n" +
		"# `forge skill list` indexes the per-topic skills, e.g. `auth`).\n\n")
	data = append(header, data...)

	destPath := filepath.Join(g.Path, "forge.yaml")
	if err := os.WriteFile(destPath, data, 0644); err != nil {
		return err
	}

	return nil
}

// resolveBinaryName returns the primary binary name (the cmd/<bin>/ leaf the
// command tree lives under) for the project at projectDir. It is the
// forge.yaml project name VERBATIM (hyphens preserved), falling back to the
// project directory's base name when the config is unreadable — mirroring
// ProjectGenerator.binaryName() so the upgrade / regenerate lane addresses the
// SAME cmd/<bin>/ dir the scaffold wrote (a project named "peptide-platform"
// lives at cmd/peptide-platform/).
func resolveBinaryName(projectDir string) string {
	cfg, err := ReadProjectConfig(filepath.Join(projectDir, "forge.yaml"))
	if err == nil && cfg != nil && cfg.Name != "" {
		return cfg.Name
	}
	return filepath.Base(projectDir)
}

// ReadProjectConfig reads a forge.yaml from the given path with strict
// validation: unknown keys, missing required fields, and type mismatches
// are surfaced together via config.ValidationError rather than failing
// fast on the first issue. forge.yaml is project-global only: what the
// project CONTAINS is read from the code that declares it
// (codegen.DiscoverProjectComponents), and the project kind is derived from
// the tree beside forge.yaml (see config.LoadProject). See config.LoadStrict
// for the full validation semantics.
func ReadProjectConfig(path string) (*config.ProjectConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read project config: %w", err)
	}
	// The path is load-bearing: LoadProject derives the kind from the real
	// sources sitting beside this forge.yaml (see config.LoadProject).
	return config.LoadProject(data, path)
}

// WriteProjectConfigFile writes a config.ProjectConfig to the given path.
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
