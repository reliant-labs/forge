package config

import "testing"

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
		{"frontend only", CIConfig{Lint: CILintConfig{Frontend: true}}, true},
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