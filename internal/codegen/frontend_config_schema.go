package codegen

import (
	"encoding/json"
	"fmt"
	"sort"
)

// FrontendConfigSchemaFile is the generated key schema's path inside a
// frontend, relative to the frontend root: beside config_gen.ts, and written
// by the same step.
const FrontendConfigSchemaFile = "src/lib/config_schema_gen.json"

// FrontendConfigSchemaVersion is the schema document's format version. A
// reader that finds a NEWER version than it understands skips the check
// rather than misreading it.
const FrontendConfigSchemaVersion = 1

// Key sources in a FrontendConfigSchema.
const (
	// FrontendConfigKeySourceConfig is a field of the frontend's
	// (forge.v1.frontend_config) message, read through config_gen.ts.
	FrontendConfigKeySourceConfig = "config"
	// FrontendConfigKeySourceRuntime is a key forge's own generated frontend
	// code reads from the document (FrontendRuntimeKeys).
	FrontendConfigKeySourceRuntime = "runtime"
)

// FrontendRuntimeKeys are the runtime config keys forge's GENERATED frontend
// code reads from window.__FORGE_CONFIG__ whether or not the frontend
// declares a config message: src/lib/otel_gen.ts (the browser telemetry
// knobs KCL's render.k derives from a frontend's `telemetry` block, and the
// API origin it propagates traces to), shared-web's native-login.ts (the API
// URL), and the ENVIRONMENT every rendered document carries. They are valid
// `runtime_config` keys for every frontend.
//
// TestFrontendRuntimeKeysMatchTheGeneratedReaders pins this list against the
// templates that read them, so a reader added there without a key added here
// fails a test rather than a user's render.
var FrontendRuntimeKeys = []string{
	"API_URL",
	"CONSOLE_CAPTURE",
	"ENVIRONMENT",
	"OTEL_ENDPOINT",
	"REPLAY_SAMPLE_RATE",
}

// FrontendConfigSchema is the machine-readable statement of which keys a
// frontend's runtime config document may carry — its config message's fields
// plus FrontendRuntimeKeys — and which it MUST carry (Required, RequiredWhen).
// The required-ness fields are additive within version 1: a reader that
// predates them still reads the key set, and simply does not refuse a
// missing key.
//
// It exists for the frontend whose code is in ANOTHER repository. The project
// that declares such a frontend's environments supplies its runtime_config
// values, but cannot see the proto that defines its fields, so a misspelled
// key used to render into config.js, be ignored by the bundle, and leave the
// field silently on its default. The frontend's own repo writes this file
// beside config_gen.ts; the declaring project reads it at render and refuses
// a key it does not list.
type FrontendConfigSchema struct {
	Comment  string                    `json:"//"`
	Version  int                       `json:"version"`
	Frontend string                    `json:"frontend"`
	Message  string                    `json:"message"`
	Keys     []FrontendConfigSchemaKey `json:"keys"`
}

// FrontendConfigSchemaKey is one key a document may carry.
type FrontendConfigSchemaKey struct {
	Name string `json:"name"`
	// Type is the value's type as config_gen.ts validates it: "string",
	// "number" or "boolean".
	Type   string `json:"type"`
	Source string `json:"source"`
	// Required is a key every document must carry, non-empty: a field
	// declared `required: true` with no default.
	Required bool `json:"required,omitempty"`
	// RequiredWhen lists the conditions under which the document must carry
	// this key with a value other than Default
	// ((forge.v1.config).required_when). Any one holding is enough.
	RequiredWhen []RequiredWhenKey `json:"required_when,omitempty"`
	// Default is the field's proto default, recorded for a RequiredWhen key:
	// a document carrying exactly the default does not satisfy the
	// condition (the mode it names is one the default is wrong for).
	Default string `json:"default,omitempty"`
}

// MissingKey is one key a runtime config document lacks.
type MissingKey struct {
	Key string
	// When is the condition that made it required; nil for a key required in
	// every mode.
	When *RequiredWhenKey
}

