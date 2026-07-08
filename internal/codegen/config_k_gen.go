// Package codegen — config_k_gen.go is the MIGRATION half of the config-as-KCL
// story: it projects an existing per-env `config.<env>.yaml` into the
// user-owned KCL values file `deploy/kcl/<env>/config.k`.
//
// Where config_schema_gen.go emits the TYPE (`schema AppConfig` +
// `schema ConfigSecretRef`) and config_projection_gen.go emits the BEHAVIOR
// (appConfigEnvVars / appConfigConfigMap), THIS file emits the VALUES —
// a single `app_config: AppConfig = { ... }` instance the two generated
// functions are applied to. Unlike the schema/projection files (generated,
// DO NOT EDIT), config.k is USER-OWNED after migration: it is the one-time
// projection of the sibling yaml into the typed model, and the author edits
// it thereafter.
//
// The migration is faithful to how `config.<env>.yaml` is authored — SPARSE.
// Only keys the user actually set are materialized; every other field falls
// back to its AppConfig schema default (which the Go projector honors too:
// a non-sensitive field with no per-env value is skipped, and a sensitive
// field with no override gets the default `<project>-secrets` backend).
//
// Field projection rules (mirroring deploy_config_gen.go so the resulting
// AppConfig renders byte-identical env output via the projection functions):
//
//   - non-sensitive key with a value -> `<field> = <typed literal>`
//     (str/duration quoted, int/float bare, bool True/False).
//   - sensitive key whose value is a "${NAME}" / "${NAME#KEY}" secret
//     reference -> `<field> = ConfigSecretRef { name = "<NAME>", key =
//     "<KEY or lower(env_var)>" }`, parsed EXACTLY like
//     deploy_config_gen.go:parseSecretRef.
//   - sensitive key absent (or present but not a parseable ${...} ref) ->
//     omitted; the schema's default-backend ConfigSecretRef applies, which
//     is exactly what the Go projector wires for an un-overridden sensitive
//     field.
//
// Phase 3 is ADDITIVE: this emitter is standalone + tested and is NOT wired
// into the generate pipeline. It does not retire `config.<env>.yaml` and does
// not touch the Go projector (deploy_config_gen.go). The Phase 4 cutover owns
// swapping the projector for the typed path once parity is proven.
package codegen

import (
	"fmt"
	"strconv"
	"strings"
)

// GenerateConfigKFromYAML projects a loaded per-env config map (the
// map[string]any that internal/config.LoadEnvironmentConfig returns from
// `config.<env>.yaml`) into a `deploy/kcl/<env>/config.k` values file: a
// typed `app_config: AppConfig = { ... }` instance carrying only the keys the
// env file actually set.
//
// fields is the proto-derived config field set (same slice the schema and
// projection emitters consume); projectName is forge.yaml `name`, used only
// to document the default secret backend in a comment (the ConfigSecretRef
// defaults themselves live in the generated schema).
func GenerateConfigKFromYAML(fields []ConfigField, envConfig map[string]any, projectName string) (string, error) {
	var b strings.Builder

	b.WriteString("# Per-environment app config VALUES (user-owned — edit here).\n")
	b.WriteString("# Migrated one-time from the sibling config.<env>.yaml. The typed AppConfig\n")
	b.WriteString("# schema + projection functions are generated from proto/config/v1/config.proto;\n")
	b.WriteString("# this instance supplies the per-env values they project. Only keys this env\n")
	b.WriteString("# overrides are set — every other field inherits its AppConfig schema default\n")
	b.WriteString(fmt.Sprintf("# (sensitive fields default to the %s-secrets Secret).\n\n", projectName))

	// The AppConfig / ConfigSecretRef schemas live in the sibling
	// config_schema.k module; KCL does not share top-level symbols across
	// separately-imported modules, so import + qualify them (see
	// ConfigSchemaModule).
	b.WriteString(fmt.Sprintf("import %s\n\n", ConfigSchemaModule))

	// Collect body lines in proto field order so the file is deterministic.
	var lines []string
	for _, f := range fields {
		// Block-reference / unbound fields carry no env binding and no
		// per-env value of their own.
		if f.EnvVar == "" {
			continue
		}
		rawVal, hasVal := envConfig[f.Name]
		if !hasVal || isEmptyConfigValue(rawVal) {
			// Sparse: unset keys inherit the schema default.
			continue
		}

		if f.Sensitive {
			// Only a parseable ${NAME} / ${NAME#KEY} reference yields a typed
			// ConfigSecretRef override. Anything else (a non-ref value) leaves
			// the field on its default-backend schema default — the same
			// result the Go projector produces for an un-overridden sensitive
			// field.
			s, ok := rawVal.(string)
			if !ok {
				continue
			}
			name, key, ok := parseSecretRef(s)
			if !ok {
				continue
			}
			if key == "" {
				key = strings.ToLower(f.EnvVar)
			}
			lines = append(lines, fmt.Sprintf(
				"    %s = %s.ConfigSecretRef { name = %q, key = %q }", f.Name, ConfigSchemaModule, name, key))
			continue
		}

		lines = append(lines, fmt.Sprintf("    %s = %s", f.Name, kclLiteralFromValue(f, rawVal)))
	}

	b.WriteString(fmt.Sprintf("app_config: %s.AppConfig = {\n", ConfigSchemaModule))
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("}\n")

	return b.String(), nil
}

// kclLiteralFromValue renders a YAML-decoded per-env value as a KCL literal in
// the field's KCL type (see kclTypeForProtoConfig): strings/durations quoted,
// ints/floats bare, bools as True/False. It is the value-driven twin of
// kclConfigDefaultLiteral (which renders the proto DEFAULT), reusing the same
// type mapping so a migrated value and a proto default of the same field
// render identically.
func kclLiteralFromValue(f ConfigField, v any) string {
	switch kclTypeForProtoConfig(f) {
	case "int", "float":
		// stringifyConfigValue renders YAML's float64 integers as integers and
		// keeps numeric spellings bare — the KCL literal form for int/float.
		return stringifyConfigValue(v)
	case "bool":
		if bv, ok := v.(bool); ok {
			if bv {
				return "True"
			}
			return "False"
		}
		if isKCLTruthy(stringifyConfigValue(v)) {
			return "True"
		}
		return "False"
	default: // str (covers durations, carried as strings)
		return strconv.Quote(stringifyConfigValue(v))
	}
}
