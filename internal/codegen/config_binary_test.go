package codegen

import (
	"go/format"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// perBinaryMessages models the target shape: a shared BaseConfig DEFINITION
// composed by two binaries, each also carrying leaves only it reads.
func perBinaryMessages() []ConfigMessage {
	base := ConfigMessage{
		Name: "BaseConfig",
		Fields: []ConfigField{
			{Name: "port", GoName: "Port", GoType: "int32", ProtoType: "int32",
				EnvVar: "PORT", Flag: "port", DefaultValue: "8080"},
			{Name: "log_level", GoName: "LogLevel", GoType: "string", ProtoType: "string",
				EnvVar: "LOG_LEVEL", Flag: "log-level", DefaultValue: "info"},
		},
	}
	admin := ConfigMessage{
		Name:   "AdminConfig",
		Binary: "admin",
		Fields: []ConfigField{
			{Name: "base", GoName: "Base", ProtoType: "message", MessageType: "BaseConfig"},
			{Name: "admin_api_key", GoName: "AdminApiKey", GoType: "string", ProtoType: "string",
				EnvVar: "ADMIN_API_KEY", Flag: "admin-api-key", Sensitive: true},
		},
	}
	gateway := ConfigMessage{
		Name:   "GatewayConfig",
		Binary: "gateway",
		Fields: []ConfigField{
			{Name: "base", GoName: "Base", ProtoType: "message", MessageType: "BaseConfig"},
			{Name: "upstream_timeout_ms", GoName: "UpstreamTimeoutMs", GoType: "int32", ProtoType: "int32",
				EnvVar: "UPSTREAM_TIMEOUT_MS", Flag: "upstream-timeout-ms", DefaultValue: "500"},
		},
	}
	return []ConfigMessage{base, admin, gateway}
}

func envVarsOf(fields []ConfigField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.EnvVar)
	}
	return out
}

func goPathsOf(fields []ConfigTemplateField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.GoPath)
	}
	return out
}

// The defining property: each binary's surface is its own leaves plus the
// shared base, and NOTHING from a sibling.
func TestBinaryConfigs_AreDisjoint(t *testing.T) {
	bcs := BinaryConfigsFromMessages(perBinaryMessages())
	if len(bcs) != 2 {
		t.Fatalf("got %d binary configs, want 2 (only annotated messages)", len(bcs))
	}

	admin, gateway := bcs[0], bcs[1]
	if admin.Binary != "admin" || gateway.Binary != "gateway" {
		t.Fatalf("binaries = %q/%q, want admin/gateway", admin.Binary, gateway.Binary)
	}

	// Own leaves are exactly the binary-specific ones.
	if got, want := goPathsOf(admin.OwnFields), []string{"AdminApiKey"}; !reflect.DeepEqual(got, want) {
		t.Errorf("admin own fields = %v, want %v", got, want)
	}
	if got, want := goPathsOf(gateway.OwnFields), []string{"UpstreamTimeoutMs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("gateway own fields = %v, want %v", got, want)
	}

	// The shared base is reached through the block field, qualified — the
	// same GoPath mechanism component config blocks already use.
	wantShared := []string{"Base.Port", "Base.LogLevel"}
	if got := goPathsOf(admin.SharedFields); !reflect.DeepEqual(got, wantShared) {
		t.Errorf("admin shared fields = %v, want %v", got, wantShared)
	}
	if got := goPathsOf(gateway.SharedFields); !reflect.DeepEqual(got, wantShared) {
		t.Errorf("gateway shared fields = %v, want %v", got, wantShared)
	}

	// Neither binary's surface mentions the other's field.
	for _, p := range goPathsOf(admin.Fields) {
		if p == "UpstreamTimeoutMs" {
			t.Error("admin config leaked gateway's field")
		}
	}
	for _, p := range goPathsOf(gateway.Fields) {
		if p == "AdminApiKey" {
			t.Error("gateway config leaked admin's field")
		}
	}
}

// Deleting a field from one binary's message must not change the other's
// surface. This is the user's stated requirement, asserted directly.
func TestBinaryConfigs_DeletingAFieldDoesNotAffectSiblings(t *testing.T) {
	before := BinaryConfigsFromMessages(perBinaryMessages())
	gatewayBefore := goPathsOf(before[1].Fields)

	// Drop admin's own field entirely.
	msgs := perBinaryMessages()
	msgs[1].Fields = msgs[1].Fields[:1] // base only
	after := BinaryConfigsFromMessages(msgs)

	if got := goPathsOf(after[0].OwnFields); len(got) != 0 {
		t.Errorf("admin own fields after delete = %v, want none", got)
	}
	if got := goPathsOf(after[1].Fields); !reflect.DeepEqual(got, gatewayBefore) {
		t.Errorf("gateway surface changed after editing admin: %v -> %v", gatewayBefore, got)
	}
}

// The env projection for a binary is built from ConfigFieldsForBinary, so
// this set IS what lands in that workload's Deployment.
func TestConfigFieldsForBinary_OnlyThatBinarysEnvVars(t *testing.T) {
	msgs := perBinaryMessages()

	adminEnv := envVarsOf(ConfigFieldsForBinary(msgs, "admin"))
	wantAdmin := []string{"PORT", "LOG_LEVEL", "ADMIN_API_KEY"}
	if !reflect.DeepEqual(adminEnv, wantAdmin) {
		t.Errorf("admin env vars = %v, want %v", adminEnv, wantAdmin)
	}

	gwEnv := envVarsOf(ConfigFieldsForBinary(msgs, "gateway"))
	wantGW := []string{"PORT", "LOG_LEVEL", "UPSTREAM_TIMEOUT_MS"}
	if !reflect.DeepEqual(gwEnv, wantGW) {
		t.Errorf("gateway env vars = %v, want %v", gwEnv, wantGW)
	}

	// The security payoff: admin's secret is absent from gateway's manifest.
	for _, e := range gwEnv {
		if e == "ADMIN_API_KEY" {
			t.Error("gateway workload would carry admin's secret env var")
		}
	}

	if got := ConfigFieldsForBinary(msgs, "nonexistent"); got != nil {
		t.Errorf("unknown binary = %v, want nil", got)
	}
}

