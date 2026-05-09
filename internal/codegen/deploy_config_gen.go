// Package codegen — deploy_config_gen.go renders per-environment
// `deploy/kcl/<env>/config_gen.k` files, projecting the project's per-env
// config map (from forge.yaml + sibling files) plus the proto-level
// ConfigFieldOptions annotations into KCL EnvVar lists AND a generated
// ConfigMap resource.
//
// The generated file declares one CATEGORY_ENV list per category present
// in the proto, plus an APP_ENV list for fields without a category, plus
// a CONFIG_MAPS list holding the project-owned ConfigMap (one per env,
// `<project>-<env>-config`) populated with all non-sensitive values. The
// hand-edited `main.k` for the env imports `deploy.kcl.<env>.config_gen`,
// concatenates the EnvVar lists into the application's `env_vars`, and
// assigns CONFIG_MAPS to `Environment.config_maps` so the render layer
// emits a `kind: ConfigMap` resource alongside the Deployments.
//
// Generation rules:
//
//   - For non-sensitive fields where the env config provides a value:
//     the value is added to the generated ConfigMap's `data` map AND the
//     EnvVar is emitted as `config_map_ref = "<project>-<env>-config",
//     config_map_key = ENV_VAR`. The rendered Deployment env entry uses
//     `valueFrom.configMapKeyRef`, so a `kubectl edit configmap` change
//     propagates to pods on the next restart without rebuilding the
//     Deployment manifest.
//   - For non-sensitive fields with no value: skip (the binary's
//     proto-derived default applies at startup).
//   - For sensitive fields: emit `EnvVar { name = ENV_VAR, secret_ref =
//     "<project>-secrets", secret_key = "<env_var lowercased>" }`. If the
//     env config's value is a "${SECRET_NAME}" reference, the secret_ref
//     is taken from the reference body instead. To override the secret
//     key as well, use "${SECRET_NAME#secret-key}" (e.g.
//     "${db-credentials#database-url}") — handy for projects whose
//     existing cluster secrets use kebab-case keys that don't match the
//     forge default of lowercase env_var. The Secret resource itself is
//     NOT generated; it's expected to be provisioned out-of-band
//     (sealed-secrets, ESO, manual `kubectl create secret`, etc.).
//
// This replaces the hand-curated DB_ENV / NATS_ENV / STRIPE_ENV groups
// that projects accumulate in deploy/kcl/base.k as soon as they grow more
// than a couple of secret-backed knobs.
package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
)

// DeployConfigGenInput is the per-env input for [GenerateDeployConfig].
type DeployConfigGenInput struct {
	ProjectName string                    // forge.yaml `name`
	EnvName     string                    // dev / staging / prod / ...
	KCLDir      string                    // deploy/kcl (absolute or relative)
	ProjectDir  string                    // project root, used to compute relative paths for checksumming. May be empty for callers that pass an absolute KCLDir outside the project tree.
	Fields      []ConfigField             // proto-derived config fields (with annotations)
	EnvConfig   map[string]any            // merged per-env config values, by proto field name
	Envs        []config.EnvironmentConfig // optional: for project-wide context (unused today)
	Checksums   *checksums.FileChecksums  // when set, the rendered config_gen.k is recorded so it doesn't show up as an orphan in `forge audit`
}

// GenerateDeployConfig writes deploy/kcl/<env>/config_gen.k for one
// environment. It returns nil if there are no config fields at all.
//
// The function is idempotent — running it twice produces the same file.
func GenerateDeployConfig(in DeployConfigGenInput) error {
	if len(in.Fields) == 0 {
		return nil
	}
	if in.KCLDir == "" {
		in.KCLDir = "deploy/kcl"
	}
	if in.EnvName == "" {
		return fmt.Errorf("deploy config gen: env name is required")
	}

	body := renderDeployConfigKCL(in)

	outDir := filepath.Join(in.KCLDir, in.EnvName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", outDir, err)
	}
	outPath := filepath.Join(outDir, "config_gen.k")

	// Record the checksum when we have a project root we can express the
	// path relative to. ProjectDir may be empty in fixture-driven tests
	// that pass an absolute KCLDir outside the project tree; in that case
	// we fall back to the raw path which avoids tracking but still writes.
	if in.Checksums != nil && in.ProjectDir != "" {
		rel, relErr := filepath.Rel(in.ProjectDir, outPath)
		if relErr == nil {
			if _, err := checksums.WriteGeneratedFile(in.ProjectDir, rel, []byte(body), in.Checksums, true); err != nil {
				return fmt.Errorf("write %s: %w", outPath, err)
			}
			return nil
		}
	}
	return os.WriteFile(outPath, []byte(body), 0o644)
}

