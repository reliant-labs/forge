package codegen

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// boundMessage is a one-field config message bound to binary, declared at
// config/v1/config.proto:line.
func boundMessage(name, binary string, line int) ConfigMessage {
	return ConfigMessage{
		Name:      name,
		Binary:    binary,
		ProtoFile: "config/v1/config.proto",
		Line:      line,
		Fields: []ConfigField{{
			Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32",
			EnvVar: strings.ToUpper(name) + "_PORT",
		}},
	}
}

// The repro from the forge-migration research: a subcommand LINE written
// where a binary name belongs, and a binary that does not exist, both used
// to generate green. Each is refused, named with its file:line, and the
// subcommand case says what `binary` actually is.
func TestValidateBinaryConfigBindings_RefusesUnknownBinaries(t *testing.T) {
	msgs := []ConfigMessage{
		boundMessage("ServerApiConfig", "x server api", 42),
		boundMessage("GhostConfig", "nonexistent", 57),
	}
	err := ValidateBinaryConfigBindings(msgs, []string{"x"})
	if err == nil {
		t.Fatal("bindings to binaries with no cmd/<name> generated cleanly; want a refusal")
	}
	got := err.Error()
	for _, want := range []string{
		"proto/config/v1/config.proto:42: message ServerApiConfig: binary_config.binary = \"x server api\"",
		"there is no cmd/x server api/ directory",
		"not a command line",
		"proto/config/v1/config.proto:57: message GhostConfig: binary_config.binary = \"nonexistent\"",
		"there is no cmd/nonexistent/ directory",
		"Binaries in this project (cmd/<name>): x",
		"forge scaffold binary <name>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal missing %q\n---\n%s", want, got)
		}
	}
}

// A binary's NAME and its cmd/ package directory can differ by -/_
// (`forge scaffold binary workspace-proxy` writes cmd/workspace_proxy); the
// refusal points at the directory that does exist.
func TestValidateBinaryConfigBindings_SuggestsTheDirectorySpelling(t *testing.T) {
	err := ValidateBinaryConfigBindings(
		[]ConfigMessage{boundMessage("ProxyConfig", "workspace-proxy", 3)},
		[]string{"server", "workspace_proxy"})
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), `Did you mean "workspace_proxy"?`) {
		t.Errorf("refusal should suggest the cmd/ spelling:\n%s", err)
	}
}

func TestValidateBinaryConfigBindings_AcceptsRealBinaries(t *testing.T) {
	msgs := []ConfigMessage{
		{Name: "BaseConfig"}, // a shared block: bound to nothing
		{Name: "AppConfig"},  // the project-global root: bound to nothing
		boundMessage("AdminConfig", "admin", 10),
		boundMessage("GatewayConfig", "gateway", 20),
	}
	if err := ValidateBinaryConfigBindings(msgs, []string{"admin", "gateway", "server"}); err != nil {
		t.Fatalf("valid bindings refused: %v", err)
	}
}

// "forge could not tell" must never render as "it does not exist": an
// unreadable cmd/ (nil binaries) skips the existence check.
func TestValidateBinaryConfigBindings_UnknownBinarySetSkipsExistence(t *testing.T) {
	if err := ValidateBinaryConfigBindings([]ConfigMessage{boundMessage("AdminConfig", "admin", 1)}, nil); err != nil {
		t.Fatalf("nil binary set must skip the existence check, got: %v", err)
	}
}

// A project with no cmd/ at all has no binaries, so any binding is refused.
func TestValidateBinaryConfigBindings_NoBinariesRefusesEveryBinding(t *testing.T) {
	err := ValidateBinaryConfigBindings([]ConfigMessage{boundMessage("AdminConfig", "admin", 1)}, []string{})
	if err == nil || !strings.Contains(err.Error(), "This project has no cmd/ binaries.") {
		t.Fatalf("want a refusal naming the empty binary set, got: %v", err)
	}
}

// Two messages bound to one binary used to project the FIRST message's
// fields for both (the KCL emitter looked the binary up, not the message).
// A binary has one config surface, so the second binding is refused.
func TestValidateBinaryConfigBindings_RefusesTwoConfigsForOneBinary(t *testing.T) {
	msgs := []ConfigMessage{
		boundMessage("AdminConfig", "admin", 10),
		boundMessage("AdminExtraConfig", "admin", 30),
	}
	err := ValidateBinaryConfigBindings(msgs, []string{"admin"})
	if err == nil {
		t.Fatal("two configs for one binary generated cleanly; want a refusal")
	}
	got := err.Error()
	if !strings.Contains(got, "proto/config/v1/config.proto:30: message AdminExtraConfig") ||
		!strings.Contains(got, "already bound by AdminConfig") {
		t.Errorf("refusal should name the second binding and the first:\n%s", got)
	}
	if strings.Contains(got, "config.proto:10:") {
		t.Errorf("the FIRST binding is not the offender and should not be listed:\n%s", got)
	}
}

// The position degrades rather than lying: no file renders no prefix, and
// a file with no line renders the file alone.
func TestConfigMessageWhere_Degrades(t *testing.T) {
	cases := []struct {
		m    ConfigMessage
		want string
	}{
		{ConfigMessage{}, ""},
		{ConfigMessage{ProtoFile: "config/v1/config.proto"}, "proto/config/v1/config.proto"},
		{ConfigMessage{ProtoFile: "config/v1/config.proto", Line: 7}, "proto/config/v1/config.proto:7"},
		{ConfigMessage{ProtoFile: "proto/config/v1/x.proto", Line: 2}, "proto/config/v1/x.proto:2"},
	}
	for _, c := range cases {
		if got := configMessageWhere(c.m); got != c.want {
			t.Errorf("configMessageWhere(%+v) = %q, want %q", c.m, got, c.want)
		}
	}
}

func TestProjectBinaries(t *testing.T) {
	dir := t.TempDir()
	if got := ProjectBinaries(dir); got == nil || len(got) != 0 {
		t.Fatalf("no cmd/ should be a KNOWN empty set, got %#v", got)
	}
	for _, d := range []string{"cmd/server", "cmd/admin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file under cmd/ is not a binary.
	if err := os.WriteFile(filepath.Join(dir, "cmd", "README.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := ProjectBinaries(dir), []string{"admin", "server"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ProjectBinaries = %v, want %v", got, want)
	}
}
