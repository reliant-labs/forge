package projectstore

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func boolp(b bool) *bool { return &b }

func sampleConfig() *config.ProjectConfig {
	return &config.ProjectConfig{
		Name:         "demo",
		ModulePath:   "github.com/acme/demo",
		Kind:         "service",
		Binary:       "shared",
		Version:      "1.2.3",
		ForgeVersion: "0.9.0",
		Components: []config.ComponentConfig{
			{Name: "api", Kind: "server", Path: "handlers/api", Ports: map[string]config.PortSpec{"http": {Port: 8080}}},
			{Name: "sweeper", Kind: "worker", Path: "workers/sweeper"},
			{Name: "nightly", Kind: "cron", Schedule: "0 0 * * *"},
			{Name: "ctrl", Kind: "operator", Group: "acme.dev", Version: "v1"},
			{Name: "tool", Kind: "binary", Path: "cmd/tool.go"},
		},
		Packs:    []string{"audit"},
		Database: config.DatabaseConfig{Driver: "postgres"},
	}
}

func TestMetaMirrorsConfig(t *testing.T) {
	s := New(sampleConfig())
	m := s.Meta()
	if m.Name != "demo" || m.ModulePath != "github.com/acme/demo" {
		t.Fatalf("meta identity mismatch: %+v", m)
	}
	if !m.IsServiceKind() || m.IsCLIKind() || m.IsLibraryKind() {
		t.Fatalf("kind helpers wrong: %+v", m)
	}
	if !m.IsBinaryShared() {
		t.Fatalf("expected shared binary")
	}
	if m.EffectiveForgeVersion() != "0.9.0" {
		t.Fatalf("forge version: %q", m.EffectiveForgeVersion())
	}
	if New(&config.ProjectConfig{}).Meta().EffectiveForgeVersion() != "0.0.0" {
		t.Fatalf("empty forge version should default to 0.0.0")
	}
}

func TestComponentKindFilters(t *testing.T) {
	s := New(sampleConfig())
	if len(s.Components()) != 5 {
		t.Fatalf("want 5 components, got %d", len(s.Components()))
	}
	if len(s.Servers()) != 1 || !s.Servers()[0].IsServer() {
		t.Fatalf("servers filter wrong")
	}
	if len(s.Workers()) != 1 || !s.Workers()[0].IsWorker() {
		t.Fatalf("workers filter wrong")
	}
	if len(s.Crons()) != 1 || s.Crons()[0].Schedule != "0 0 * * *" {
		t.Fatalf("crons filter wrong")
	}
	if len(s.Operators()) != 1 || !s.Operators()[0].IsOperator() {
		t.Fatalf("operators filter wrong")
	}
	if len(s.BinaryComponents()) != 1 || !s.BinaryComponents()[0].IsBinary() {
		t.Fatalf("binary filter wrong")
	}
	if s.Servers()[0].PrimaryPort() != 8080 {
		t.Fatalf("primary port wrong")
	}
}

func TestEmptyKindDefaultsToServer(t *testing.T) {
	s := New(&config.ProjectConfig{Components: []config.ComponentConfig{{Name: "x"}}})
	if !s.Components()[0].IsServer() {
		t.Fatalf("empty kind should be server")
	}
}

func TestFeaturesMirror(t *testing.T) {
	cfg := sampleConfig()
	cfg.Features.Deploy = boolp(false)
	s := New(cfg)
	if s.Features().DeployEnabled() {
		t.Fatalf("explicit deploy:false should resolve disabled")
	}
}

func TestAppendComponent(t *testing.T) {
	cfg := sampleConfig()
	s := New(cfg)
	s.AppendComponent(config.ComponentConfig{Name: "new", Kind: "server"})
	if len(cfg.Components) != 6 {
		t.Fatalf("append did not reach underlying config: %d", len(cfg.Components))
	}
	if len(s.Components()) != 6 {
		t.Fatalf("store view did not reflect append")
	}
}

func TestAppendWebhook(t *testing.T) {
	cfg := sampleConfig()
	s := New(cfg)
	if !s.AppendWebhook("api", config.WebhookConfig{Name: "stripe"}) {
		t.Fatalf("expected webhook append to succeed")
	}
	if s.AppendWebhook("nope", config.WebhookConfig{Name: "x"}) {
		t.Fatalf("expected false for missing component")
	}
	if len(cfg.Components[0].Webhooks) != 1 {
		t.Fatalf("webhook not appended to underlying config")
	}
}

func TestSetPacks(t *testing.T) {
	cfg := sampleConfig()
	s := New(cfg)
	s.SetPacks([]string{"a", "b"})
	if len(cfg.Packs) != 2 {
		t.Fatalf("SetPacks did not reach config")
	}
}

func TestSectionAccessors(t *testing.T) {
	cfg := sampleConfig()
	s := New(cfg)
	if s.Database().Driver != "postgres" {
		t.Fatalf("database accessor wrong")
	}
	if len(s.Packs()) != 1 || s.Packs()[0] != "audit" {
		t.Fatalf("packs accessor wrong")
	}
	if s.Config() != cfg {
		t.Fatalf("Config() should return the underlying pointer")
	}
}
