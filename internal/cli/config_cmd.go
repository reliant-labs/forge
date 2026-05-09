package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/cliutil"
)

// newConfigCmd builds the `forge config` subcommand surface. Today the
// only verb is `set`, which programmatically edits forge.yaml's
// environments[<env>].config[<key>] = <value> map without forcing the
// caller to round-trip YAML by hand. The motivating use case is LLM /
// scripted edits where YAML whitespace fragility (key indentation, new
// vs. existing environment block, list-vs-mapping under environments)
// produced silent errors during the control-plane-next port. Type
// validation (against proto/config/v1/config.proto's field annotations,
// when present) is a best-effort guard so a typo like
// `--env dev port not-a-number` fails up front instead of at startup.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Edit per-environment runtime config (forge.yaml environments[].config)",
		Long: `Programmatically edit forge.yaml's environments[<env>].config map.

The config map carries per-environment values for fields declared in
proto/config/v1/config.proto (port, log_level, database_url, ...). Two
storage shapes are supported:

  1. Inline under environments[].config in forge.yaml (good for dev /
     staging where values aren't sensitive)
  2. A sibling file config.<env>.yaml next to forge.yaml (good for prod
     where values mix secret refs with toggles)

` + "`forge config set`" + ` always edits the inline shape. For sensitive values
pass a ${secret-name#secret-key} reference rather than the cleartext.`,
	}
	cmd.AddCommand(newConfigSetCmd())
	return cmd
}

