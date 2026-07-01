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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DeployConfigGenInput is the per-env input for [GenerateDeployConfig].
// Embeds GenContext for ProjectDir + Checksums (ModulePath is unused —
// KCL output carries no Go imports). ProjectDir may be empty here for
// callers that pass an absolute KCLDir outside the project tree; the
// checksum path then falls back to the raw path (see GenerateDeployConfig).
type DeployConfigGenInput struct {
	GenContext

	ProjectName string         // forge.yaml `name`
	EnvName     string         // dev / staging / prod / ...
	KCLDir      string         // deploy/kcl (absolute or relative)
	Fields      []ConfigField  // proto-derived config fields (with annotations)
	EnvConfig   map[string]any // per-env config values loaded from the sibling config.<env>.yaml file
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

	// Fail BEFORE rendering for config divergence the renderer would
	// otherwise paper over with a silent default (CrashLoop at deploy
	// time) or a silent fallback (malformed ${...} ref). This converts a
	// deploy-time failure into an author-time `forge generate` error.
	if err := validateDeployConfig(in); err != nil {
		return err
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
			if err := writeForgeOwned(in.ProjectDir, rel, []byte(body), in.Checksums); err != nil {
				return fmt.Errorf("write %s: %w", outPath, err)
			}
			return nil
		}
	}
	return writeUserScaffold(outPath, []byte(body))
}

// validateDeployConfig enforces, at `forge generate` time, that a
// project's per-env config (config.<env>.yaml) actually supplies the
// values the proto declares it MUST. Without this the renderer silently
// wires a default `secret_ref`/`config_map_ref` for a required field the
// env file forgot — the manifest builds clean and the pod CrashLoops at
// deploy time on a missing/empty value. We turn that deploy-time failure
// into an actionable author-time error.
//
// Three classes are caught, all keyed off the proto annotations already
// threaded onto ConfigField:
//
//   - required + sensitive + absent: a default `<project>-secrets`
//     secret_ref would be wired pointing at a key that doesn't exist.
//   - required + non-sensitive + no proto default + absent: the binary
//     has no value to fall back on at startup.
//   - present but malformed ${...} ref: a value that LOOKS like a secret
//     reference (starts with `${`, ends with `}`) but parseSecretRef
//     rejects it would otherwise silently fall back to the default
//     secret_ref/key — losing the author's intended override.
//
// Errors are joined so a single `forge generate` reports every problem in
// the env file at once rather than one-at-a-time.
func validateDeployConfig(in DeployConfigGenInput) error {
	var errs []error
	for _, f := range in.Fields {
		// Block-reference fields (message-typed) carry no env binding of
		// their own — validation lives on their leaf fields.
		if f.EnvVar == "" {
			continue
		}

		rawVal, hasVal := in.EnvConfig[f.Name]
		present := hasVal && !isEmptyConfigValue(rawVal)

		// A value that looks like a secret ref but doesn't parse is a typo
		// the renderer would silently swallow (falling back to defaults).
		if present {
			if s, ok := rawVal.(string); ok && looksLikeSecretRef(s) {
				if _, _, refOK := parseSecretRef(s); !refOK {
					errs = append(errs, fmt.Errorf(
						"config.%s.yaml: field %s (%s) has a malformed secret reference %q — "+
							"use ${secret-name} or ${secret-name#key}",
						in.EnvName, f.Name, f.EnvVar, s))
				}
			}
			continue
		}

		if !f.Required {
			continue
		}
		// Required + non-sensitive with a proto default is satisfiable at
		// startup from the default — not an error.
		if !f.Sensitive && f.DefaultValue != "" {
			continue
		}
		errs = append(errs, fmt.Errorf(
			"config.%s.yaml: required %s field %s (%s) is missing — add it, e.g. %s: %q",
			in.EnvName, sensitivity(f.Sensitive), f.Name, f.EnvVar, f.Name, exampleConfigValue(f)))
	}
	return errors.Join(errs...)
}

// sensitivity renders the field class for the error message.
func sensitivity(sensitive bool) string {
	if sensitive {
		return "sensitive"
	}
	return "non-sensitive"
}

// exampleConfigValue suggests a config.<env>.yaml value for a missing
// required field — a secret reference for sensitive fields, an explicit
// "<set me>" placeholder otherwise.
func exampleConfigValue(f ConfigField) string {
	if f.Sensitive {
		return "${secret-name#key}"
	}
	return "<set me>"
}