// A project that annotates NOTHING must be completely unaffected: no binary
// configs, so every caller stays on the single-AppConfig path.
func TestBinaryConfigs_UnannotatedProjectIsUnchanged(t *testing.T) {
	if got := BinaryConfigsFromMessages(DefaultConfigMessages()); len(got) != 0 {
		t.Errorf("unannotated project produced %d binary configs, want 0", len(got))
	}

	// And the existing flat template data is byte-for-byte what it was:
	// the scaffold's fields all land on the root Config with unqualified
	// GoPaths, exactly as before this feature existed.
	data := BuildConfigTemplateData(DefaultConfigMessages())
	if len(data.Fields) == 0 {
		t.Fatal("expected the default scaffold to produce config fields")
	}
	for _, f := range data.Fields {
		if f.GoPath != f.GoName {
			t.Errorf("field %s has qualified GoPath %q; the single-AppConfig shape must stay flat", f.GoName, f.GoPath)
		}
	}
	if data.RoleModeField == "" {
		t.Error("role=MODE selection regressed for the unannotated project")
	}
}

// A shared block is a DEFINITION, not a binary: it must never produce a
// config surface of its own even though it carries config fields.
func TestBinaryConfigs_SharedBlockIsNotABinary(t *testing.T) {
	for _, bc := range BinaryConfigsFromMessages(perBinaryMessages()) {
		if bc.MessageName == "BaseConfig" {
			t.Error("BaseConfig must not be treated as a binary config")
		}
	}
}

func renderConfigGo(t *testing.T, msgs []ConfigMessage) string {
	t.Helper()
	d := BuildConfigTemplateData(msgs)
	d.Module = "example.com/app"
	out, err := templates.ProjectTemplates().Render("config.go.tmpl", d)
	if err != nil {
		t.Fatalf("render config.go.tmpl: %v", err)
	}
	// Every .go render goes through the canonical formatter on write
	// (checksums.WriteGeneratedFile), so compile-shape assertions are made
	// against the formatted bytes, which is what lands on disk.
	formatted, err := format.Source(out)
	if err != nil {
		t.Fatalf("rendered config.go is not valid Go: %v\n%s", err, out)
	}
	return string(formatted)
}

// The single-binary project — the common case — must not pay for per-binary
// config: its generated pkg/config carries the same surface it always did and
// nothing per-binary at all.
func TestConfigTemplate_SingleBinaryIsUnchanged(t *testing.T) {
	got := renderConfigGo(t, DefaultConfigMessages())

	for _, want := range []string{
		"type Config = configv1.AppConfig",
		"func RegisterFlags(cmd *cobra.Command)",
		"func Load(cmd *cobra.Command) (*Config, error)",
		"func Validate(cfg *Config) error",
		"func ModeOf(cfg *Config) Mode",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("single-binary config.go missing %q", want)
		}
	}
	if strings.Contains(got, "Per-binary configs") {
		t.Error("single-binary config.go must not carry the per-binary section")
	}
}

// A per-binary project gets one type + one flag pair + one loader PER BINARY,
// and the two binaries' surfaces do not mention each other.
func TestConfigTemplate_PerBinarySurface(t *testing.T) {
	msgs := perBinaryMessages()
	got := renderConfigGo(t, msgs)

	for _, want := range []string{
		"type AdminConfig = configv1.AdminConfig",
		"type GatewayConfig = configv1.GatewayConfig",
		// Shared block -> persistent flags on the root.
		"func RegisterAdminConfigSharedFlags(cmd *cobra.Command)",
		"forgeconfig.RegisterSharedFlags(cmd, &configv1.AdminConfig{})",
		// Own leaves -> local flags on the binary's subcommand.
		"func RegisterAdminConfigFlags(cmd *cobra.Command)",
		"forgeconfig.RegisterOwnFlags(cmd, &configv1.AdminConfig{})",
		"func LoadAdminConfig(cmd *cobra.Command) (*AdminConfig, error)",
		"func LoadGatewayConfig(cmd *cobra.Command) (*GatewayConfig, error)",
		"func ValidateAdminConfig(cfg *AdminConfig) error",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("per-binary config.go missing %q", want)
		}
	}
}

// A project that has moved ENTIRELY to per-binary configs — no AppConfig left
// — must still generate valid Go rather than aliasing a type that is gone.
func TestConfigTemplate_PurePerBinaryOmitsRootConfig(t *testing.T) {
	msgs := perBinaryMessages() // BaseConfig + two annotated binaries, no AppConfig
	got := renderConfigGo(t, msgs)

	if strings.Contains(got, "type Config = configv1.") {
		t.Error("no AppConfig exists, so no Config alias should be emitted")
	}
	if strings.Contains(got, "func RegisterFlags(cmd *cobra.Command)") {
		t.Error("project-global RegisterFlags should be omitted when there is no root config")
	}
	// The per-binary half is still fully present.
	if !strings.Contains(got, "func LoadAdminConfig(") {
		t.Error("per-binary loaders must still be emitted")
	}
	// Mode is re-exported unconditionally — it is a library type, not tied
	// to the root config.
	if !strings.Contains(got, "type Mode = forgeconfig.RuntimeMode") {
		t.Error("Mode alias should always be emitted")
	}
}
