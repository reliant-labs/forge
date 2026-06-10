package config

import (
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/naming"
)

// LoadStrict parses a forge.yaml byte stream into a ProjectConfig with
// strict validation: unknown keys (typos, dropped fields) and missing
// required fields are reported in a single error rather than silently
// succeeding or failing on the first issue.
//
// path is used purely for error-message context (e.g. "forge.yaml" or
// the absolute path); it is not opened. Pass an empty string for inline
// data without a file backing.
//
// Behaviour:
//
//  1. The YAML is decoded into a yaml.Node tree, then walked against
//     the ProjectConfig struct shape. Unknown keys are collected with
//     their YAML line number and parent path; a Levenshtein-based
//     suggestion is attached when a known sibling key is within edit
//     distance 2 (or 3 for keys >= 8 chars).
//  2. The same bytes are then decoded into a ProjectConfig via the
//     standard yaml decoder so that scalar-type mismatches (e.g.
//     port: "8080") surface as their own error class.
//  3. Required-field validation runs on the populated struct.
//
// All issues across the three phases are batched into a single
// ValidationError; the caller sees the full list rather than just the
// first failure.
func LoadStrict(data []byte, path string) (*ProjectConfig, error) {
	label := path
	if label == "" {
		label = "forge.yaml"
	}

	// Phase 1: walk yaml.Node to find unknown keys with position info.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: parse error: %w", label, err)
	}
	var root *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	var issues []validationIssue
	if root != nil && root.Kind == yaml.MappingNode {
		issues = append(issues, walkUnknownKeys(root, "", reflect.TypeFor[ProjectConfig]())...)
	} else if root != nil && root.Kind != 0 {
		issues = append(issues, validationIssue{
			line:   root.Line,
			column: root.Column,
			msg:    "expected a YAML mapping at the top level",
			fix:    "the file must be a YAML mapping (key: value pairs), not a list or scalar.",
		})
	}

	// Phase 2: decode into the typed struct. This catches scalar-type
	// mismatches and any other yaml decoding failures. We do NOT pass
	// KnownFields(true) here because phase 1 already covered that with
	// better suggestions.
	var cfg ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// yaml type errors look like:
		//   "yaml: line 7: cannot unmarshal !!str `8080` into int"
		// We surface them verbatim alongside any unknown-key issues.
		for _, line := range splitYAMLErrorLines(err) {
			issues = append(issues, validationIssue{msg: line})
		}
	}

	// Phase 3: required-field validation. The yaml root is threaded
	// through so issues can carry the line:col of the *parent* mapping
	// (or the existing-field's own line, when it's present but invalid).
	// Without this, "module_path is required" reports no location and
	// the model has to grep — model-friendly file:line:col on every
	// issue is the goal of the LoadStrict surface.
	issues = append(issues, validateRequired(&cfg, root)...)

	// Phase 4: name-shape validation across services/binaries/frontends.
	// This catches Go-package collisions and reserved-word/identifier
	// shapes that would otherwise blow up the generator with a confusing
	// downstream error.
	issues = append(issues, validateServices(&cfg, root)...)

	if len(issues) > 0 {
		return nil, &ValidationError{Path: label, Issues: issues}
	}
	return &cfg, nil
}

