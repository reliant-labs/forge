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
// only verb is `set`, which programmatically edits the sibling
// `config.<env>.yaml` file (next to forge.yaml) without forcing the
// caller to round-trip YAML by hand. The motivating use case is LLM /
// scripted edits where YAML whitespace fragility (key indentation,
// quoting of secret refs, accidental flow-style maps) produced silent
// errors during the control-plane-next port. Type validation (against
// proto/config/v1/config.proto's field annotations, when present) is a
// best-effort guard so a typo like `--env dev port not-a-number` fails
// up front instead of at startup.
func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Edit per-environment runtime config (config.<env>.yaml sibling files)",
		Long: `Programmatically edit a project's per-environment config.<env>.yaml.

Per-env app config (runtime AppConfig values keyed by snake_case proto
field names from proto/config/v1/config.proto — port, log_level,
database_url, ...) lives in sibling files next to forge.yaml:

  config.dev.yaml
  config.staging.yaml
  config.prod.yaml

Each file is a flat top-level YAML mapping of <key>: <value> entries,
loaded by forge run / forge deploy via internal/config.LoadEnvironmentConfig.

` + "`forge config set`" + ` edits these sibling files. It never touches
forge.yaml — the legacy ` + "`environments[].config`" + ` inline shape was
removed in the KCL-canonical refactor.

For sensitive values pass a ${secret-name#secret-key} reference rather
than cleartext; the reference shape is validated but the secret is not
resolved.`,
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
		Short: "Set or unset <key> in config.<env>.yaml",
		Long: `Edit the sibling config.<env>.yaml file (next to forge.yaml) by
setting or unsetting a top-level <key>: <value> entry.

Behaviour:
  - Type-checks <value> against proto/config/v1/config.proto's field
    annotation (when the field is declared there) — int/bool fields
    reject non-numeric / non-bool strings up front.
  - Accepts ${secret-name#secret-key} references for sensitive values;
    the reference shape is validated but the secret is NOT resolved.
  - Creates config.<env>.yaml on demand if it doesn't exist yet, with
    a header comment describing what the file is for.
  - --unset removes the key (and leaves the now-empty file in place so
    later sets stay deterministic; never modifies forge.yaml).

Examples:
  forge config set --env dev log_level debug     # writes to config.dev.yaml
  forge config set --env prod port 9090          # writes to config.prod.yaml
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
					"pass --env <name> (the sibling file config.<name>.yaml is created if missing)")
			}
			key := args[0]
			value := ""
			if !unsetKey {
				value = args[1]
			}
			return runConfigSet(envName, key, value, unsetKey)
		},
	}
	cmd.Flags().StringVar(&envName, "env", "", "Environment name (selects config.<env>.yaml sibling file; created if missing)")
	cmd.Flags().BoolVar(&unsetKey, "unset", false, "Remove the key instead of setting it")
	return cmd
}

// configEnvFileHeader is the comment block prepended to a freshly
// created config.<env>.yaml. It mirrors the wording in env_loader.go's
// doc comment so consumers (humans, LLMs) can orient themselves without
// jumping to the Go source.
const configEnvFileHeader = `# config.%s.yaml — per-environment runtime config for the %q environment.
#
# Flat top-level mapping of <key>: <value> entries, keyed by snake_case
# field names declared in proto/config/v1/config.proto (port, log_level,
# database_url, ...). Loaded by forge run / forge deploy via
# internal/config.LoadEnvironmentConfig. Sensitive values may be a
# ${secret-name#secret-key} reference instead of cleartext.
#
# Edit by hand or via 'forge config set --env %s <key> <value>'.
`

// runConfigSet locates forge.yaml (to anchor the project directory),
// then edits the sibling config.<env>.yaml file. The yaml.Node round-
// trip preserves comments and key order in the file; only the targeted
// key is mutated. forge.yaml itself is never read or written here —
// per-env config no longer lives there.
func runConfigSet(envName, key, rawValue string, unset bool) error {
	if !validConfigKey(key) {
		return cliutil.UserErr(fmt.Sprintf("forge config set --env %s", envName),
			fmt.Sprintf("invalid config key %q (must match [a-z][a-z0-9_]*)", key),
			"",
			"use snake_case starting with a lowercase letter (e.g. log_level, database_url)")
	}

	// findProjectConfigFile walks up to forge.yaml — we use it as a project
	// anchor (sibling files live next to it) but never write to forge.yaml.
	projectAnchor, err := findProjectConfigFile()
	if err != nil {
		return err
	}
	projectDir := filepath.Dir(projectAnchor)
	envFilePath := filepath.Join(projectDir, fmt.Sprintf("config.%s.yaml", envName))

	// Type-check the value against proto/config/v1/config.proto, when the
	// project ships one. Best-effort: if the proto isn't present (CLI/library
	// kinds, or a project that hasn't generated AppConfig yet), we accept the
	// raw string and let the runtime parser surface a mismatch.
	var coerced any
	if !unset {
		typed, err := coerceConfigValue(projectDir, key, rawValue)
		if err != nil {
			return err
		}
		coerced = typed
	}

	// Load (or initialize) the sibling file. Missing-file on a set is fine —
	// we'll create it with a header. Missing-file on an unset is a no-op.
	raw, fileExists, err := readEnvConfigFile(envFilePath)
	if err != nil {
		return err
	}
	if !fileExists && unset {
		fmt.Printf("%s does not exist; nothing to unset.\n", envFilePath)
		return nil
	}

	root, headerComment, err := parseEnvConfigYAML(raw, envName)
	if err != nil {
		return fmt.Errorf("parse %s: %w", envFilePath, err)
	}

	if unset {
		removed := removeMappingEntry(root, key)
		if !removed {
			fmt.Printf("Key %q not present in %s; nothing to unset.\n", key, envFilePath)
			return nil
		}
	} else {
		setMappingScalar(root, key, coerced)
	}

	out, err := marshalEnvConfigYAML(root, headerComment, envName, fileExists)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", envFilePath, err)
	}
	if err := os.WriteFile(envFilePath, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", envFilePath, err)
	}

	if unset {
		fmt.Printf("Unset %q in %s\n", key, envFilePath)
	} else {
		fmt.Printf("Set %q = %v in %s\n", key, coerced, envFilePath)
	}
	return nil
}

// readEnvConfigFile reads config.<env>.yaml, returning (data, true, nil)
// when the file exists, (nil, false, nil) when it doesn't, and a wrapped
// error for any other I/O failure. Split out so callers can distinguish
// "fresh create" from "edit existing" without re-statting.
func readEnvConfigFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		return data, true, nil
	}
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("read %s: %w", path, err)
}

// parseEnvConfigYAML turns the raw file contents into a mapping node
// rooted at the document, plus the leading header-comment block we
// preserve verbatim. An empty/absent file yields a fresh mapping node
// and an empty header (the caller stamps a default header when writing
// a freshly created file).
func parseEnvConfigYAML(raw []byte, envName string) (*yaml.Node, string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, "", nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, "", err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, "", fmt.Errorf("expected a YAML document")
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, "", fmt.Errorf("expected top-level mapping (got kind=%v)", root.Kind)
	}
	// yaml.v3 attaches the leading-comment block to the first child of
	// the root mapping; we extract the raw `#`-prefixed lines so callers
	// can prepend them ahead of marshal output without yaml escaping
	// them. For our use-case we rely on a leading-line scan of `raw`
	// instead, which is robust to empty mappings.
	header := extractLeadingCommentLines(raw)
	_ = envName // envName unused here; the marshal path stamps a default header when needed.
	return root, header, nil
}

