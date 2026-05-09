package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
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
		issues = append(issues, walkUnknownKeys(root, "", reflect.TypeOf(ProjectConfig{}))...)
	} else if root != nil && root.Kind != 0 {
		issues = append(issues, validationIssue{
			line: root.Line,
			msg:  "expected a YAML mapping at the top level",
			fix:  "the file must be a YAML mapping (key: value pairs), not a list or scalar.",
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

	// Phase 3: required-field validation.
	issues = append(issues, validateRequired(&cfg)...)

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
		iss := e.Issues[0]
		var b strings.Builder
		fmt.Fprintf(&b, "%s", e.Path)
		if iss.line > 0 {
			fmt.Fprintf(&b, " line %d", iss.line)
		}
		fmt.Fprintf(&b, ": %s", iss.msg)
		if iss.fix != "" {
			fmt.Fprintf(&b, " Fix: %s", iss.fix)
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
		if iss.line > 0 {
			fmt.Fprintf(&b, "line %d: ", iss.line)
		}
		b.WriteString(iss.msg)
		if iss.fix != "" {
			fmt.Fprintf(&b, " Fix: %s", iss.fix)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

type validationIssue struct {
	line int    // YAML line number (1-based); 0 if unknown.
	msg  string // primary message ("unknown key 'auht' — did you mean 'auth'?")
	fix  string // "Fix: rename to 'auth' or remove if unused."
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
	if t.Kind() == reflect.Ptr {
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
		field, ok := known[key]
		if !ok {
			suggestion := closestMatch(key, knownNames(known))
			msg := fmt.Sprintf("unknown key %q", qualifiedKey(path, key))
			fix := "rename or remove this key."
			if suggestion != "" {
				msg += fmt.Sprintf(" — did you mean %q?", suggestion)
				fix = fmt.Sprintf("rename to %q or remove if unused.", suggestion)
			}
			out = append(out, validationIssue{line: keyNode.Line, msg: msg, fix: fix})
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
		case reflect.Ptr:
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
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			if f.Anonymous {
				for k, v := range yamlKeysOf(f.Type) {
					out[k] = v
				}
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
func validateRequired(cfg *ProjectConfig) []validationIssue {
	var out []validationIssue

	if strings.TrimSpace(cfg.Name) == "" {
		out = append(out, validationIssue{
			msg: "'name' is required but missing or empty",
			fix: "add 'name: <project-name>' near the top of forge.yaml.",
		})
	}
	if strings.TrimSpace(cfg.ModulePath) == "" {
		out = append(out, validationIssue{
			msg: "'module_path' is required but missing or empty",
			fix: "add 'module_path: github.com/<org>/<project>' near the top of forge.yaml.",
		})
	} else if !looksLikeGoModulePath(cfg.ModulePath) {
		out = append(out, validationIssue{
			msg: fmt.Sprintf("'module_path' value %q does not look like a Go module path", cfg.ModulePath),
			fix: "use a path like 'github.com/<org>/<project>' (must contain a slash, no spaces).",
		})
	}
	// Kind defaults to "service" via EffectiveProjectKind, so an empty
	// kind is fine. Only validate when set.
	if k := strings.ToLower(strings.TrimSpace(cfg.Kind)); k != "" {
		switch k {
		case ProjectKindService, ProjectKindCLI, ProjectKindLibrary:
		default:
			out = append(out, validationIssue{
				msg: fmt.Sprintf("'kind' value %q is invalid", cfg.Kind),
				fix: "use one of: service, cli, library.",
			})
		}
	}

	for i, svc := range cfg.Services {
		prefix := fmt.Sprintf("services[%d]", i)
		if strings.TrimSpace(svc.Name) == "" {
			out = append(out, validationIssue{
				msg: fmt.Sprintf("%s.name is required", prefix),
				fix: "add a 'name:' for this service entry.",
			})
		}
		// services[].path is intentionally not required: the cli loader
		// applies a 'handlers/<name>' default when the user omits it.
	}

	for i, fe := range cfg.Frontends {
		prefix := fmt.Sprintf("frontends[%d]", i)
		if strings.TrimSpace(fe.Name) == "" {
			out = append(out, validationIssue{
				msg: fmt.Sprintf("%s.name is required", prefix),
				fix: "add a 'name:' for this frontend entry.",
			})
		}
		// frontends[].type and frontends[].path are filled in by the
		// loader when omitted (type → "nextjs", path → "frontends/<name>"),
		// so we only validate non-empty values here. Required-ness would
		// be a regression for existing forge.yaml files.
		if t := strings.ToLower(strings.TrimSpace(fe.Type)); t != "" {
			if t != "nextjs" && t != "react_native" && t != "react-native" {
				out = append(out, validationIssue{
					msg: fmt.Sprintf("%s.type value %q is invalid", prefix, fe.Type),
					fix: "use one of: nextjs, react-native.",
				})
			}
		}
	}

	for i, env := range cfg.Envs {
		prefix := fmt.Sprintf("environments[%d]", i)
		if strings.TrimSpace(env.Name) == "" {
			out = append(out, validationIssue{
				msg: fmt.Sprintf("%s.name is required", prefix),
				fix: "add a 'name:' for this environment entry.",
			})
		}
		if strings.TrimSpace(env.Type) == "" {
			out = append(out, validationIssue{
				msg: fmt.Sprintf("%s.type is required", prefix),
				fix: "add 'type: local' or 'type: cloud'.",
			})
		} else {
			t := strings.ToLower(strings.TrimSpace(env.Type))
			if t != "local" && t != "cloud" {
				out = append(out, validationIssue{
					msg: fmt.Sprintf("%s.type value %q is invalid", prefix, env.Type),
					fix: "use one of: local, cloud.",
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
		out = append(out, validationIssue{
			msg: "'database.driver' is required when features.orm is explicitly enabled",
			fix: "add 'database:\\n  driver: postgres' (or 'sqlite').",
		})
	}

	return out
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