// renderDeployConfigKCL builds the KCL body for a single env.
// It groups fields by category. The result has stable ordering:
//   - default category ("") first as APP_ENV
//   - other categories sorted alphabetically as <CATEGORY>_ENV
//   - within a group: original proto field order is preserved
//
// In addition to the EnvVar lists, it emits a `CONFIG_MAPS` list with a
// single `schema.ConfigMap` populated from non-sensitive value-bearing
// fields. The env's hand-edited `main.k` is expected to wire
// `Environment.config_maps = cfg.CONFIG_MAPS` — without that wire, the
// rendered manifest will still build but no ConfigMap resource is
// emitted (the env vars then point at a missing ConfigMap and pods
// crash-loop on apply, which is the loud failure we want).
func renderDeployConfigKCL(in DeployConfigGenInput) string {
	// Bucket fields by category in stable proto order.
	type bucket struct {
		category string
		fields   []ConfigField
	}
	buckets := map[string]*bucket{}
	var categoryOrder []string
	for _, f := range in.Fields {
		cat := strings.ToLower(strings.TrimSpace(f.Category))
		b, ok := buckets[cat]
		if !ok {
			b = &bucket{category: cat}
			buckets[cat] = b
			categoryOrder = append(categoryOrder, cat)
		}
		b.fields = append(b.fields, f)
	}

	// Stable category ordering: "" first, the rest alphabetical.
	sort.SliceStable(categoryOrder, func(i, j int) bool {
		ci, cj := categoryOrder[i], categoryOrder[j]
		switch {
		case ci == "" && cj != "":
			return true
		case ci != "" && cj == "":
			return false
		default:
			return ci < cj
		}
	})

	defaultSecretName := fmt.Sprintf("%s-secrets", in.ProjectName)
	configMapName := fmt.Sprintf("%s-%s-config", in.ProjectName, in.EnvName)

	// Collect ConfigMap data entries in proto field order, deduped by
	// env-var name.
	type cmEntry struct {
		key, value string
	}
	var cmEntries []cmEntry
	seenCMKeys := map[string]bool{}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("\"\"\"\nGenerated per-environment config for the %s environment.\n\n", in.EnvName))
	b.WriteString("Do not hand-edit. Source of truth:\n")
	b.WriteString("  * Proto annotations in proto/config/v1/config.proto\n")
	b.WriteString(fmt.Sprintf("  * Inline values: forge.yaml -> environments[%s].config\n", in.EnvName))
	b.WriteString(fmt.Sprintf("  * Or sibling file: config.%s.yaml\n", in.EnvName))
	b.WriteString("\nRegenerated by `forge generate`.\n\"\"\"\n\n")
	b.WriteString("import deploy.kcl.schema\n\n")

	// First pass: render each category's entries into a per-bucket
	// buffer. We need to know whether a bucket emitted any lines before
	// deciding whether to write its `<CAT>_ENV: [schema.EnvVar] = [...]`
	// block — empty categories (everything is a default-only,
	// non-sensitive field) are elided entirely so the generated file
	// doesn't carry visually-noisy `BILLING_ENV: [schema.EnvVar] = []`
	// stubs. Callers concatenating in main.k must reference only the
	// categories that have entries; the SKILL.md flags this.
	type rendered struct {
		listName string
		body     strings.Builder // pre-indented entry lines
	}
	emitted := make([]rendered, 0, len(categoryOrder))
	for _, cat := range categoryOrder {
		bk := buckets[cat]
		var r rendered
		r.listName = groupListName(cat)
		for _, f := range bk.fields {
			rawVal, hasVal := in.EnvConfig[f.Name]
			line, kind, ok := renderEnvVarEntry(f, rawVal, hasVal, defaultSecretName, configMapName)
			if !ok {
				continue
			}
			r.body.WriteString("    ")
			r.body.WriteString(line)
			r.body.WriteString("\n")
			if kind == envVarKindConfigMap && !seenCMKeys[f.EnvVar] {
				seenCMKeys[f.EnvVar] = true
				cmEntries = append(cmEntries, cmEntry{
					key:   f.EnvVar,
					value: stringifyConfigValue(rawVal),
				})
			}
		}
		if r.body.Len() == 0 {
			continue
		}
		emitted = append(emitted, r)
	}
	for _, r := range emitted {
		b.WriteString(fmt.Sprintf("%s: [schema.EnvVar] = [\n", r.listName))
		b.WriteString(r.body.String())
		b.WriteString("]\n\n")
	}

	// Emit the CONFIG_MAPS list. We always emit the list (even when
	// empty) so the env's main.k can unconditionally reference
	// `cfg.CONFIG_MAPS` without conditional wiring.
	b.WriteString("CONFIG_MAPS: [schema.ConfigMap] = [\n")
	if len(cmEntries) > 0 {
		b.WriteString("    schema.ConfigMap {\n")
		b.WriteString(fmt.Sprintf("        name = %q\n", configMapName))
		b.WriteString("        data = {\n")
		for _, e := range cmEntries {
			b.WriteString(fmt.Sprintf("            %q = %q\n", e.key, e.value))
		}
		b.WriteString("        }\n")
		b.WriteString("    }\n")
	}
	b.WriteString("]\n")

	return b.String()
}