// ValidationError aggregates all forge.yaml validation issues into a
// single error so callers see the full picture instead of fail-fast on
// the first problem. Implements error.
type ValidationError struct {
	Path   string
	Issues []validationIssue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		var b strings.Builder
		b.WriteString(formatIssueLocation(e.Path, e.Issues[0]))
		b.WriteString(": ")
		b.WriteString(e.Issues[0].msg)
		if e.Issues[0].fix != "" {
			fmt.Fprintf(&b, " Fix: %s", e.Issues[0].fix)
		}
		return b.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d validation issue", e.Path, len(e.Issues))
	if len(e.Issues) != 1 {
		b.WriteString("s")
	}
	b.WriteString(":\n")
	for _, iss := range e.Issues {
		b.WriteString("  ")
		b.WriteString(formatIssueLocation(e.Path, iss))
		b.WriteString(": ")
		b.WriteString(iss.msg)
		if iss.fix != "" {
			fmt.Fprintf(&b, " Fix: %s", iss.fix)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatIssueLocation renders the per-issue position in standard
// compiler/editor format: `path:line:col` when both line and column are
// known, `path:line` for line-only, `path` when neither. Matches what
// every editor, LSP client, and `cc`/`go vet`-style tool already
// understands — a model reading the error can immediately open the
// right line, no grep round-trip required.
func formatIssueLocation(path string, iss validationIssue) string {
	switch {
	case iss.line > 0 && iss.column > 0:
		return fmt.Sprintf("%s:%d:%d", path, iss.line, iss.column)
	case iss.line > 0:
		return fmt.Sprintf("%s:%d", path, iss.line)
	default:
		return path
	}
}

type validationIssue struct {
	line   int    // YAML line number (1-based); 0 if unknown.
	column int    // YAML column (1-based); 0 if unknown.
	msg    string // primary message ("unknown key 'auht' — did you mean 'auth'?")
	fix    string // "Fix: rename to 'auth' or remove if unused."
}

// isDeprecatedTopLevelKey returns true for top-level forge.yaml keys
// that were once part of the schema but have been removed. Strict
// validation skips them rather than reporting "unknown key" so
// projects that haven't run the corresponding migration skill yet
// still load.
//
// Currently:
//   - `environments`: removed in the deploy-target-architecture
//     migration. Per-env deploy info (cluster/namespace/registry/
//     domain) now lives in KCL `forge.K8sCluster` blocks; per-env
//     app config lives in sibling `config.<env>.yaml` files. See
//     the `environments-to-kcl` migration skill.
func isDeprecatedTopLevelKey(key string) bool {
	switch key {
	case "environments":
		return true
	}
	return false
}

// removedSchemaKeys maps a fully-qualified dot-notation key path
// (e.g. "k8s.provider") to a human-readable migration hint. When the
// validator encounters an unknown key whose path matches an entry
// here, it reports the migration hint in the "Fix:" suggestion
// instead of the generic "rename or remove this key" — so users hitting
// schema-drift get a pointer to the real correction rather than a
// dead-end "this is a typo" framing.
//
// Unlike isDeprecatedTopLevelKey, entries here STILL surface as
// validation errors — the key must be removed for the file to load.
// We only fail-soft for keys that are still semi-meaningful during a
// migration window (currently just top-level `environments`).
//
// Path format: dot-separated, matching qualifiedKey output. Slice
// indices in the path use `[N]` (e.g. "services[0].dev_target") so
// migration hints can target a field on every element of a slice via
// the literal "[*]" wildcard.
var removedSchemaKeys = map[string]string{
	// k8s.provider was dropped when forge stopped owning cluster-type
	// detection. The k3d/gke/eks choice now lives in the KCL
	// `forge.K8sCluster` block (per-env `provider:` field). The forge.yaml
	// `k8s:` section retains only `kcl_dir`.
	"k8s.provider": "this key was removed; cluster provider now lives in the per-env KCL `forge.K8sCluster` block. Remove this key.",
	// services[].dev_target was dropped when host-vs-cluster placement
	// moved to the KCL layer (per-env `deploy:` field on `forge.Service`).
	// See the dev-target-to-kcl-deploy migration skill.
	"services[*].dev_target": "this key was removed; host-vs-cluster placement now lives in the per-env KCL `forge.Service.deploy` field. Remove this key.",
}

// removedSchemaKeyHint returns the migration hint for a fully-qualified
// path if one is registered, or "" otherwise. The `[*]` wildcard in
// the table matches any `[N]` index in the input path so a single
// entry covers every slice element.
func removedSchemaKeyHint(path string) string {
	if hint, ok := removedSchemaKeys[path]; ok {
		return hint
	}
	// Wildcard match: replace each `[N]` in the input with `[*]` and
	// look up again.
	wild := wildcardIndices(path)
	if wild != path {
		if hint, ok := removedSchemaKeys[wild]; ok {
			return hint
		}
	}
	return ""
}

// wildcardIndices replaces every `[<digits>]` substring in s with
// `[*]`. Used so removedSchemaKeys can register one entry per slice
// field rather than one per index.
func wildcardIndices(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] != '[' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// Find matching ']'.
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j < len(s) && s[j] == ']' && j > i+1 {
			b.WriteString("[*]")
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// walkUnknownKeys recursively descends a yaml.Node mapping against the
// reflected Go type. Unknown keys produce issues with line numbers and
// suggestions; known keys recurse if they map to nested struct or slice
// types.
func walkUnknownKeys(node *yaml.Node, path string, t reflect.Type) []validationIssue {
	var out []validationIssue
	if t == nil {
		return nil
	}
	// Unwrap pointer.
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// We only descend into struct mappings here. Map[string]X with
	// declared key type just accepts anything (e.g. PackOverrides), so
	// no unknown-key warning at that layer.
	if t.Kind() != reflect.Struct {
		return nil
	}
	known := yamlKeysOf(t)
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		key := keyNode.Value
		// Deprecated keys at the top level: silently ignored so
		// projects mid-migration don't fail validation. The
		// `environments-to-kcl` skill handles user-facing communication.
		if path == "" && isDeprecatedTopLevelKey(key) {
			continue
		}
		field, ok := known[key]
		if !ok {
			fullPath := qualifiedKey(path, key)
			msg := fmt.Sprintf("unknown key %q", fullPath)
			fix := "rename or remove this key."
			// Schema-drift hints take precedence over typo suggestions:
			// when we know a key was deliberately removed, "did you mean
			// <X>" would mislead the user toward an even more wrong
			// answer.
			if hint := removedSchemaKeyHint(fullPath); hint != "" {
				fix = hint
			} else if suggestion := closestMatch(key, knownNames(known)); suggestion != "" {
				msg += fmt.Sprintf(" — did you mean %q?", suggestion)
				fix = fmt.Sprintf("rename to %q or remove if unused.", suggestion)
			}
			out = append(out, validationIssue{line: keyNode.Line, column: keyNode.Column, msg: msg, fix: fix})
			continue
		}
		// Recurse into nested structs and slices of structs.
		ft := field.Type
		switch ft.Kind() {
		case reflect.Struct:
			if valNode.Kind == yaml.MappingNode {
				childPath := joinPath(path, key)
				out = append(out, walkUnknownKeys(valNode, childPath, ft)...)
			}
		case reflect.Slice:
			elem := ft.Elem()
			if elem.Kind() == reflect.Struct && valNode.Kind == yaml.SequenceNode {
				for idx, item := range valNode.Content {
					if item.Kind == yaml.MappingNode {
						childPath := fmt.Sprintf("%s[%d]", joinPath(path, key), idx)
						out = append(out, walkUnknownKeys(item, childPath, elem)...)
					}
				}
			}
		case reflect.Pointer:
			if ft.Elem().Kind() == reflect.Struct && valNode.Kind == yaml.MappingNode {
				childPath := joinPath(path, key)
				out = append(out, walkUnknownKeys(valNode, childPath, ft.Elem())...)
			}
		case reflect.Map:
			// Map[string]Struct: descend into each entry's value, where
			// the key is user-defined (e.g. pack name) so we can't
			// validate the key itself.
			if ft.Elem().Kind() == reflect.Struct && valNode.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(valNode.Content); j += 2 {
					entryKey := valNode.Content[j]
					entryVal := valNode.Content[j+1]
					if entryVal.Kind == yaml.MappingNode {
						childPath := fmt.Sprintf("%s.%s", joinPath(path, key), entryKey.Value)
						out = append(out, walkUnknownKeys(entryVal, childPath, ft.Elem())...)
					}
				}
			}
		}
	}
	return out
}

// yamlKeysOf returns a map from yaml-tag-name -> reflect.StructField for
// every field declared on t. Embedded structs are flattened so their
// keys appear at the parent level (yaml.v3 default behaviour).
func yamlKeysOf(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField)
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			if f.Anonymous {
				maps.Copy(out, yamlKeysOf(f.Type))
			}
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
}

func knownNames(m map[string]reflect.StructField) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func qualifiedKey(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// closestMatch returns the closest entry in candidates to needle by
// Levenshtein distance, or "" if no candidate is close enough. Threshold
// scales with needle length: short keys (< 8 chars) require <= 2,
// longer keys allow <= 3.
func closestMatch(needle string, candidates []string) string {
	if needle == "" || len(candidates) == 0 {
		return ""
	}
	threshold := 2
	if len(needle) >= 8 {
		threshold = 3
	}
	best := ""
	bestDist := threshold + 1
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(needle), strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist <= threshold {
		return best
	}
	return ""
}

// levenshtein returns the edit distance (insert/delete/substitute, all
// cost 1) between a and b. Implementation uses a single-row DP buffer
// for O(min(len)) memory.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = minInt(curr[j-1]+1, minInt(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// splitYAMLErrorLines turns a yaml decoding error into one issue per
// underlying problem. yaml.v3's TypeError aggregates issues with newlines
// in its message, while plain errors have a single line.
func splitYAMLErrorLines(err error) []string {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// yaml.v3 prefixes TypeError messages with "yaml: unmarshal errors:\n  ".
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	parts := strings.Split(msg, "\n")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Skip the "field X not found" lines — phase 1 covered those
		// with better suggestions.
		if p == "" || strings.Contains(p, " not found in type ") {
			continue
		}
		// Trim the leading "yaml: " prefix when present so the message
		// reads cleanly under our path-prefixed format.
		p = strings.TrimPrefix(p, "yaml: ")
		out = append(out, p)
	}
	return out
}

// validateRequired checks that fields the project cannot meaningfully
// be missing are present. The list intentionally stays small — every
// required field here corresponds to a real downstream breakage when
// absent (broken go.mod, empty deploy, ambiguous codegen target).
func validateRequired(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	// rootPos is the fallback location for "this required field is
	// missing entirely from the file" — we point at the top-level
	// mapping (line 1, col 1) so the model knows it's a forge.yaml-wide
	// concern, not a nested-block one.
	var rootLine, rootCol int
	if root != nil {
		rootLine, rootCol = root.Line, root.Column
	}

	if strings.TrimSpace(cfg.Name) == "" {
		out = append(out, validationIssue{
			line:   rootLine,
			column: rootCol,
			msg:    "'name' is required but missing or empty",
			fix:    "add 'name: <project-name>' near the top of forge.yaml.",
		})
	}
	if strings.TrimSpace(cfg.ModulePath) == "" {
		out = append(out, validationIssue{
			line:   rootLine,
			column: rootCol,
			msg:    "'module_path' is required but missing or empty",
			fix:    "add 'module_path: github.com/<org>/<project>' near the top of forge.yaml.",
		})
	} else if !looksLikeGoModulePath(cfg.ModulePath) {
		// Existing-but-invalid: point at the actual `module_path:` line.
		line, col := findNodePos(root, []string{"module_path"})
		out = append(out, validationIssue{
			line:   line,
			column: col,
			msg:    fmt.Sprintf("'module_path' value %q does not look like a Go module path", cfg.ModulePath),
			fix:    "use a path like 'github.com/<org>/<project>' (must contain a slash, no spaces).",
		})
	}
	// Kind defaults to "service" via EffectiveProjectKind, so an empty
	// kind is fine. Only validate when set.
	if k := strings.ToLower(strings.TrimSpace(cfg.Kind)); k != "" {
		switch k {
		case ProjectKindService, ProjectKindCLI, ProjectKindLibrary:
		default:
			line, col := findNodePos(root, []string{"kind"})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("'kind' value %q is invalid", cfg.Kind),
				fix:    "use one of: service, cli, library.",
			})
		}
	}

	for i, svc := range cfg.Services {
		prefix := fmt.Sprintf("services[%d]", i)
		if strings.TrimSpace(svc.Name) == "" {
			// Position at the parent services[i] mapping so the model
			// can open the right block and add the missing field.
			line, col := findNodePos(root, []string{"services", fmt.Sprintf("[%d]", i)})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name is required", prefix),
				fix:    "add a 'name:' for this service entry.",
			})
		}
		// services[].path is intentionally not required: the cli loader
		// applies a 'handlers/<name>' default when the user omits it.
		// Host/cluster placement was previously gated here via
		// services[].dev_target. It moved to the KCL layer (per-env
		// `deploy:` field on the [Service] schema) — see the
		// migration/dev-target-to-kcl-deploy skill.
	}

	for i, fe := range cfg.Frontends {
		prefix := fmt.Sprintf("frontends[%d]", i)
		if strings.TrimSpace(fe.Name) == "" {
			line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i)})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name is required", prefix),
				fix:    "add a 'name:' for this frontend entry.",
			})
		}
		// frontends[].type and frontends[].path are filled in by the
		// loader when omitted (type → "nextjs", path → "frontends/<name>"),
		// so we only validate non-empty values here. Required-ness would
		// be a regression for existing forge.yaml files.
		if t := strings.ToLower(strings.TrimSpace(fe.Type)); t != "" {
			if t != "nextjs" && t != "react_native" && t != "react-native" && t != "vite-spa" {
				line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "type"})
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.type value %q is invalid", prefix, fe.Type),
					fix:    "use one of: nextjs, react-native, vite-spa.",
				})
			}
		}
		// frontends[].output selects the Next.js build/runtime shape.
		// Only meaningful for type=nextjs; we still validate the value
		// for other types because changing the type later shouldn't
		// silently re-validate against a stale value. Defaults to
		// "static" when empty.
		if o := strings.ToLower(strings.TrimSpace(fe.Output)); o != "" {
			if o != "static" && o != "standalone" && o != "server" {
				line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "output"})
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.output value %q is invalid", prefix, fe.Output),
					fix:    "use one of: static (default), standalone, server.",
				})
			}
		}
	}

	// Only require database.driver when ORM has been *explicitly* enabled.
	// Features.ORM defaults to nil → ORMEnabled() reports true, but a nil
	// value means "user didn't make a choice"; many legacy projects work
	// without a driver because they aren't actually exercising the ORM
	// codegen at runtime. Demanding a driver in that case would be a
	// breaking change. Explicit `features.orm: true` is the signal that
	// the user is committing to the ORM and so must declare a driver.
	if cfg.Features.ORM != nil && *cfg.Features.ORM && strings.TrimSpace(cfg.Database.Driver) == "" {
		// Point at the `database:` block (or the file root if absent).
		line, col := findNodePos(root, []string{"database"})
		if line == 0 {
			line, col = rootLine, rootCol
		}
		out = append(out, validationIssue{
			line:   line,
			column: col,
			msg:    "'database.driver' is required when features.orm is explicitly enabled",
			fix:    "add 'database:\\n  driver: postgres' (or 'sqlite').",
		})
	}

	return out
}

