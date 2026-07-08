package codegen

import (
	"strings"
	"testing"
)

// migrationConfigFields exercises every migration branch: non-sensitive
// str/int/bool/duration, a sensitive field with a ${NAME#KEY} override, a
// sensitive field with a bare ${NAME} (key falls back to lower(env_var)), and
// a sensitive field with NO per-env value (schema default applies → omitted).
func migrationConfigFields() []ConfigField {
	return []ConfigField{
		{Name: "log_level", ProtoType: "string", GoType: "string", EnvVar: "LOG_LEVEL", DefaultValue: "info"},
		{Name: "port", ProtoType: "int32", GoType: "int32", EnvVar: "PORT", DefaultValue: "8080"},
		{Name: "auto_migrate", ProtoType: "bool", GoType: "bool", EnvVar: "AUTO_MIGRATE", DefaultValue: "false"},
		{Name: "shutdown_timeout", ProtoType: "google.protobuf.Duration", GoType: "time.Duration", EnvVar: "SHUTDOWN_TIMEOUT", DefaultValue: "30s"},
		{Name: "internal_service_secret", ProtoType: "string", GoType: "string", EnvVar: "INTERNAL_SERVICE_SECRET", Sensitive: true, Required: true},
		{Name: "resend_api_key", ProtoType: "string", GoType: "string", EnvVar: "RESEND_API_KEY", Sensitive: true},
		{Name: "oauth_state_secret", ProtoType: "string", GoType: "string", EnvVar: "OAUTH_STATE_SECRET", Sensitive: true},
	}
}

func TestGenerateConfigKFromYAML_ExactBlock(t *testing.T) {
	env := map[string]any{
		"log_level":               "warn",
		"port":                    8080,      // yaml ints decode to int
		"auto_migrate":            true,      // yaml bools decode to bool
		"shutdown_timeout":        "45s",     // duration carried as a string
		"internal_service_secret": "${control-plane-internal#secret}", // ${NAME#KEY} override
		"oauth_state_secret":      "${control-plane-secrets}",         // bare ${NAME}, key defaults
		// resend_api_key intentionally absent → omitted (schema default).
	}
	got, err := GenerateConfigKFromYAML(migrationConfigFields(), env, "control-plane")
	if err != nil {
		t.Fatalf("GenerateConfigKFromYAML: %v", err)
	}

	want := "# Per-environment app config VALUES (user-owned — edit here).\n" +
		"# Migrated one-time from the sibling config.<env>.yaml. The typed AppConfig\n" +
		"# schema + projection functions are generated from proto/config/v1/config.proto;\n" +
		"# this instance supplies the per-env values they project. Only keys this env\n" +
		"# overrides are set — every other field inherits its AppConfig schema default\n" +
		"# (sensitive fields default to the control-plane-secrets Secret).\n" +
		"\n" +
		"import config_schema\n" +
		"\n" +
		"app_config: config_schema.AppConfig = {\n" +
		"    log_level = \"warn\"\n" +
		"    port = 8080\n" +
		"    auto_migrate = True\n" +
		"    shutdown_timeout = \"45s\"\n" +
		"    internal_service_secret = config_schema.ConfigSecretRef { name = \"control-plane-internal\", key = \"secret\" }\n" +
		"    oauth_state_secret = config_schema.ConfigSecretRef { name = \"control-plane-secrets\", key = \"oauth_state_secret\" }\n" +
		"}\n"

	if got != want {
		t.Fatalf("config.k mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestGenerateConfigKFromYAML_Sparse: a field the env file does not set is
// omitted entirely (the schema default applies), matching the sparse authoring
// of config.<env>.yaml and the Go projector's skip-when-absent behavior.
func TestGenerateConfigKFromYAML_Sparse(t *testing.T) {
	env := map[string]any{"log_level": "debug"}
	got, err := GenerateConfigKFromYAML(migrationConfigFields(), env, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `log_level = "debug"`) {
		t.Fatalf("set key must be emitted:\n%s", got)
	}
	for _, absent := range []string{"port", "auto_migrate", "shutdown_timeout", "internal_service_secret", "resend_api_key", "oauth_state_secret"} {
		if strings.Contains(got, absent+" =") {
			t.Fatalf("unset key %q must be omitted:\n%s", absent, got)
		}
	}
}

// TestGenerateConfigKFromYAML_SensitiveNonRefOmitted: a sensitive key whose
// value is NOT a parseable ${...} ref leaves the field on its schema default
// (the Go projector likewise ignores the value and wires the default backend).
func TestGenerateConfigKFromYAML_SensitiveNonRefOmitted(t *testing.T) {
	env := map[string]any{"internal_service_secret": "not-a-ref"}
	got, err := GenerateConfigKFromYAML(migrationConfigFields(), env, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "internal_service_secret") {
		t.Fatalf("non-ref sensitive value must be omitted (schema default applies):\n%s", got)
	}
}

// TestGenerateConfigKFromYAML_EmptyStringOmitted: a key present with an empty
// value is treated as unset (same footgun guard as isEmptyConfigValue).
func TestGenerateConfigKFromYAML_EmptyStringOmitted(t *testing.T) {
	env := map[string]any{"log_level": "  ", "port": 9090}
	got, err := GenerateConfigKFromYAML(migrationConfigFields(), env, "control-plane")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "log_level") {
		t.Fatalf("empty-string value must be omitted:\n%s", got)
	}
	if !strings.Contains(got, "port = 9090") {
		t.Fatalf("real value must survive:\n%s", got)
	}
}
