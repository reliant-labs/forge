package codegen

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// durationMessages is a config shape carrying the scaffold's duration fields.
// Durations are now google.protobuf.Duration proto fields (ProtoType
// "message", MessageType google.protobuf.Duration) — the config object IS the
// proto type, so the cmd shim consumes them via .AsDuration().
func durationMessages() []ConfigMessage {
	dur := func(name, goName, env, flag, def, desc string) ConfigField {
		return ConfigField{
			Name: name, GoName: goName, GoType: "*durationpb.Duration",
			ProtoType: "message", MessageType: "google.protobuf.Duration",
			EnvVar: env, Flag: flag, DefaultValue: def, Description: desc,
		}
	}
	return []ConfigMessage{{
		Name: "AppConfig",
		Fields: []ConfigField{
			{Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32", EnvVar: "PORT", Flag: "port", DefaultValue: "8080", Description: "HTTP server port"},
			{Name: "environment", GoName: "Environment", GoType: "string", ProtoType: "string", EnvVar: "ENVIRONMENT", Flag: "environment", DefaultValue: "production", Role: "CONFIG_FIELD_ROLE_MODE", Description: "Runtime environment"},
			dur("pre_stop_delay", "PreStopDelay", "PRE_STOP_DELAY", "pre-stop-delay", "5s", "drain pause (Go duration)"),
			dur("shutdown_timeout", "ShutdownTimeout", "SHUTDOWN_TIMEOUT", "shutdown-timeout", "30s", "drain budget (Go duration)"),
			dur("db_conn_max_idle_time", "DbConnMaxIdleTime", "DB_CONN_MAX_IDLE_TIME", "db-conn-max-idle-time", "5m", "idle cap (Go duration)"),
			dur("db_conn_max_lifetime", "DbConnMaxLifetime", "DB_CONN_MAX_LIFETIME", "db-conn-max-lifetime", "30m", "lifetime cap (Go duration)"),
		},
	}}
}

// TestCmdServer_DurationViaAsDuration pins how the rewired cmd shim consumes
// the proto config: duration fields are *durationpb.Duration on the proto
// type, so the shim projects them onto serverkit via .AsDuration() — never a
// string re-parse (Load resolved them once, into the message).
func TestCmdServer_DurationViaAsDuration(t *testing.T) {
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
		t.Errorf("cmd serve shim must not re-parse durations (Load owns parsing)\n%s", got)
	}
	for _, want := range []string{
		"skCfg.PreStopDelay = cfg.PreStopDelay.AsDuration()",
		"skCfg.ShutdownTimeout = cfg.ShutdownTimeout.AsDuration()",
		"skCfg.DBPoolTuning.ConnMaxIdleTime = cfg.DbConnMaxIdleTime.AsDuration()",
		"skCfg.DBPoolTuning.ConnMaxLifetime = cfg.DbConnMaxLifetime.AsDuration()",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cmd serve shim should consume the proto duration via %q\n%s", want, got)
		}
	}

	// Mode/Validate/DevAuthBypass are FREE FUNCS over the proto message,
	// never methods on a generated struct.
	if strings.Contains(got, "cfg.DevAuthBypass()") || strings.Contains(got, "cfg.Validate()") {
		t.Errorf("cmd serve shim must call the free funcs config.DevAuthBypass(cfg)/config.Validate(cfg), not methods\n%s", got)
	}
	if !strings.Contains(got, "config.Validate(cfg)") || !strings.Contains(got, "config.DevAuthBypass(cfg)") {
		t.Errorf("cmd serve shim must call config.Validate(cfg) and config.DevAuthBypass(cfg)\n%s", got)
	}

	// The rendered shim must stay syntactically valid Go (both the subset
	// field set and the full default scaffold field set).
	fset := token.NewFileSet()
	if _, perr := parser.ParseFile(fset, "serve.go", out, parser.AllErrors); perr != nil {
		t.Fatalf("rendered cmd serve shim does not parse: %v\n%s", perr, got)
	}
	full, err := templates.ProjectTemplates().Render("cmd-tree-serve.go.tmpl", CmdServerTemplateData{
		Module:       "example.com/proj",
		ConfigFields: DefaultConfigFieldNames(),
	})
	if err != nil {
		t.Fatalf("render (default fields): %v", err)
	}
	if _, perr := parser.ParseFile(fset, "serve.go", full, parser.AllErrors); perr != nil {
		t.Fatalf("rendered cmd serve shim (default fields) does not parse: %v\n%s", perr, string(full))
	}
}