// extractLeadingCommentLines returns the contiguous `#`-prefixed (or
// blank) header block at the top of `raw`, preserving the user's
// formatting. The scan stops at the first non-comment, non-blank line.
func extractLeadingCommentLines(raw []byte) string {
	var b strings.Builder
	for _, line := range strings.SplitAfter(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			b.WriteString(line)
			continue
		}
		break
	}
	return b.String()
}

// marshalEnvConfigYAML renders the mapping back to bytes, prepending a
// header comment. For freshly created files we stamp the canonical
// configEnvFileHeader; for existing files we preserve whatever leading
// comment block the user already had.
func marshalEnvConfigYAML(root *yaml.Node, existingHeader, envName string, fileExisted bool) ([]byte, error) {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	// Empty mapping marshals as "{}\n" — strip that to an empty body so
	// the file reads as "header only, no entries" after the last unset.
	if bytes.Equal(bytes.TrimSpace(body), []byte("{}")) {
		body = nil
	}

	var header string
	switch {
	case !fileExisted:
		header = fmt.Sprintf(configEnvFileHeader, envName, envName, envName)
	case existingHeader != "":
		header = existingHeader
	}

	var out bytes.Buffer
	if header != "" {
		out.WriteString(header)
		if !strings.HasSuffix(header, "\n") {
			out.WriteString("\n")
		}
		// Insert a single blank line between header and body for readability,
		// unless the header already ends with one.
		if !strings.HasSuffix(header, "\n\n") && len(body) > 0 {
			out.WriteString("\n")
		}
	}
	out.Write(body)
	return out.Bytes(), nil
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