// findNodePos walks a YAML mapping/sequence tree along a dot/index path
// and returns the line/col of the resolved node. Path segments are
// either bare keys (e.g. "module_path") or sequence indices in literal
// `[N]` form (e.g. "[0]") — same shape used in qualifiedKey output so
// callers can construct paths once and reuse them across issue messages
// and position lookups. Returns (0, 0) when the path doesn't resolve;
// callers fall back to the root position (or omit position entirely)
// in that case.
func findNodePos(node *yaml.Node, segments []string) (int, int) {
	if node == nil {
		return 0, 0
	}
	cur := node
	for _, seg := range segments {
		if cur == nil {
			return 0, 0
		}
		if strings.HasPrefix(seg, "[") && strings.HasSuffix(seg, "]") {
			// Sequence index.
			if cur.Kind != yaml.SequenceNode {
				return 0, 0
			}
			idx := 0
			if _, err := fmt.Sscanf(seg, "[%d]", &idx); err != nil {
				return 0, 0
			}
			if idx < 0 || idx >= len(cur.Content) {
				return 0, 0
			}
			cur = cur.Content[idx]
			continue
		}
		// Mapping key lookup.
		if cur.Kind != yaml.MappingNode {
			return 0, 0
		}
		var matched *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Kind == yaml.ScalarNode && cur.Content[i].Value == seg {
				matched = cur.Content[i+1]
				break
			}
		}
		if matched == nil {
			return 0, 0
		}
		cur = matched
	}
	if cur == nil {
		return 0, 0
	}
	return cur.Line, cur.Column
}

