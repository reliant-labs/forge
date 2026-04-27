package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCIConfig_EffectiveGoVersion(t *testing.T) {
	tests := []struct {
		version string
		want    string
	}{
		{"", "1.26"},
		{"1.24", "1.24"},
		{"1.25", "1.25"},
	}
	for _, tt := range tests {
		cfg := &CIConfig{GoVersion: tt.version}
		got := cfg.EffectiveGoVersion()
		if got != tt.want {
			t.Errorf("EffectiveGoVersion() with version=%q: got %q, want %q", tt.version, got, tt.want)
		}
	}
}

func TestCIConfig_IsLintEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  CIConfig
		want bool
	}{
		{"zero value = all enabled", CIConfig{}, true},
		{"golangci only", CIConfig{Lint: CILintConfig{Golangci: true}}, true},
		{"buf only", CIConfig{Lint: CILintConfig{Buf: true}}, true},
		{"buf breaking only", CIConfig{Lint: CILintConfig{BufBreaking: true}}, true},
		{"frontend only", CIConfig{Lint: CILintConfig{Frontend: true}}, true},
		{"migration safety only", CIConfig{Lint: CILintConfig{MigrationSafety: true}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsLintEnabled()
			if got != tt.want {
				t.Errorf("IsLintEnabled(): got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCIConfig_IsTestRaceEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  CIConfig
		want bool
	}{
		{"zero value = enabled", CIConfig{}, true},
		{"race true", CIConfig{Test: CITestConfig{Race: true}}, true},
		{"coverage only (race false)", CIConfig{Test: CITestConfig{Coverage: true}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsTestRaceEnabled()
			if got != tt.want {
				t.Errorf("IsTestRaceEnabled(): got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCIConfig_IsVulnScanEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  CIConfig
		want bool
	}{
		{"zero value = all enabled", CIConfig{}, true},
		{"go only", CIConfig{VulnScan: CIVulnConfig{Go: true}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsVulnScanEnabled()
			if got != tt.want {
				t.Errorf("IsVulnScanEnabled(): got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCIConfig_EffectivePermContents(t *testing.T) {
	tests := []struct {
		contents string
		want     string
	}{
		{"", "read"},
		{"write", "write"},
	}
	for _, tt := range tests {
		cfg := &CIConfig{Permissions: CIPermConfig{Contents: tt.contents}}
		got := cfg.EffectivePermContents()
		if got != tt.want {
			t.Errorf("EffectivePermContents() with contents=%q: got %q, want %q", tt.contents, got, tt.want)
		}
	}
}

func TestMigrationSafetyConfigDefaults(t *testing.T) {
	cfg := MigrationSafetyConfig{}
	if !cfg.IsEnabled() {
		t.Fatal("zero-value migration safety config should be enabled")
	}
	if got := cfg.EffectiveUnsafeAddColumn(); got != "error" {
		t.Errorf("EffectiveUnsafeAddColumn() = %q, want error", got)
	}
	if got := cfg.EffectiveDestructiveChange(); got != "error" {
		t.Errorf("EffectiveDestructiveChange() = %q, want error", got)
	}
	if got := cfg.EffectiveVolatileDefault(); got != "warn" {
		t.Errorf("EffectiveVolatileDefault() = %q, want warn", got)
	}
}

func TestMigrationSafetyConfigYAMLRoundTrip(t *testing.T) {
	yamlStr := `enabled: false
unsafe_add_column: warn
destructive_change: off
volatile_default: error
allowed_destructive:
  - "*_drop_legacy.up.sql"
`
	var cfg MigrationSafetyConfig
	if err := yaml.Unmarshal([]byte(yamlStr), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.IsEnabled() {
		t.Fatal("expected enabled=false to disable migration safety")
	}
	if got := cfg.EffectiveUnsafeAddColumn(); got != "warn" {
		t.Errorf("EffectiveUnsafeAddColumn() = %q, want warn", got)
	}
	if got := cfg.EffectiveDestructiveChange(); got != "off" {
		t.Errorf("EffectiveDestructiveChange() = %q, want off", got)
	}
	if got := cfg.EffectiveVolatileDefault(); got != "error" {
		t.Errorf("EffectiveVolatileDefault() = %q, want error", got)
	}
	if len(cfg.AllowedDestructive) != 1 || cfg.AllowedDestructive[0] != "*_drop_legacy.up.sql" {
		t.Fatalf("AllowedDestructive = %#v", cfg.AllowedDestructive)
	}
}

func TestCIExtraJob_EffectiveRunsOn(t *testing.T) {
	tests := []struct {
		runsOn string
		want   string
	}{
		{"", "ubuntu-latest"},
		{"self-hosted", "self-hosted"},
	}
	for _, tt := range tests {
		job := &CIExtraJob{RunsOn: tt.runsOn}
		got := job.EffectiveRunsOn()
		if got != tt.want {
			t.Errorf("EffectiveRunsOn() with runsOn=%q: got %q, want %q", tt.runsOn, got, tt.want)
		}
	}
}

func TestServiceConfig_KindAndScheduleYAMLRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		yamlStr  string
		wantKind string
		wantSch  string
	}{
		{
			"worker with cron kind and schedule",
			"name: cleanup\ntype: worker\nkind: cron\npath: workers/cleanup\nschedule: \"*/5 * * * *\"\n",
			"cron",
			"*/5 * * * *",
		},
		{
			"worker with no kind",
			"name: processor\ntype: worker\npath: workers/processor\n",
			"",
			"",
		},
		{
			"go_service with no kind",
			"name: api\ntype: go_service\npath: handlers/api\nport: 8080\n",
			"",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg ServiceConfig
			if err := yaml.Unmarshal([]byte(tt.yamlStr), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", cfg.Kind, tt.wantKind)
			}
			if cfg.Schedule != tt.wantSch {
				t.Errorf("Schedule = %q, want %q", cfg.Schedule, tt.wantSch)
			}

			// Round-trip: marshal and unmarshal again
			out, err := yaml.Marshal(&cfg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var cfg2 ServiceConfig
			if err := yaml.Unmarshal(out, &cfg2); err != nil {
				t.Fatalf("unmarshal round-trip: %v", err)
			}
			if cfg2.Kind != tt.wantKind {
				t.Errorf("round-trip Kind = %q, want %q", cfg2.Kind, tt.wantKind)
			}
			if cfg2.Schedule != tt.wantSch {
				t.Errorf("round-trip Schedule = %q, want %q", cfg2.Schedule, tt.wantSch)
			}
		})
	}
}

func TestFrontendConfig_KindYAMLRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		yamlStr  string
		wantKind string
	}{
		{
			"mobile kind",
			"name: mobile-app\ntype: react-native\nkind: mobile\npath: frontends/mobile-app\nport: 8081\n",
			"mobile",
		},
		{
			"web kind (default)",
			"name: web\ntype: nextjs\npath: frontends/web\nport: 8080\n",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg FrontendConfig
			if err := yaml.Unmarshal([]byte(tt.yamlStr), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", cfg.Kind, tt.wantKind)
			}

			// Round-trip
			out, err := yaml.Marshal(&cfg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var cfg2 FrontendConfig
			if err := yaml.Unmarshal(out, &cfg2); err != nil {
				t.Fatalf("unmarshal round-trip: %v", err)
			}
			if cfg2.Kind != tt.wantKind {
				t.Errorf("round-trip Kind = %q, want %q", cfg2.Kind, tt.wantKind)
			}
		})
	}
}

func TestPackageConfig_KindYAMLRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		yamlStr  string
		wantKind string
	}{
		{"eventbus", "name: events\nkind: eventbus\n", "eventbus"},
		{"client", "name: stripe\nkind: client\n", "client"},
		{"generic (no kind)", "name: utils\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cfg PackageConfig
			if err := yaml.Unmarshal([]byte(tt.yamlStr), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if cfg.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", cfg.Kind, tt.wantKind)
			}

			out, err := yaml.Marshal(&cfg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var cfg2 PackageConfig
			if err := yaml.Unmarshal(out, &cfg2); err != nil {
				t.Fatalf("unmarshal round-trip: %v", err)
			}
			if cfg2.Kind != tt.wantKind {
				t.Errorf("round-trip Kind = %q, want %q", cfg2.Kind, tt.wantKind)
			}
		})
	}
}

func TestDeployConfig_EffectiveRegistry(t *testing.T) {
	tests := []struct {
		reg  string
		want string
	}{
		{"", "ghcr"},
		{"ecr", "ecr"},
		{"gar", "gar"},
	}
	for _, tt := range tests {
		cfg := &DeployConfig{Registry: tt.reg}
		got := cfg.EffectiveRegistry()
		if got != tt.want {
			t.Errorf("EffectiveRegistry() with reg=%q: got %q, want %q", tt.reg, got, tt.want)
		}
	}
}

func TestDeployConfig_IsConcurrencyEnabled(t *testing.T) {
	tests := []struct {
		name string
		cfg  DeployConfig
		want bool
	}{
		{"zero value = enabled", DeployConfig{}, true},
		{"explicitly enabled", DeployConfig{Concurrency: DeployConcurrency{Enabled: true}}, true},
		{"cancel in progress only (enabled false)", DeployConfig{Concurrency: DeployConcurrency{CancelInProgress: true}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.IsConcurrencyEnabled()
			if got != tt.want {
				t.Errorf("IsConcurrencyEnabled(): got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAPIKeyConfig_EffectiveAPIKeyHeader(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", "X-API-Key"},
		{"X-Custom-Key", "X-Custom-Key"},
		{"Authorization", "Authorization"},
	}

	for _, tt := range tests {
		cfg := APIKeyConfig{Header: tt.header}
		got := cfg.EffectiveAPIKeyHeader()
		if got != tt.want {
			t.Errorf("EffectiveAPIKeyHeader() with header=%q: got %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestJWTConfig_EffectiveSigningMethod(t *testing.T) {
	tests := []struct {
		method string
		want   string
	}{
		{"", "RS256"},
		{"HS256", "HS256"},
		{"ES256", "ES256"},
	}

	for _, tt := range tests {
		cfg := JWTConfig{SigningMethod: tt.method}
		got := cfg.EffectiveSigningMethod()
		if got != tt.want {
			t.Errorf("EffectiveSigningMethod() with method=%q: got %q, want %q", tt.method, got, tt.want)
		}
	}
}