// groupListName converts a category to the KCL list name used in the
// generated file (e.g. "" → "APP_ENV", "stripe" → "STRIPE_ENV").
func groupListName(category string) string {
	if category == "" {
		return "APP_ENV"
	}
	upper := strings.ToUpper(category)
	upper = strings.ReplaceAll(upper, "-", "_")
	return upper + "_ENV"
}

// envVarKind classifies the projection chosen for a generated EnvVar.
// The renderer uses it to decide which fields land in the per-env
// ConfigMap's `data:` block.
type envVarKind int

const (
	envVarKindSecret envVarKind = iota
	envVarKindConfigMap
)

// renderEnvVarEntry builds a single KCL `schema.EnvVar { ... }` literal
// for one field. It returns the literal, the projection kind (configMap
// vs secret), and true when an entry should be emitted; false signals
// "skip" (e.g. a non-sensitive field with no per-env value — the
// binary's proto-default applies at startup).
//
// Non-sensitive fields with values are routed through the per-env
// ConfigMap (`config_map_ref` + `config_map_key`) so deploy operators
// can `kubectl edit configmap` and roll without re-rendering the
// Deployment. Sensitive fields stay as `secret_ref` pointing at
// externally-provisioned Secrets.
func renderEnvVarEntry(f ConfigField, rawVal any, hasVal bool, defaultSecret, configMapName string) (string, envVarKind, bool) {
	if f.EnvVar == "" {
		return "", 0, false
	}

	if f.Sensitive {
		secretName, secretKey := defaultSecret, strings.ToLower(f.EnvVar)
		// Allow overriding the secret name (and optionally the key) via
		// "${NAME}" or "${NAME#KEY}" in env config.
		if hasVal {
			if s, ok := rawVal.(string); ok {
				if name, key, ok := parseSecretRef(s); ok {
					secretName = name
					if key != "" {
						secretKey = key
					}
				}
			}
		}
		return fmt.Sprintf(`schema.EnvVar { name = %q, secret_ref = %q, secret_key = %q }`,
			f.EnvVar, secretName, secretKey), envVarKindSecret, true
	}

	if !hasVal {
		return "", 0, false
	}
	return fmt.Sprintf(`schema.EnvVar { name = %q, config_map_ref = %q, config_map_key = %q }`,
		f.EnvVar, configMapName, f.EnvVar), envVarKindConfigMap, true
}

// parseSecretRef reads a secret reference string and returns its name and
// optional key. Recognized forms:
//
//   - "${SECRET_NAME}"           -> name="SECRET_NAME", key=""    (key falls back to the codegen default)
//   - "${SECRET_NAME#SECRET_KEY}" -> name="SECRET_NAME", key="SECRET_KEY"
//
// The key-override form lets projects mirror existing cluster secrets that
// use kebab-case (or otherwise non-default) keys without renaming them.
// Anything else returns ("", "", false).
func parseSecretRef(s string) (string, string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "${") || !strings.HasSuffix(s, "}") {
		return "", "", false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, "${"), "}")
	if inner == "" {
		return "", "", false
	}
	if i := strings.Index(inner, "#"); i >= 0 {
		name := strings.TrimSpace(inner[:i])
		key := strings.TrimSpace(inner[i+1:])
		if name == "" {
			return "", "", false
		}
		return name, key, true
	}
	return inner, "", true
}

// stringifyConfigValue renders a YAML-decoded scalar in a form suitable
// for a KCL string literal. Numbers and bools get the obvious string
// form; nil becomes empty.
func stringifyConfigValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		// YAML's number type is float64; render integers as integers.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}
