package codegen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateDeployConfig_BasicValuesAndSecrets(t *testing.T) {
	tmp := t.TempDir()
	in := DeployConfigGenInput{
		ProjectName: "myapp",
		EnvName:     "dev",
		KCLDir:      filepath.Join(tmp, "deploy", "kcl"),
		Fields: []ConfigField{
			{Name: "log_level", EnvVar: "LOG_LEVEL"},
			{Name: "rate_limit_rps", EnvVar: "RATE_LIMIT_RPS"},
			{Name: "database_url", EnvVar: "DATABASE_URL", Sensitive: true},
			{Name: "stripe_key", EnvVar: "STRIPE_KEY", Sensitive: true, Category: "stripe"},
			{Name: "stripe_webhook_secret", EnvVar: "STRIPE_WEBHOOK_SECRET", Sensitive: true, Category: "stripe"},
		},
		EnvConfig: map[string]any{
			"log_level":      "debug",
			"rate_limit_rps": 100,
			"database_url":   "${MY_DB_SECRET}",
		},
	}

	if err := GenerateDeployConfig(in); err != nil {
		t.Fatalf("GenerateDeployConfig: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(in.KCLDir, "dev", "config_gen.k"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	body := string(got)

	mustContain(t, body, "APP_ENV: [schema.EnvVar] = [")
	mustContain(t, body, "STRIPE_ENV: [schema.EnvVar] = [")
	// Non-sensitive value-bearing fields project through the per-env
	// ConfigMap (myapp-dev-config) instead of inline `value=`.
	mustContain(t, body, `name = "LOG_LEVEL", config_map_ref = "myapp-dev-config", config_map_key = "LOG_LEVEL"`)
	mustContain(t, body, `name = "RATE_LIMIT_RPS", config_map_ref = "myapp-dev-config", config_map_key = "RATE_LIMIT_RPS"`)
	// database_url is sensitive — even though rawVal is a ${...} ref, it
	// becomes a secret_ref override, not an inline value.
	mustContain(t, body, `name = "DATABASE_URL", secret_ref = "MY_DB_SECRET", secret_key = "database_url"`)
	// Stripe fields with no inline values still emit secret refs.
	mustContain(t, body, `name = "STRIPE_KEY", secret_ref = "myapp-secrets", secret_key = "stripe_key"`)
	mustContain(t, body, `name = "STRIPE_WEBHOOK_SECRET", secret_ref = "myapp-secrets", secret_key = "stripe_webhook_secret"`)
	// Non-sensitive values land in the generated ConfigMap's `data` block.
	mustContain(t, body, "CONFIG_MAPS: [schema.ConfigMap] = [")
	mustContain(t, body, `name = "myapp-dev-config"`)
	mustContain(t, body, `"LOG_LEVEL" = "debug"`)
	mustContain(t, body, `"RATE_LIMIT_RPS" = "100"`)
}

func TestGenerateDeployConfig_SkipsNonSensitiveWithoutValue(t *testing.T) {
	tmp := t.TempDir()
	in := DeployConfigGenInput{
		ProjectName: "myapp",
		EnvName:     "dev",
		KCLDir:      filepath.Join(tmp, "deploy", "kcl"),
		Fields: []ConfigField{
			{Name: "set_value", EnvVar: "SET_VALUE"},
			{Name: "no_value", EnvVar: "NO_VALUE"},
		},
		EnvConfig: map[string]any{"set_value": "hello"},
	}
	if err := GenerateDeployConfig(in); err != nil {
		t.Fatalf("GenerateDeployConfig: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(in.KCLDir, "dev", "config_gen.k"))
	body := string(got)
	mustContain(t, body, `name = "SET_VALUE"`)
	if strings.Contains(body, `name = "NO_VALUE"`) {
		t.Errorf("non-sensitive field with no value should be skipped, got:\n%s", body)
	}
	// SET_VALUE has a value -> ConfigMap entry; NO_VALUE doesn't.
	mustContain(t, body, `"SET_VALUE" = "hello"`)
	if strings.Contains(body, `"NO_VALUE" =`) {
		t.Errorf("non-sensitive field with no value should not appear in ConfigMap data, got:\n%s", body)
	}
}

func TestGenerateDeployConfig_NoFieldsNoFile(t *testing.T) {
	tmp := t.TempDir()
	if err := GenerateDeployConfig(DeployConfigGenInput{
		ProjectName: "myapp",
		EnvName:     "dev",
		KCLDir:      tmp,
	}); err != nil {
		t.Fatalf("GenerateDeployConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "dev", "config_gen.k")); !os.IsNotExist(err) {
		t.Errorf("expected no file; stat err=%v", err)
	}
}

func TestGenerateDeployConfig_SecretKeyOverride(t *testing.T) {
	tmp := t.TempDir()
	in := DeployConfigGenInput{
		ProjectName: "myapp",
		EnvName:     "prod",
		KCLDir:      filepath.Join(tmp, "deploy", "kcl"),
		Fields: []ConfigField{
			{Name: "database_url", EnvVar: "DATABASE_URL", Sensitive: true},
			{Name: "db_password", EnvVar: "DB_PASSWORD", Sensitive: true},
			{Name: "stripe_key", EnvVar: "STRIPE_KEY", Sensitive: true, Category: "stripe"},
		},
		EnvConfig: map[string]any{
			// Override both name and key — the existing cluster secret
			// uses kebab-case keys.
			"database_url": "${db-credentials#database-url}",
			// Override only the name.
			"db_password": "${db-credentials}",
			// Override only the key (empty name preserves default).
			// stripe_key relies on default name + default key.
		},
	}
	if err := GenerateDeployConfig(in); err != nil {
		t.Fatalf("GenerateDeployConfig: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(in.KCLDir, "prod", "config_gen.k"))
	body := string(got)
	mustContain(t, body, `name = "DATABASE_URL", secret_ref = "db-credentials", secret_key = "database-url"`)
	mustContain(t, body, `name = "DB_PASSWORD", secret_ref = "db-credentials", secret_key = "db_password"`)
	mustContain(t, body, `name = "STRIPE_KEY", secret_ref = "myapp-secrets", secret_key = "stripe_key"`)
}

// Empty-category elision: when every field in a category is a
// non-sensitive default-only (no per-env override), the category-list
// is omitted entirely so config_gen.k doesn't carry a noisy
// `BILLING_ENV: [schema.EnvVar] = []` stub. Categories that DO emit
// at least one entry still appear.
func TestGenerateDeployConfig_OmitsEmptyCategoryLists(t *testing.T) {
	tmp := t.TempDir()
	in := DeployConfigGenInput{
		ProjectName: "myapp",
		EnvName:     "dev",
		KCLDir:      filepath.Join(tmp, "deploy", "kcl"),
		Fields: []ConfigField{
			// APP_ENV: has a value -> emits.
			{Name: "log_level", EnvVar: "LOG_LEVEL"},
			// BILLING_ENV: only non-sensitive default-only fields ->
			// renderEnvVarEntry returns ok=false for both, so the
			// category list should be elided.
			{Name: "billing_grace_days", EnvVar: "BILLING_GRACE_DAYS", Category: "billing"},
			{Name: "billing_currency", EnvVar: "BILLING_CURRENCY", Category: "billing"},
			// STRIPE_ENV: a sensitive field -> always emits.
			{Name: "stripe_key", EnvVar: "STRIPE_KEY", Sensitive: true, Category: "stripe"},
		},
		EnvConfig: map[string]any{"log_level": "debug"},
	}
	if err := GenerateDeployConfig(in); err != nil {
		t.Fatalf("GenerateDeployConfig: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(in.KCLDir, "dev", "config_gen.k"))
	body := string(got)

	mustContain(t, body, "APP_ENV: [schema.EnvVar] = [")
	mustContain(t, body, "STRIPE_ENV: [schema.EnvVar] = [")
	if strings.Contains(body, "BILLING_ENV") {
		t.Errorf("empty BILLING_ENV category should be elided, got:\n%s", body)
	}
	// Defensive: no `[schema.EnvVar] = []` stubs anywhere.
	if strings.Contains(body, "[schema.EnvVar] = [\n]") || strings.Contains(body, "[schema.EnvVar] = []") {
		t.Errorf("expected no empty EnvVar list stubs, got:\n%s", body)
	}
}

func TestParseSecretRef(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantKey  string
		wantOK   bool
	}{
		{"${MY_SECRET}", "MY_SECRET", "", true},
		{"${db-credentials#database-url}", "db-credentials", "database-url", true},
		{"${ db-credentials # database-url }", "db-credentials", "database-url", true},
		{"plain-string", "", "", false},
		{"", "", "", false},
		{"${}", "", "", false},
		{"${#key}", "", "", false},
	}
	for _, c := range cases {
		gotName, gotKey, gotOK := parseSecretRef(c.in)
		if gotName != c.wantName || gotKey != c.wantKey || gotOK != c.wantOK {
			t.Errorf("parseSecretRef(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, gotName, gotKey, gotOK, c.wantName, c.wantKey, c.wantOK)
		}
	}
}

func TestStringifyConfigValue(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, ""},
		{"x", "x"},
		{true, "true"},
		{false, "false"},
		{int(42), "42"},
		{int64(1<<33), "8589934592"},
		{float64(3), "3"},
		{float64(3.14), "3.14"},
	}
	for _, c := range cases {
		if got := stringifyConfigValue(c.in); got != c.want {
			t.Errorf("stringifyConfigValue(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("expected substring %q in:\n%s", sub, s)
	}
}