// Reason is the sentence a refusal prints for the key.
func (m MissingKey) Reason() string {
	if m.When == nil {
		return m.Key + " (required)"
	}
	return fmt.Sprintf("%s (required when %s is %q)", m.Key, m.When.Key, m.When.Equals)
}

// MissingRequired is every key the schema requires that doc does not carry.
// present(key) reports whether the document carries the key with a usable
// value, and literal(key) its literal value, when it has one (a value only
// known at deploy time, like another workload's URL, has none and so never
// satisfies — or fails — an `equals` condition, nor matches a default).
//
// It is the same judgment config_gen.ts's missingRequiredConfig makes at boot,
// made at render by a project that cannot run the bundle.
func (s FrontendConfigSchema) MissingRequired(present func(key string) bool, literal func(key string) (string, bool)) []MissingKey {
	var out []MissingKey
	for _, k := range s.Keys {
		if k.Required && !present(k.Name) {
			out = append(out, MissingKey{Key: k.Name})
			continue
		}
		for _, c := range k.RequiredWhen {
			if v, ok := literal(c.Key); !ok || v != c.Equals {
				continue
			}
			satisfied := present(k.Name)
			if v, ok := literal(k.Name); ok && k.Default != "" && v == k.Default {
				satisfied = false
			}
			if !satisfied {
				c := c
				out = append(out, MissingKey{Key: k.Name, When: &c})
			}
			break
		}
	}
	return out
}

// GenerateFrontendConfigSchema renders fc's key schema (FrontendConfigSchemaFile).
func GenerateFrontendConfigSchema(fc FrontendConfig) (string, error) {
	doc := FrontendConfigSchema{
		Comment: "Code generated by forge from the (forge.v1.frontend_config) message. DO NOT EDIT. " +
			"Read at render by a project that declares this frontend's environments, to refuse runtime_config keys this frontend does not read.",
		Version:  FrontendConfigSchemaVersion,
		Frontend: fc.Frontend,
		Message:  fc.MessageName,
	}
	seen := map[string]bool{}
	for _, f := range fc.Fields {
		if f.MessageType != "" {
			continue
		}
		key := tsConfigKeyRaw(f)
		if seen[key] {
			continue
		}
		seen[key] = true
		k := FrontendConfigSchemaKey{
			Name: key, Type: frontendConfigValueType(f), Source: FrontendConfigKeySourceConfig,
			Required:     frontendFieldRequired(f),
			RequiredWhen: frontendRequiredWhen(fc, f),
		}
		if len(k.RequiredWhen) > 0 {
			k.Default = f.DefaultValue
		}
		doc.Keys = append(doc.Keys, k)
	}
	for _, key := range FrontendRuntimeKeys {
		if seen[key] {
			continue
		}
		seen[key] = true
		doc.Keys = append(doc.Keys, FrontendConfigSchemaKey{Name: key, Type: "string", Source: FrontendConfigKeySourceRuntime})
	}
	sort.Slice(doc.Keys, func(i, j int) bool { return doc.Keys[i].Name < doc.Keys[j].Name })
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode config schema for %s: %w", fc.Frontend, err)
	}
	return string(out) + "\n", nil
}

// frontendConfigValueType is a field's type as config_gen.ts validates it.
func frontendConfigValueType(f ConfigField) string {
	switch kclTypeForProtoConfig(f) {
	case "int", "float":
		return "number"
	case "bool":
		return "boolean"
	default:
		return "string"
	}
}

// ParseFrontendConfigSchema reads a FrontendConfigSchemaFile. ok is false for
// a document this forge cannot interpret (a newer version), which a caller
// treats as "no schema" rather than an error: the frontend's repo is ahead.
func ParseFrontendConfigSchema(data []byte) (FrontendConfigSchema, bool, error) {
	var doc FrontendConfigSchema
	if err := json.Unmarshal(data, &doc); err != nil {
		return FrontendConfigSchema{}, false, err
	}
	if doc.Version > FrontendConfigSchemaVersion {
		return doc, false, nil
	}
	return doc, true, nil
}