func newConfigSetCmd() *cobra.Command {
	var (
		envName  string
		unsetKey bool
	)
	cmd := &cobra.Command{
		Use:   "set <key> [value]",
		Short: "Set or unset environments[<env>].config[<key>] in forge.yaml",
		Long: `Edit forge.yaml's environments[<env>].config[<key>] entry without
hand-formatting YAML.

Behaviour:
  - Type-checks <value> against proto/config/v1/config.proto's field
    annotation (when the field is declared there) — int/bool fields
    reject non-numeric / non-bool strings up front.
  - Accepts ${secret-name#secret-key} references for sensitive values;
    the reference shape is validated but the secret is NOT resolved.
  - Creates the environment block on demand if --env names an
    environment not yet present in forge.yaml.
  - --unset removes the key from the config map (and removes the map
    entirely if it becomes empty).

Examples:
  forge config set --env dev log_level debug
  forge config set --env prod port 9090
  forge config set --env prod database_url '${prod-db#dsn}'
  forge config set --env dev --unset auto_migrate`,
		Args: func(cmd *cobra.Command, args []string) error {
			if unsetKey {
				if len(args) != 1 {
					return cliutil.UserErr("forge config set --unset",
						"requires exactly one positional argument: <key>",
						"",
						"call as 'forge config set --env <env> --unset <key>'")
				}
				return nil
			}
			if len(args) != 2 {
				return cliutil.UserErr("forge config set",
					"requires two positional arguments: <key> <value>",
					"",
					"call as 'forge config set --env <env> <key> <value>' (or pass --unset <key> to remove)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if envName == "" {
				return cliutil.UserErr("forge config set",
					"--env is required",
					"",
					"pass --env <name> matching an environments[] entry in forge.yaml (the env block is created if missing)")
			}
			key := args[0]
			value := ""
			if !unsetKey {
				value = args[1]
			}
			return runConfigSet(envName, key, value, unsetKey)
		},
	}
	cmd.Flags().StringVar(&envName, "env", "", "Environment name (must match an environments[].name entry; created if missing)")
	cmd.Flags().BoolVar(&unsetKey, "unset", false, "Remove the key instead of setting it")
	return cmd
}

// runConfigSet locates forge.yaml, edits the environments[<env>].config
// map, and writes the file back. The yaml.Node round-trip preserves
// comments and key order in unrelated parts of the file — only the
// targeted env's config map is mutated.
func runConfigSet(envName, key, rawValue string, unset bool) error {
	if !validConfigKey(key) {
		return cliutil.UserErr(fmt.Sprintf("forge config set --env %s", envName),
			fmt.Sprintf("invalid config key %q (must match [a-z][a-z0-9_]*)", key),
			"",
			"use snake_case starting with a lowercase letter (e.g. log_level, database_url)")
	}

	configPath, err := findProjectConfigFile()
	if err != nil {
		return err
	}

	// Type-check the value against proto/config/v1/config.proto, when the
	// project ships one. Best-effort: if the proto isn't present (CLI/library
	// kinds, or a project that hasn't generated AppConfig yet), we accept the
	// raw string and let the runtime parser surface a mismatch.
	var coerced any
	if !unset {
		typed, err := coerceConfigValue(filepath.Dir(configPath), key, rawValue)
		if err != nil {
			return err
		}
		coerced = typed
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("read forge.yaml: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("parse forge.yaml: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return fmt.Errorf("forge.yaml: expected a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("forge.yaml: expected top-level mapping")
	}

	envsNode := mappingChild(root, "environments")
	if envsNode == nil {
		envsNode = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		root.Content = append(root.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "environments"},
			envsNode,
		)
	}
	if envsNode.Kind != yaml.SequenceNode {
		return fmt.Errorf("forge.yaml: `environments` must be a sequence (got kind=%v)", envsNode.Kind)
	}

	envEntry := findEnvNode(envsNode, envName)
	if envEntry == nil {
		if unset {
			// Nothing to remove if the env doesn't exist — quiet success.
			fmt.Printf("Environment %q not found; nothing to unset.\n", envName)
			return nil
		}
		envEntry = newEnvEntryNode(envName)
		envsNode.Content = append(envsNode.Content, envEntry)
	}

	configNode := mappingChild(envEntry, "config")
	if configNode == nil {
		if unset {
			fmt.Printf("environments[%q].config is empty; nothing to unset.\n", envName)
			return nil
		}
		configNode = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		envEntry.Content = append(envEntry.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "config"},
			configNode,
		)
	}
	if configNode.Kind != yaml.MappingNode {
		return fmt.Errorf("environments[%q].config must be a mapping (got kind=%v)", envName, configNode.Kind)
	}

	if unset {
		removed := removeMappingEntry(configNode, key)
		if !removed {
			fmt.Printf("Key %q not present under environments[%q].config; nothing to unset.\n", key, envName)
			return nil
		}
		// Drop the now-empty config node so the file doesn't accumulate
		// stub `config: {}` entries after a series of unsets.
		if len(configNode.Content) == 0 {
			removeMappingEntry(envEntry, "config")
		}
	} else {
		setMappingScalar(configNode, key, coerced)
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal forge.yaml: %w", err)
	}
	if err := os.WriteFile(configPath, out, 0o644); err != nil {
		return fmt.Errorf("write forge.yaml: %w", err)
	}

	if unset {
		fmt.Printf("Unset environments[%q].config[%q] in %s\n", envName, key, configPath)
	} else {
		fmt.Printf("Set environments[%q].config[%q] = %v in %s\n", envName, key, coerced, configPath)
	}
	return nil
}

// validConfigKey returns true if name is a plausible proto field name
// (snake_case starting with a lowercase letter). The conservative shape
// avoids YAML keys that need quoting and rejects accidental flag-like
// inputs (e.g. `--port`).
var configKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func validConfigKey(s string) bool { return configKeyPattern.MatchString(s) }

// secretRefPattern recognises ${secret-name#secret-key} references. We
// validate the *shape* of the reference (so a typo like `${prod_db}` —
// missing #key — is caught), but never attempt to resolve the secret.
var secretRefPattern = regexp.MustCompile(`^\$\{[A-Za-z0-9._-]+#[A-Za-z0-9._-]+\}$`)

// coerceConfigValue type-checks rawValue against the field declared in
// proto/config/v1/config.proto (when present), returning the canonical
// Go-typed value to write into the YAML map. Secret references and
// missing proto annotations short-circuit to string.
func coerceConfigValue(projectDir, key, rawValue string) (any, error) {
	if secretRefPattern.MatchString(rawValue) {
		// Sensitive fields can carry secret refs verbatim; no type check.
		return rawValue, nil
	}

	protoType, err := lookupProtoFieldType(projectDir, key)
	if err != nil {
		// Non-fatal: project may not own a config.proto yet (CLI/library
		// kinds, or a half-bootstrapped project). Fall back to string.
		return rawValue, nil
	}
	if protoType == "" {
		return rawValue, nil
	}

	switch protoType {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64", "fixed32", "fixed64":
		n, err := strconv.ParseInt(rawValue, 10, 64)
		if err != nil {
			return nil, cliutil.UserErr("forge config set",
				fmt.Sprintf("config key %q is %s in proto; value %q is not a valid integer", key, protoType, rawValue),
				"proto/config/v1/config.proto",
				"pass a numeric value, or change the proto field type if the key should accept strings")
		}
		return n, nil
	case "float", "double":
		n, err := strconv.ParseFloat(rawValue, 64)
		if err != nil {
			return nil, cliutil.UserErr("forge config set",
				fmt.Sprintf("config key %q is %s in proto; value %q is not a valid number", key, protoType, rawValue),
				"proto/config/v1/config.proto",
				"pass a numeric value (e.g. 1.5), or change the proto field type")
		}
		return n, nil
	case "bool":
		b, err := strconv.ParseBool(rawValue)
		if err != nil {
			return nil, cliutil.UserErr("forge config set",
				fmt.Sprintf("config key %q is bool in proto; value %q is not a valid bool", key, rawValue),
				"proto/config/v1/config.proto",
				"pass true or false")
		}
		return b, nil
	default:
		return rawValue, nil
	}
}

// protoFieldDeclPattern matches a top-level scalar field declaration in
// AppConfig: e.g. `int32 port = 1 [...]`. We use a lightweight regex
// rather than a proto parser because the CLI runs without a buf
// dependency and we only need the type token for known scalar fields.
var protoFieldDeclPattern = regexp.MustCompile(`^\s*(\w+)\s+([a-z][a-z0-9_]*)\s*=\s*\d+`)

// lookupProtoFieldType scans proto/config/v1/config.proto (best-effort)
// for a field named `name` and returns its declared type token. Returns
// ("", nil) when the proto exists but doesn't declare the field, and an
// error only when the file can't be read.
func lookupProtoFieldType(projectDir, name string) (string, error) {
	path := filepath.Join(projectDir, "proto", "config", "v1", "config.proto")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		m := protoFieldDeclPattern.FindStringSubmatch(line)
		if len(m) != 3 {
			continue
		}
		typeTok := m[1]
		fieldName := m[2]
		// Skip syntax/option/import lines that share the regex shape.
		if typeTok == "option" || typeTok == "syntax" || typeTok == "import" || typeTok == "package" {
			continue
		}
		if fieldName == name {
			return typeTok, nil
		}
	}
	return "", nil
}

