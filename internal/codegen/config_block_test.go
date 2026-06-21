// Component config-block tests — the kalshi-trader acceptance shape
// (fr-ad24278452): a worker declares `WTIPersistMaxPerTick int` in Deps
// and wire_gen can never resolve it because scalars are CONFIGURATION,
// not collaborators. The fix is a per-component config block:
//
//	proto/config/v1/config.proto:
//	    message TraderConfig {
//	        int32 max_per_tick = 1 [(forge.v1.config) = {
//	            env_var: "TRADER_MAX_PER_TICK", default_value: "10", ...}];
//	    }
//	    message AppConfig { ... TraderConfig trader = 21; }
//
//	workers/trader/worker.go:
//	    type Deps struct {
//	        Logger *slog.Logger
//	        Cfg    config.TraderConfig // resolved by TYPE → cfg.Trader
//	    }
//
// These tests cover the chain end-to-end at the codegen layer:
// descriptor-shaped messages → pkg/config nested struct + env/flag/
// .env.example → wire_gen type-based resolution (value + pointer +
// ambiguity error) → per-env deploy projection of a block leaf →
// regenerate idempotency.
package codegen

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// traderConfigMessages is the canonical fixture: a root AppConfig with
// one scalar field + one block reference, and the TraderConfig block
// with an int leaf (the kalshi MaxPerTick shape).
func traderConfigMessages() []ConfigMessage {
	return []ConfigMessage{
		{
			Name: "AppConfig",
			Fields: []ConfigField{
				{
					Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32",
					EnvVar: "PORT", Flag: "port", DefaultValue: "8080",
					Description: "HTTP server port",
				},
				{
					Name: "trader", GoName: "Trader", ProtoType: "message",
					MessageType: "TraderConfig",
				},
			},
		},
		{
			Name: "TraderConfig",
			Fields: []ConfigField{
				{
					Name: "max_per_tick", GoName: "MaxPerTick", GoType: "int32", ProtoType: "int32",
					EnvVar: "TRADER_MAX_PER_TICK", Flag: "trader-max-per-tick", DefaultValue: "10",
					Description: "Maximum WTI persists per tick",
				},
			},
		},
	}
}

// writeConfigDescriptor writes go.mod + gen/forge_descriptor.json so
// loadConfigBlockIndex (via ParseConfigProto's go.mod walk-up) sees the
// same config messages GenerateConfigLoader consumed.
func writeConfigDescriptor(t *testing.T, projectDir string, messages []ConfigMessage) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(projectDir, "go.mod"), []byte("module example.com/proj\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "gen"), 0o755); err != nil {
		t.Fatal(err)
	}
	desc := ForgeDescriptor{Configs: messages}
	data, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "gen", "forge_descriptor.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildConfigTemplateData_PartitionsBlocks(t *testing.T) {
	data := BuildConfigTemplateData(traderConfigMessages())

	// Root: just Port. The block reference itself is not a root leaf.
	if len(data.RootFields) != 1 || data.RootFields[0].GoName != "Port" {
		t.Fatalf("RootFields = %+v, want exactly [Port]", data.RootFields)
	}
	// All leaves: Port + the block leaf with a qualified GoPath.
	if len(data.Fields) != 2 {
		t.Fatalf("Fields = %+v, want 2 leaves", data.Fields)
	}
	if data.Fields[1].GoPath != "Trader.MaxPerTick" {
		t.Errorf("block leaf GoPath = %q, want Trader.MaxPerTick", data.Fields[1].GoPath)
	}
	if len(data.BlockTypes) != 1 || data.BlockTypes[0].TypeName != "TraderConfig" {
		t.Fatalf("BlockTypes = %+v, want [TraderConfig]", data.BlockTypes)
	}
	if len(data.BlockFields) != 1 || data.BlockFields[0].GoName != "Trader" || data.BlockFields[0].TypeName != "TraderConfig" {
		t.Fatalf("BlockFields = %+v, want [Trader TraderConfig]", data.BlockFields)
	}
	// No field carries role=MODE here, so no dev-mode field is selected.
	// (Semantic role selection replaced the old name-keyed FieldNames map.)
	if data.RoleModeField != "" {
		t.Errorf("RoleModeField = %q, want empty (no role=MODE field declared)", data.RoleModeField)
	}

	refs := ConfigBlocksFromMessages(traderConfigMessages())
	if len(refs) != 1 || refs[0].FieldName != "Trader" || refs[0].TypeName != "TraderConfig" {
		t.Fatalf("ConfigBlocksFromMessages = %+v, want [{Trader TraderConfig}]", refs)
	}
}