// looksLikeSecretRef reports whether a raw string is shaped like a secret
// reference — `${...}` — and so SHOULD parse. Used to distinguish a typo'd
// ref (which must fail) from an ordinary literal value (which is fine).
func looksLikeSecretRef(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "${") && strings.HasSuffix(s, "}")
}

// isEmptyConfigValue treats nil and the empty/whitespace string as "no
// value provided" — a YAML key present with no value is the same footgun
// as the key being absent.
func isEmptyConfigValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	default:
		return false
	}
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
	// Header is `#` comments, NOT a `"""` module docstring. The Tier-1
	// stamp prepends a `# forge:hash=...` line above this file's body;
	// with a comment before it, a `"""..."""` block stops being a module
	// docstring and becomes a free-standing string statement that KCL
	// merges as a package-level value — which then conflicts with the
	// sibling main.k when `kcl run deploy/kcl/<env>` compiles the env
	// directory as one package. Comment lines avoid that entirely.
	b.WriteString(fmt.Sprintf("# Generated per-environment config for the %s environment.\n", in.EnvName))
	b.WriteString("# Do not hand-edit. Source of truth:\n")
	b.WriteString("#   * Proto annotations in proto/config/v1/config.proto\n")
	b.WriteString(fmt.Sprintf("#   * Sibling file: config.%s.yaml\n", in.EnvName))
	b.WriteString("# Regenerated by `forge generate`.\n\n")
	// The forge KCL module ships the EnvVar / ConfigMap schemas that
	// this generated file references; the project's `deploy/kcl/kcl.mod`
	// pins it as a dependency.
	b.WriteString("import forge\n\n")

	// First pass: render each category's entries into a per-bucket
	// buffer. We need to know whether a bucket emitted any lines before
	// deciding whether to write its `<CAT>_ENV: [schema.EnvVar] = [...]`
	// block — empty user-category lists (everything is a default-only,
	// non-sensitive field) are elided entirely so the generated file
	// doesn't carry visually-noisy `BILLING_ENV: [schema.EnvVar] = []`
	// stubs. Callers concatenating in main.k must reference only the
	// categories that have entries; the SKILL.md flags this.
	//
	// EXCEPTION: APP_ENV (the default, uncategorized bucket) is always
	// emitted — as `APP_ENV: [forge.EnvVar] = []` when empty. The
	// scaffolded `deploy/kcl/<env>/main.k` references `cfg.APP_ENV`
	// unconditionally, and on a fresh project the default category often
	// has no value-bearing fields. Eliding it would make KCL render fail
	// with an undefined-attribute error on the very first `forge deploy`.
	type rendered struct {
		listName string
		body     strings.Builder // pre-indented entry lines
		isApp    bool            // true for the default (APP_ENV) bucket
	}
	emitted := make([]rendered, 0, len(categoryOrder))
	seenApp := false
	for _, cat := range categoryOrder {
		bk := buckets[cat]
		var r rendered
		r.listName = groupListName(cat)
		r.isApp = cat == ""
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
		if r.isApp {
			seenApp = true
		}
		if r.body.Len() == 0 && !r.isApp {
			continue
		}
		emitted = append(emitted, r)
	}
	// If the proto has zero default-category fields, the loop above never
	// produced an APP_ENV bucket. Synthesize an empty one so main.k can
	// reference `cfg.APP_ENV` unconditionally.
	if !seenApp {
		emitted = append([]rendered{{listName: "APP_ENV", isApp: true}}, emitted...)
	}
	for _, r := range emitted {
		if r.body.Len() == 0 {
			b.WriteString(fmt.Sprintf("%s: [forge.EnvVar] = []\n\n", r.listName))
			continue
		}
		b.WriteString(fmt.Sprintf("%s: [forge.EnvVar] = [\n", r.listName))
		b.WriteString(r.body.String())
		b.WriteString("]\n\n")
	}

	// Emit the CONFIG_MAPS list. We always emit the list (even when
	// empty) so the env's main.k can unconditionally reference
	// `cfg.CONFIG_MAPS` without conditional wiring.
	b.WriteString("CONFIG_MAPS: [forge.ConfigMap] = [\n")
	if len(cmEntries) > 0 {
		b.WriteString("    forge.ConfigMap {\n")
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
		return fmt.Sprintf(`forge.EnvVar { name = %q, secret_ref = %q, secret_key = %q }`,
			f.EnvVar, secretName, secretKey), envVarKindSecret, true
	}

	if !hasVal {
		return "", 0, false
	}
	return fmt.Sprintf(`forge.EnvVar { name = %q, config_map_ref = %q, config_map_key = %q }`,
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