// mappingChild returns the value node for `key` in a mapping node, or
// nil if absent. yaml.Node represents a mapping as alternating key/value
// children; this helper hides that layout from callers.
func mappingChild(parent *yaml.Node, key string) *yaml.Node {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			return parent.Content[i+1]
		}
	}
	return nil
}

// findEnvNode walks an environments sequence and returns the entry whose
// `name` scalar matches envName.
func findEnvNode(seq *yaml.Node, envName string) *yaml.Node {
	for _, entry := range seq.Content {
		if entry.Kind != yaml.MappingNode {
			continue
		}
		nameNode := mappingChild(entry, "name")
		if nameNode != nil && nameNode.Kind == yaml.ScalarNode && nameNode.Value == envName {
			return entry
		}
	}
	return nil
}

// newEnvEntryNode constructs a fresh environments[] mapping with
// `name: <envName>` set. Callers append config entries via
// setMappingScalar against the returned node.
func newEnvEntryNode(envName string) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: envName},
		},
	}
}

// removeMappingEntry deletes the `key` entry from a mapping node and
// reports whether anything was removed. Mapping content is laid out
// alternately, so we must remove both the key and value nodes.
func removeMappingEntry(parent *yaml.Node, key string) bool {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
			return true
		}
	}
	return false
}

// setMappingScalar inserts or updates `key: value` in a mapping. Value
// kinds are inferred so that integers / bools render as YAML scalars
// without quotes (preserving the proto-typed shape).
func setMappingScalar(parent *yaml.Node, key string, value any) {
	valueNode := scalarNodeFor(value)
	for i := 0; i+1 < len(parent.Content); i += 2 {
		k := parent.Content[i]
		if k.Kind == yaml.ScalarNode && k.Value == key {
			parent.Content[i+1] = valueNode
			return
		}
	}
	parent.Content = append(parent.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		valueNode,
	)
}

// scalarNodeFor renders the Go value as a YAML scalar with the right
// tag so int/bool values don't emit as quoted strings.
func scalarNodeFor(v any) *yaml.Node {
	switch x := v.(type) {
	case bool:
		s := "false"
		if x {
			s = "true"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: s}
	case int64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.FormatInt(x, 10)}
	case float64:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: strconv.FormatFloat(x, 'g', -1, 64)}
	case string:
		// Preserve the input verbatim. yaml.Node will quote on emit when
		// needed (special chars / leading whitespace); we don't try to
		// pre-quote because that would double-encode in the round-trip.
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: x}
	default:
		// Fallback: emit via go-yaml's default marshal, then re-parse.
		// Used for unexpected shapes; should not happen in practice but
		// keeps the code total.
		buf, err := yaml.Marshal(v)
		if err != nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%v", v)}
		}
		var n yaml.Node
		_ = yaml.Unmarshal(bytes.TrimSpace(buf), &n)
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			return n.Content[0]
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%v", v)}
	}
}