// TestBuildConfigTemplateData_FlatStaysFlat pins backwards
// compatibility: messages with no block references render exactly the
// legacy flat shape (every field a root leaf, GoPath == GoName).
func TestBuildConfigTemplateData_FlatStaysFlat(t *testing.T) {
	data := BuildConfigTemplateData(DefaultConfigMessages())
	if len(data.BlockTypes) != 0 || len(data.BlockFields) != 0 {
		t.Fatalf("flat scaffold config must produce no blocks; got types=%+v fields=%+v", data.BlockTypes, data.BlockFields)
	}
	if len(data.Fields) != len(data.RootFields) {
		t.Fatalf("flat config: Fields (%d) != RootFields (%d)", len(data.Fields), len(data.RootFields))
	}
	for _, f := range data.Fields {
		if f.GoPath != f.GoName {
			t.Errorf("flat field %s: GoPath %q != GoName", f.GoName, f.GoPath)
		}
	}
}

func TestGenerateConfigLoader_ComponentBlock(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateConfigLoader(traderConfigMessages(), dir, nil); err != nil {
		t.Fatalf("GenerateConfigLoader: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "pkg", "config", "config.go"))
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	content := string(data)

	for _, want := range []string{
		"type TraderConfig struct {",
		"MaxPerTick int32",
		"Trader TraderConfig",
		`loadField(cmd, "trader-max-per-tick", "TRADER_MAX_PER_TICK", "10", true, false, false, "Trader.MaxPerTick", parseInt32)`,
		"cfg.Trader.MaxPerTick, err = loadField",
		`cmd.Flags().Int32("trader-max-per-tick", 10,`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("config.go missing %q\n--- content ---\n%s", want, content)
		}
	}

	// The generated file must be syntactically valid Go.
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "config.go", data, 0); err != nil {
		t.Fatalf("generated config.go does not parse: %v\n--- content ---\n%s", err, content)
	}

	// Block leaves flow into .env.example like any other field.
	env, err := os.ReadFile(filepath.Join(dir, ".env.example"))
	if err != nil {
		t.Fatalf("read .env.example: %v", err)
	}
	if !strings.Contains(string(env), "TRADER_MAX_PER_TICK=10") {
		t.Errorf(".env.example missing TRADER_MAX_PER_TICK=10:\n%s", env)
	}
}

// NOTE: the config-block-by-TYPE wiring tests
// (TestGenerateWireGen_ConfigBlock{ByType,Pointer,Ambiguous} and the
// wire half of TestConfigBlock_RegenerateIdempotent) were removed with
// the old name-matched wire_gen unit (FORGE_SHAPE_REDESIGN §2). The
// config.go projection idempotency below is preserved.

// TestConfigBlock_RegenerateIdempotent: generating twice produces
// byte-identical pkg/config/config.go (the "forever diff" regression
// class this feature kills).
func TestConfigBlock_RegenerateIdempotent(t *testing.T) {
	projectDir := t.TempDir()
	writeConfigDescriptor(t, projectDir, traderConfigMessages())

	gen := func() string {
		if err := GenerateConfigLoader(traderConfigMessages(), projectDir, nil); err != nil {
			t.Fatalf("GenerateConfigLoader: %v", err)
		}
		cfgGo, err := os.ReadFile(filepath.Join(projectDir, "pkg", "config", "config.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(cfgGo)
	}

	cfg1 := gen()
	cfg2 := gen()
	if cfg1 != cfg2 {
		t.Error("pkg/config/config.go is not idempotent across regenerates")
	}
}

// TestGenerateDeployConfig_BlockLeafProjection: a per-env value for a
// block leaf in config.<env>.yaml (flat snake_case key, same namespace
// as root fields) projects into the generated KCL exactly like a root
// field — ConfigMap entry + configMapKeyRef EnvVar. The block REFERENCE
// field (message-typed, no env_var) is skipped.
func TestGenerateDeployConfig_BlockLeafProjection(t *testing.T) {
	dir := t.TempDir()

	// Flatten root + block messages the way generatePerEnvDeployConfig
	// does — block leaves participate via their own ConfigMessage.
	var fields []ConfigField
	for _, m := range traderConfigMessages() {
		fields = append(fields, m.Fields...)
	}

	err := GenerateDeployConfig(DeployConfigGenInput{
		ProjectName: "demo",
		EnvName:     "prod",
		KCLDir:      filepath.Join(dir, "deploy", "kcl"),
		Fields:      fields,
		EnvConfig: map[string]any{
			"max_per_tick": 50,
		},
	})
	if err != nil {
		t.Fatalf("GenerateDeployConfig: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "deploy", "kcl", "prod", "config_gen.k"))
	if err != nil {
		t.Fatalf("read config_gen.k: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, `forge.EnvVar { name = "TRADER_MAX_PER_TICK", config_map_ref = "demo-prod-config", config_map_key = "TRADER_MAX_PER_TICK" }`) {
		t.Errorf("expected configMapKeyRef EnvVar for block leaf:\n%s", content)
	}
	if !strings.Contains(content, `"TRADER_MAX_PER_TICK" = "50"`) {
		t.Errorf("expected ConfigMap data entry for block leaf value:\n%s", content)
	}
	// The block-reference field has no EnvVar and must not leak into
	// the rendered lists.
	if strings.Contains(content, "TraderConfig") || strings.Contains(content, `name = ""`) {
		t.Errorf("block reference field must not render an EnvVar entry:\n%s", content)
	}
}
