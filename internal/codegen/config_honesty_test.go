package codegen

import (
	"go/ast"
	"go/parser"
	"go/token"
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

// TestCmdServer_DurationViaAsDuration pins how the cmd scaffold consumes the
// proto config: duration fields are *durationpb.Duration on the proto type, so
// the scaffold projects them onto serverkit via .AsDuration() — never a string
// re-parse (Load resolved them once, into the message).
//
// The projections are read out of the PARSED scaffold and checked against the
// duration fields the config messages actually declare. Deriving the expected
// set from the producer is the point: the old form listed four hand-written
// assignment strings, so a fifth duration field could be added to the config
// and silently dropped on the floor by the scaffold with this test still
// green. Now an unprojected duration field fails here by construction.
func TestCmdServer_DurationViaAsDuration(t *testing.T) {
	msgs := durationMessages()
	fields := ConfigFieldNamesFromMessages(msgs)
	out, err := templates.ProjectTemplates().Render("cmd-tree-serve.go.tmpl", CmdServerTemplateData{
		Module:       "example.com/proj",
		ConfigFields: fields,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(out)

	fset := token.NewFileSet()
	file, perr := parser.ParseFile(fset, "serve.go", out, parser.AllErrors)
	if perr != nil {
		t.Fatalf("rendered cmd serve scaffold does not parse: %v\n%s", perr, got)
	}

	// Every duration field the config declares must be projected, and each
	// projection must go through .AsDuration() on that same config field.
	wantDurations := durationGoNames(msgs)
	if len(wantDurations) == 0 {
		t.Fatal("the config fixture declares NO duration fields — the projection assertions below " +
			"would iterate an empty set and pass without checking anything")
	}
	projected := asDurationProjections(file)
	for _, name := range wantDurations {
		if !projected[name] {
			t.Errorf("config declares duration field %s but the serve scaffold never projects it "+
				"via cfg.%s.AsDuration() — the env var exists and silently does nothing\n%s",
				name, name, got)
		}
	}

	// Durations are resolved ONCE by Load, into the message. A re-parse here
	// would be a second, divergent interpretation of the same string.
	if calls := serveCalls(file); calls["ParseDuration"] {
		t.Errorf("cmd serve scaffold re-parses durations (Load owns parsing)\n%s", got)
	}

	// Validate is a FREE FUNC over the proto message, never a method on a
	// generated struct. Checked as a call shape: `config.Validate(cfg)` takes
	// the config as an ARGUMENT, `cfg.Validate()` as a receiver.
	if !callsFreeFuncWithArg(file, "Validate", "cfg") {
		t.Errorf("cmd serve scaffold must call the free func config.Validate(cfg) — passing the "+
			"message as an argument, not invoking a method on it\n%s", got)
	}

	// The scaffold must also stay valid Go for the full default field set,
	// which exercises every conditional block at once.
	full, err := templates.ProjectTemplates().Render("cmd-tree-serve.go.tmpl", CmdServerTemplateData{
		Module:       "example.com/proj",
		ConfigFields: DefaultConfigFieldNames(),
	})
	if err != nil {
		t.Fatalf("render (default fields): %v", err)
	}
	if _, perr := parser.ParseFile(fset, "serve.go", full, parser.AllErrors); perr != nil {
		t.Fatalf("rendered cmd serve scaffold (default fields) does not parse: %v\n%s", perr, string(full))
	}
}

// durationGoNames returns the GoName of every google.protobuf.Duration field
// the config messages declare — the set the scaffold is obliged to project.
func durationGoNames(msgs []ConfigMessage) []string {
	var out []string
	for _, m := range msgs {
		for _, f := range m.Fields {
			if f.MessageType == "google.protobuf.Duration" {
				out = append(out, f.GoName)
			}
		}
	}
	return out
}

// asDurationProjections returns the set of config field names the file reads
// through `<x>.<Field>.AsDuration()`. Keyed by the CONFIG field name rather
// than by the serverkit field it lands on, because the config side is what the
// producer enumerates — where each value is assigned is a serverkit detail
// that may legitimately move.
func asDurationProjections(file *ast.File) map[string]bool {
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AsDuration" {
			return true
		}
		if inner, isSel := sel.X.(*ast.SelectorExpr); isSel {
			found[inner.Sel.Name] = true
		}
		return true
	})
	return found
}

// serveCalls returns the set of function names the file calls, by trailing
// identifier.
func serveCalls(file *ast.File) map[string]bool {
	called := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			called[fn.Name] = true
		case *ast.SelectorExpr:
			called[fn.Sel.Name] = true
		}
		return true
	})
	return called
}

// callsFreeFuncWithArg reports whether the file calls a function named name
// passing the identifier arg — i.e. `pkg.name(arg)`, the free-function shape,
// as opposed to `arg.name()`.
func callsFreeFuncWithArg(file *ast.File, name, arg string) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name != name {
				return true
			}
		case *ast.SelectorExpr:
			// Reject the method shape: `cfg.Validate()` has the config as
			// the receiver, which is exactly what this must not be.
			if fn.Sel.Name != name {
				return true
			}
			if id, isIdent := fn.X.(*ast.Ident); isIdent && id.Name == arg {
				return true
			}
		default:
			return true
		}
		if id, isIdent := call.Args[0].(*ast.Ident); isIdent && id.Name == arg {
			found = true
		}
		return true
	})
	return found
}