// goReservedWords is the set of Go keywords plus predeclared identifiers
// that cannot be used as package names without breaking the build.
// We use this to flag service / binary / frontend names whose canonical
// Go-package form (naming.ServicePackage) lands on one of them — e.g.
// a service named "select" or "type" would compile-fail downstream.
var goReservedWords = map[string]bool{
	// Keywords.
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	// Predeclared identifiers that would shadow basic types and break
	// `package <name>` in the generated tree.
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "any": true, "true": true, "false": true, "nil": true,
	"iota": true, "init": true,
}

// validateServices walks services / binaries / frontends and rejects
// name shapes that would silently break codegen downstream:
//
//   - empty name (or name that normalises to empty)
//   - non-Go-legal package shape after normalisation (starts with a
//     digit, contains punctuation/space that survives `ServicePackage`)
//   - normalisation collisions across the same slice (e.g.
//     `admin-server` and `admin_server` both → `admin_server` since
//     hyphens normalise to underscores) AND across slices (a service
//     and a binary with the same canonical form would write to the same
//     scaffold directory)
//   - the canonical form lands on a Go reserved word / predeclared
//     identifier (e.g. "select", "type"), which would compile-fail
//
// The lint is name-shape-only — it does not look at config semantics.
// Returning the issues batched lets ValidationError surface every
// problem in one go.
func validateServices(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	// Track canonical -> first-seen-source so collisions can name both
	// the earlier and the later entry in the error message.
	seen := map[string]string{}

	check := func(rawName, source string, pathSegs []string) {
		trimmed := strings.TrimSpace(rawName)
		if trimmed == "" {
			// Empty-name issues are already reported by validateRequired
			// for the slices that have a required-name rule. Don't double
			// up; just skip the canonical check.
			return
		}
		// Resolve position once for whichever issue fires. Falls back to
		// (0,0) if the path doesn't resolve — formatIssueLocation handles
		// that by omitting the position part of the error.
		line, col := findNodePos(root, pathSegs)
		canonical := naming.ServicePackage(trimmed)
		if canonical == "" {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q normalises to an empty Go package", source, rawName),
				fix:    "use at least one ASCII letter or digit in the name.",
			})
			return
		}
		if !isValidGoPackageIdent(canonical) {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q produces invalid Go package %q", source, rawName, canonical),
				fix:    "use ASCII letters, digits, hyphens, and underscores only; must not start with a digit.",
			})
			return
		}
		if goReservedWords[canonical] {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q normalises to Go reserved word %q", source, rawName, canonical),
				fix:    "rename so the compact lowercase form is not a Go keyword or predeclared identifier.",
			})
			return
		}
		if prev, ok := seen[canonical]; ok {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q collides with %s after normalisation (both → %q)", source, rawName, prev, canonical),
				fix:    "rename one of the entries so their compact lowercase forms differ.",
			})
			return
		}
		seen[canonical] = source
	}

	for i, svc := range cfg.Services {
		check(svc.Name, fmt.Sprintf("services[%d]", i), []string{"services", fmt.Sprintf("[%d]", i), "name"})
	}
	for i, b := range cfg.Binaries {
		check(b.Name, fmt.Sprintf("binaries[%d]", i), []string{"binaries", fmt.Sprintf("[%d]", i), "name"})
	}
	for i, fe := range cfg.Frontends {
		check(fe.Name, fmt.Sprintf("frontends[%d]", i), []string{"frontends", fmt.Sprintf("[%d]", i), "name"})
	}

	return out
}

// isValidGoPackageIdent reports whether s is a syntactically-legal Go
// package identifier: starts with an ASCII letter or underscore, and
// the rest are ASCII letters, digits, or underscores. We restrict to
// ASCII even though Go technically allows broader Unicode-letter
// package names — every forge-generated import path, directory name,
// and KCL/k8s identifier downstream assumes ASCII, so a Unicode-letter
// service name would surface as a downstream error far from the cause.
func isValidGoPackageIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if !isLetter {
				return false
			}
			continue
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

// looksLikeGoModulePath does a cheap shape check so we catch obvious
// typos (e.g. a stray period only) without trying to be a full Go
// modules validator. The Go module path rule we enforce: contains at
// least one slash and no whitespace.
func looksLikeGoModulePath(s string) bool {
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	if !strings.Contains(s, "/") {
		return false
	}
	return true
}
