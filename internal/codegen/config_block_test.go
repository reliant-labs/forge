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
	// FieldNames gates root-level Validate clauses — block leaves must
	// not leak in (a block leaf named LogFormat would otherwise emit a
	// Validate referencing c.LogFormat that doesn't exist at root).
	if data.FieldNames["MaxPerTick"] {
		t.Error("FieldNames must index ROOT fields only; found block leaf MaxPerTick")
	}
	if !data.FieldNames["Port"] {
		t.Error("FieldNames missing root field Port")
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

// TestGenerateWireGen_ConfigBlockByType is the worker acceptance case:
// a Deps field typed `config.TraderConfig` resolves to `cfg.Trader` by
// TYPE (no TODO, no nil-dep entry), while a naked scalar in the same
// Deps still falls through — and its hint now points at the
// config-block convention instead of the AppExtras two-step.
func TestGenerateWireGen_ConfigBlockByType(t *testing.T) {
	projectDir := t.TempDir()
	writeConfigDescriptor(t, projectDir, traderConfigMessages())

	workerDir := filepath.Join(projectDir, "workers", "trader")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package trader

import (
	"log/slog"

	"example.com/proj/pkg/config"
)

type Deps struct {
	Logger *slog.Logger
	Cfg    config.TraderConfig
	MaxPerTick int
}
`
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	workers := []BootstrapWorkerData{{
		Name: "trader", Package: "trader", ImportPath: "trader",
		FieldName: "Trader", VarName: "trader",
	}}
	if err := GenerateWireGen(nil, nil, workers, nil, "example.com/proj", projectDir, false, nil); err != nil {
		t.Fatalf("GenerateWireGen: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
	if err != nil {
		t.Fatalf("read wire_gen.go: %v", err)
	}
	content := string(data)

	// The block-typed field wires from cfg by type.
	if !strings.Contains(content, "Cfg: cfg.Trader,") {
		t.Errorf("expected `Cfg: cfg.Trader,` in wire_gen.go:\n%s", content)
	}
	if strings.Contains(content, "TODO: wire Cfg") {
		t.Errorf("config-block field must not carry a TODO:\n%s", content)
	}

	// The naked scalar still falls through — with the config-block hint.
	if !strings.Contains(content, "TODO: wire MaxPerTick") {
		t.Errorf("expected TODO for naked scalar MaxPerTick:\n%s", content)
	}
	if !strings.Contains(content, "scalar Deps fields are configuration") {
		t.Errorf("expected scalar config-block hint in UNRESOLVED header:\n%s", content)
	}
	if !strings.Contains(content, "message TraderConfig") {
		t.Errorf("scalar hint should carry the exact block-message snippet:\n%s", content)
	}
}

// TestGenerateWireGen_ConfigBlockPointer covers the `*config.<Block>`
// Deps spelling: wire_gen emits `&cfg.<Field>` (Config block fields are
// value-typed on Config, and cfg is *config.Config so the field is
// addressable).
func TestGenerateWireGen_ConfigBlockPointer(t *testing.T) {
	projectDir := t.TempDir()
	writeConfigDescriptor(t, projectDir, traderConfigMessages())

	handlerDir := filepath.Join(projectDir, "handlers", "api")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package api

import (
	"log/slog"

	"example.com/proj/pkg/config"
)

type Deps struct {
	Logger *slog.Logger
	Cfg    *config.TraderConfig
}
`
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	services := []ServiceDef{{Name: "APIService", ModulePath: "example.com/proj"}}
	if err := GenerateWireGen(services, nil, nil, nil, "example.com/proj", projectDir, false, nil); err != nil {
		t.Fatalf("GenerateWireGen: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
	if err != nil {
		t.Fatalf("read wire_gen.go: %v", err)
	}
	if !strings.Contains(string(data), "Cfg: &cfg.Trader,") {
		t.Errorf("expected `Cfg: &cfg.Trader,` for pointer block field:\n%s", data)
	}
}

// TestGenerateWireGen_ConfigBlockAmbiguous: two Config fields of the
// same block type make type-based resolution ambiguous — hard error
// listing the candidates, not a silent pick.
func TestGenerateWireGen_ConfigBlockAmbiguous(t *testing.T) {
	projectDir := t.TempDir()
	messages := traderConfigMessages()
	messages[0].Fields = append(messages[0].Fields, ConfigField{
		Name: "shadow_trader", GoName: "ShadowTrader", ProtoType: "message",
		MessageType: "TraderConfig",
	})
	writeConfigDescriptor(t, projectDir, messages)

	workerDir := filepath.Join(projectDir, "workers", "trader")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package trader

import "example.com/proj/pkg/config"

type Deps struct {
	Cfg config.TraderConfig
}
`
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	workers := []BootstrapWorkerData{{
		Name: "trader", Package: "trader", ImportPath: "trader",
		FieldName: "Trader", VarName: "trader",
	}}
	err := GenerateWireGen(nil, nil, workers, nil, "example.com/proj", projectDir, false, nil)
	if err == nil {
		t.Fatal("expected ambiguity error for two Config fields of type TraderConfig, got nil")
	}
	for _, want := range []string{"TraderConfig", "Trader", "ShadowTrader", "exactly one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ambiguity error missing %q: %v", want, err)
		}
	}
}

// TestConfigBlock_RegenerateIdempotent: generating twice produces
// byte-identical pkg/config/config.go AND pkg/app/wire_gen.go — the
// "forever TODO / forever diff" regression class this feature kills.
func TestConfigBlock_RegenerateIdempotent(t *testing.T) {
	projectDir := t.TempDir()
	writeConfigDescriptor(t, projectDir, traderConfigMessages())

	workerDir := filepath.Join(projectDir, "workers", "trader")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := `package trader

import (
	"log/slog"

	"example.com/proj/pkg/config"
)

type Deps struct {
	Logger *slog.Logger
	Cfg    config.TraderConfig
}
`
	if err := os.WriteFile(filepath.Join(workerDir, "worker.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	workers := []BootstrapWorkerData{{
		Name: "trader", Package: "trader", ImportPath: "trader",
		FieldName: "Trader", VarName: "trader",
	}}

	gen := func() (string, string) {
		if err := GenerateConfigLoader(traderConfigMessages(), projectDir, nil); err != nil {
			t.Fatalf("GenerateConfigLoader: %v", err)
		}
		if err := GenerateWireGen(nil, nil, workers, nil, "example.com/proj", projectDir, false, nil); err != nil {
			t.Fatalf("GenerateWireGen: %v", err)
		}
		cfgGo, err := os.ReadFile(filepath.Join(projectDir, "pkg", "config", "config.go"))
		if err != nil {
			t.Fatal(err)
		}
		wireGo, err := os.ReadFile(filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
		if err != nil {
			t.Fatal(err)
		}
		return string(cfgGo), string(wireGo)
	}

	cfg1, wire1 := gen()
	cfg2, wire2 := gen()
	if cfg1 != cfg2 {
		t.Error("pkg/config/config.go is not idempotent across regenerates")
	}
	if wire1 != wire2 {
		t.Error("pkg/app/wire_gen.go is not idempotent across regenerates")
	}
	if !strings.Contains(wire1, "Cfg: cfg.Trader,") {
		t.Errorf("expected stable `Cfg: cfg.Trader,` wiring:\n%s", wire1)
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
