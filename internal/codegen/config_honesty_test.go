package codegen

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// durationMessages is a config shape carrying the scaffold's duration
// fields plus a non-duration string for contrast.
func durationMessages() []ConfigMessage {
	return []ConfigMessage{{
		Name: "AppConfig",
		Fields: []ConfigField{
			{Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32", EnvVar: "PORT", Flag: "port", DefaultValue: "8080", Description: "HTTP server port"},
			{Name: "environment", GoName: "Environment", GoType: "string", ProtoType: "string", EnvVar: "ENVIRONMENT", Flag: "environment", DefaultValue: "production", Role: "CONFIG_FIELD_ROLE_MODE", Description: "Runtime environment"},
			{Name: "pre_stop_delay", GoName: "PreStopDelay", GoType: "string", ProtoType: "string", EnvVar: "PRE_STOP_DELAY", Flag: "pre-stop-delay", DefaultValue: "5s", Description: "drain pause (Go duration)"},
			{Name: "shutdown_timeout", GoName: "ShutdownTimeout", GoType: "string", ProtoType: "string", EnvVar: "SHUTDOWN_TIMEOUT", Flag: "shutdown-timeout", DefaultValue: "30s", Description: "drain budget (Go duration)"},
			{Name: "db_conn_max_idle_time", GoName: "DbConnMaxIdleTime", GoType: "string", ProtoType: "string", EnvVar: "DB_CONN_MAX_IDLE_TIME", Flag: "db-conn-max-idle-time", DefaultValue: "5m", Description: "idle cap (Go duration)"},
			{Name: "db_conn_max_lifetime", GoName: "DbConnMaxLifetime", GoType: "string", ProtoType: "string", EnvVar: "DB_CONN_MAX_LIFETIME", Flag: "db-conn-max-lifetime", DefaultValue: "30m", Description: "lifetime cap (Go duration)"},
		},
	}}
}

func renderConfigGo(t *testing.T, messages []ConfigMessage) string {
	t.Helper()
	targetDir := t.TempDir()
	if err := GenerateConfigLoader(messages, targetDir, nil); err != nil {
		t.Fatalf("GenerateConfigLoader() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(targetDir, "pkg", "config", "config.go"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return string(data)
}

// TestGenerateConfigLoader_FlagBeatsEnv pins the precedence fix: an
// explicit CLI flag (typed by an operator on THIS invocation) beats the
// ambient environment variable. The old order was env > flag, which
// made `./server --port 9090` silently ignore the flag whenever PORT
// happened to be exported.
func TestGenerateConfigLoader_FlagBeatsEnv(t *testing.T) {
	content := renderConfigGo(t, durationMessages())

	// One generic helper owns resolution; the 20 per-field if/else
	// ladders are gone.
	if !strings.Contains(content, "func loadField[") {
		t.Fatalf("generated config.go must emit the single generic loadField helper\n%s", content)
	}
	flagIdx := strings.Index(content, `cmd.Flags().Changed(flagName)`)
	envIdx := strings.Index(content, `os.LookupEnv(envVar)`)
	if flagIdx < 0 || envIdx < 0 {
		t.Fatalf("helper must consult both flag and env (flagIdx=%d envIdx=%d)\n%s", flagIdx, envIdx, content)
	}
	if flagIdx > envIdx {
		t.Errorf("flag must be consulted BEFORE env (flag beats env), got env first\n%s", content)
	}
	// The per-field env-then-flag ladder shape must be gone.
	if strings.Contains(content, "} else if cmd != nil && cmd.Flags().Changed(") {
		t.Errorf("per-field env-first ladders must be deleted in favor of loadField\n%s", content)
	}
}

// TestGenerateConfigLoader_DurationFieldsTyped pins config honesty for
// durations: duration-shaped fields become time.Duration on Config,
// parsed exactly once in Load with errors surfaced — not strings that
// every consumer re-parses (and silently zeroes on typos).
func TestGenerateConfigLoader_DurationFieldsTyped(t *testing.T) {
	content := renderConfigGo(t, durationMessages())

	for _, want := range []string{
		"PreStopDelay time.Duration",
		"ShutdownTimeout time.Duration",
		"DbConnMaxIdleTime time.Duration",
		"DbConnMaxLifetime time.Duration",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Config struct missing typed duration field %q", want)
		}
	}
	// Non-duration strings stay strings.
	if !strings.Contains(content, "Environment string") {
		t.Errorf("Environment must remain a plain string\n%s", content)
	}
}

// TestGenerateConfigLoader_EmitsTypedMode pins the dev-mode unification
// substrate: pkg/config exposes ONE typed Mode (zero value Production)
// that bootstrap, the auth middleware, and auth packs consume by
// injection instead of scattering os.Getenv("ENVIRONMENT") gates.
func TestGenerateConfigLoader_EmitsTypedMode(t *testing.T) {
	content := renderConfigGo(t, durationMessages())

	for _, want := range []string{
		"type Mode int",
		"ModeProduction Mode = iota",
		"ModeDevelopment",
		"func (c *Config) Mode() Mode",
		"func (m Mode) IsDev() bool",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("generated config.go missing %q\n%s", want, content)
		}
	}

	// Mode derivation is ANNOTATION-driven: it reads the field tagged
	// role=MODE (here Environment), selected by config_gen at build time
	// from ConfigField.Role — NOT by matching the name "Environment".
	if !strings.Contains(content, "strings.ToLower(c.Environment)") {
		t.Errorf("Mode() must derive from the role=MODE field's value\n%s", content)
	}
}

// TestGenerateConfigLoader_ModeFollowsAnnotationNotName proves the deleted
// name-magic: the dev-mode field is selected by the role annotation, so
// renaming it keeps Mode working, and an UNannotated field named
// "Environment" does NOT drive Mode.
func TestGenerateConfigLoader_ModeFollowsAnnotationNotName(t *testing.T) {
	// (1) Role field renamed to "Stage" — Mode must still read it.
	renamed := []ConfigMessage{{Name: "AppConfig", Fields: []ConfigField{
		{Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32", EnvVar: "PORT", Flag: "port", DefaultValue: "8080", Description: "port"},
		{Name: "stage", GoName: "Stage", GoType: "string", ProtoType: "string", EnvVar: "STAGE", Flag: "stage", DefaultValue: "production", Role: "CONFIG_FIELD_ROLE_MODE", Description: "mode"},
	}}}
	content := renderConfigGo(t, renamed)
	if !strings.Contains(content, "strings.ToLower(c.Stage)") {
		t.Errorf("renamed role field: Mode() must read c.Stage\n%s", content)
	}

	// (2) A field literally named Environment but WITHOUT the role must NOT
	// drive Mode — the project gets the always-production Mode() variant.
	unannotated := []ConfigMessage{{Name: "AppConfig", Fields: []ConfigField{
		{Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32", EnvVar: "PORT", Flag: "port", DefaultValue: "8080", Description: "port"},
		{Name: "environment", GoName: "Environment", GoType: "string", ProtoType: "string", EnvVar: "ENVIRONMENT", Flag: "environment", DefaultValue: "production", Description: "NOT role-tagged"},
	}}}
	content = renderConfigGo(t, unannotated)
	if strings.Contains(content, "strings.ToLower(c.Environment)") {
		t.Errorf("unannotated Environment must NOT drive Mode (no name-magic)\n%s", content)
	}
	if !strings.Contains(content, "func (c *Config) Mode() Mode { return ModeProduction }") {
		t.Errorf("unannotated config must emit the always-production Mode()\n%s", content)
	}
}

// TestCmdServer_NoSilentDurationReparse pins the deletion of the silent
// `if err == nil` duration re-parses in internal/cli/serve.go: the typed config
// now carries time.Duration values, so the shim assigns them directly.
func TestCmdServer_NoSilentDurationReparse(t *testing.T) {
	fields := ConfigFieldNamesFromMessages(durationMessages())
	out, err := templates.ProjectTemplates().Render("cmd-tree-serve.go.tmpl", CmdServerTemplateData{
		Module:       "example.com/proj",
		ConfigFields: fields,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)
	if strings.Contains(got, "time.ParseDuration") {
		t.Errorf("internal/cli/serve.go must not re-parse durations (Load owns parsing)\n%s", got)
	}
	if strings.Contains(got, "perr == nil") {
		t.Errorf("the silent `if err == nil` duration swallows must be deleted\n%s", got)
	}
	if !strings.Contains(got, "skCfg.PreStopDelay = cfg.PreStopDelay") {
		t.Errorf("internal/cli/serve.go should assign the typed duration directly\n%s", got)
	}

	// The rendered shim must stay syntactically valid Go.
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "server.go", out, parser.AllErrors); perr != nil {
		t.Fatalf("rendered internal/cli/serve.go does not parse: %v\n%s", perr, got)
	}

	// Same with the full default scaffold field set.
	full, err := templates.ProjectTemplates().Render("cmd-tree-serve.go.tmpl", CmdServerTemplateData{
		Module:       "example.com/proj",
		ConfigFields: DefaultConfigFieldNames(),
	})
	if err != nil {
		t.Fatalf("render (default fields): %v", err)
	}
	if _, perr := parser.ParseFile(fset, "server.go", full, parser.AllErrors); perr != nil {
		t.Fatalf("rendered internal/cli/serve.go (default fields) does not parse: %v\n%s", perr, string(full))
	}
}
