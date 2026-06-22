package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/reliant-labs/forge/pkg/forgepb"
)

// buildFileLayerMessage compiles a synthetic root config with:
//   - a top-level scalar with a default + env + flag (log_level)
//   - a sensitive field with env but NO flag (api_key)
//   - a nested config block (db) with a default + env + flag leaf (db.host)
//
// It is the fixture for the config-FILE layer + full precedence tests. JSON
// names are the snake field names; protojson accepts both the json_name and
// the proto field name, so the YAML/JSON documents key off these names.
func buildFileLayerMessage(t *testing.T) protoreflect.Message {
	t.Helper()

	field := func(name string, num int32, typ descriptorpb.FieldDescriptorProto_Type, opt *forgepb.ConfigFieldOptions) *descriptorpb.FieldDescriptorProto {
		fdp := &descriptorpb.FieldDescriptorProto{
			Name:     proto.String(name),
			Number:   proto.Int32(num),
			Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
			Type:     typ.Enum(),
			JsonName: proto.String(name),
		}
		if opt != nil {
			fo := &descriptorpb.FieldOptions{}
			proto.SetExtension(fo, forgepb.E_Config, opt)
			fdp.Options = fo
		}
		return fdp
	}

	dbMsg := &descriptorpb.DescriptorProto{
		Name: proto.String("DBConfig"),
		Field: []*descriptorpb.FieldDescriptorProto{
			field("host", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, &forgepb.ConfigFieldOptions{
				EnvVar: "DB_HOST", Flag: "db-host", DefaultValue: "localhost", Description: "db host",
			}),
			field("port", 2, descriptorpb.FieldDescriptorProto_TYPE_INT32, &forgepb.ConfigFieldOptions{
				EnvVar: "DB_PORT", Flag: "db-port", DefaultValue: "5432", Description: "db port",
			}),
			field("password", 3, descriptorpb.FieldDescriptorProto_TYPE_STRING, &forgepb.ConfigFieldOptions{
				EnvVar: "DB_PASSWORD", Sensitive: true, Description: "db password (sensitive)",
			}),
		},
	}

	dbFieldDesc := &descriptorpb.FieldDescriptorProto{
		Name:     proto.String("db"),
		Number:   proto.Int32(4),
		Label:    descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
		Type:     descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum(),
		TypeName: proto.String(".config.filelayertest.v1.DBConfig"),
		JsonName: proto.String("db"),
	}

	rootMsg := &descriptorpb.DescriptorProto{
		Name: proto.String("AppConfig"),
		Field: []*descriptorpb.FieldDescriptorProto{
			field("log_level", 1, descriptorpb.FieldDescriptorProto_TYPE_STRING, &forgepb.ConfigFieldOptions{
				EnvVar: "LOG_LEVEL", Flag: "log-level", DefaultValue: "info", Description: "log level",
			}),
			field("api_key", 2, descriptorpb.FieldDescriptorProto_TYPE_STRING, &forgepb.ConfigFieldOptions{
				EnvVar: "API_KEY", Sensitive: true, Description: "api key (sensitive, no flag)",
			}),
			dbFieldDesc,
		},
	}

	fdp := &descriptorpb.FileDescriptorProto{
		Name:        proto.String("config_filelayertest.proto"),
		Package:     proto.String("config.filelayertest.v1"),
		Syntax:      proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{rootMsg, dbMsg},
	}

	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatalf("protodesc.NewFile: %v", err)
	}
	return dynamicpb.NewMessage(fd.Messages().ByName("AppConfig"))
}

func blockStrOf(t *testing.T, m protoreflect.Message, block, leaf string) string {
	t.Helper()
	bf := m.Descriptor().Fields().ByName(protoreflect.Name(block))
	sub := m.Get(bf).Message()
	return sub.Get(sub.Descriptor().Fields().ByName(protoreflect.Name(leaf))).String()
}

func blockIntOf2(t *testing.T, m protoreflect.Message, block, leaf string) int64 {
	t.Helper()
	bf := m.Descriptor().Fields().ByName(protoreflect.Name(block))
	sub := m.Get(bf).Message()
	return sub.Get(sub.Descriptor().Fields().ByName(protoreflect.Name(leaf))).Int()
}

// writeFile writes content to a temp file with the given extension and returns
// its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return p
}

// cmdWithConfigFlag registers the message's flags AND the --config flag (via
// the public RegisterFlags), returning the command ready to Set() flags on.
func cmdWithConfigFlag(t *testing.T, msg proto.Message) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "test", RunE: func(*cobra.Command, []string) error { return nil }}
	if err := RegisterFlags(cmd, msg); err != nil {
		t.Fatalf("RegisterFlags: %v", err)
	}
	// Persistent flags are merged into Flags() at parse time; force the merge
	// so Set()/Changed() on cmd.Flags() see --config in these tests.
	cmd.Flags().AddFlagSet(cmd.PersistentFlags())
	return cmd
}

func TestRegisterFlags_AlwaysRegistersConfigFlag(t *testing.T) {
	m := buildFileLayerMessage(t)
	cmd := &cobra.Command{Use: "test"}
	if err := RegisterFlags(cmd, m.Interface()); err != nil {
		t.Fatalf("RegisterFlags: %v", err)
	}
	// --config is ALWAYS present (the "on, not dormant" guarantee).
	if f := cmd.PersistentFlags().Lookup(ConfigFlag); f == nil {
		t.Fatalf("--%s persistent flag not registered", ConfigFlag)
	}
}

func TestFileLayer_FileOverridesDefault(t *testing.T) {
	path := writeFile(t, "config.yaml", `
log_level: warn
db:
  host: db.example.com
  port: 6000
`)
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	// File value overrides the annotated default ("info").
	if got := strOf(m, "log_level"); got != "warn" {
		t.Errorf("log_level = %q, want warn (file beats default)", got)
	}
	// Nested block loads from the file too.
	if got := blockStrOf(t, m, "db", "host"); got != "db.example.com" {
		t.Errorf("db.host = %q, want db.example.com (nested file)", got)
	}
	if got := blockIntOf2(t, m, "db", "port"); got != 6000 {
		t.Errorf("db.port = %d, want 6000 (nested file)", got)
	}
}

func TestFileLayer_OmittedFieldKeepsDefault(t *testing.T) {
	// File sets only db.host — log_level and db.port keep their defaults.
	path := writeFile(t, "config.yaml", "db:\n  host: only-host\n")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "info" {
		t.Errorf("log_level = %q, want info (default; file omitted it)", got)
	}
	if got := blockIntOf2(t, m, "db", "port"); got != 5432 {
		t.Errorf("db.port = %d, want 5432 (default; file omitted it)", got)
	}
	if got := blockStrOf(t, m, "db", "host"); got != "only-host" {
		t.Errorf("db.host = %q, want only-host (file)", got)
	}
}

func TestFileLayer_JSONFormat(t *testing.T) {
	path := writeFile(t, "config.json", `{"log_level":"error","db":{"host":"json-host"}}`)
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "error" {
		t.Errorf("log_level = %q, want error (json file)", got)
	}
	if got := blockStrOf(t, m, "db", "host"); got != "json-host" {
		t.Errorf("db.host = %q, want json-host (json file)", got)
	}
}

func TestFileLayer_EnvOverridesFile(t *testing.T) {
	path := writeFile(t, "config.yaml", "log_level: warn\ndb:\n  host: file-host\n")
	t.Setenv("LOG_LEVEL", "debug")  // env beats file
	t.Setenv("DB_HOST", "env-host") // env beats file (nested)
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "debug" {
		t.Errorf("log_level = %q, want debug (env beats file)", got)
	}
	if got := blockStrOf(t, m, "db", "host"); got != "env-host" {
		t.Errorf("db.host = %q, want env-host (env beats file)", got)
	}
}

func TestFileLayer_FlagOverridesEnvOverridesFile_FullChain(t *testing.T) {
	// defaults < file < env < flag, all four exercised on log_level.
	path := writeFile(t, "config.yaml", "log_level: from-file\n")
	t.Setenv("LOG_LEVEL", "from-env")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("log-level", "from-flag"); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "from-flag" {
		t.Errorf("log_level = %q, want from-flag (flag wins the full chain)", got)
	}
}

func TestFileLayer_SensitiveFromFile(t *testing.T) {
	// A sensitive field has NO flag but CAN be set from the file.
	path := writeFile(t, "config.yaml", "api_key: file-secret\ndb:\n  password: file-db-pass\n")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	// No flag exists for sensitive fields.
	if f := cmd.Flags().Lookup("api-key"); f != nil {
		t.Errorf("api_key must have NO flag, got %v", f)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "api_key"); got != "file-secret" {
		t.Errorf("api_key = %q, want file-secret (sensitive from file)", got)
	}
	if got := blockStrOf(t, m, "db", "password"); got != "file-db-pass" {
		t.Errorf("db.password = %q, want file-db-pass (sensitive nested from file)", got)
	}
}

func TestFileLayer_SensitiveEnvOverridesFile(t *testing.T) {
	path := writeFile(t, "config.yaml", "api_key: file-secret\n")
	t.Setenv("API_KEY", "env-secret")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "api_key"); got != "env-secret" {
		t.Errorf("api_key = %q, want env-secret (env beats file for sensitive)", got)
	}
}

func TestFileLayer_MissingExplicitFlagFileErrorsLoudly(t *testing.T) {
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, "/nonexistent/path/config.yaml"); err != nil {
		t.Fatal(err)
	}
	err := LoadInto(cmd, m.Interface())
	if err == nil {
		t.Fatal("LoadInto: want LOUD error for missing explicit --config file, got nil")
	}
	if !contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to mention the file does not exist", err.Error())
	}
}

func TestFileLayer_MissingExplicitEnvFileErrorsLoudly(t *testing.T) {
	t.Setenv(ConfigPathEnv, "/nonexistent/env/config.yaml")
	m := buildFileLayerMessage(t)
	err := LoadInto(nil, m.Interface())
	if err == nil {
		t.Fatalf("LoadInto: want LOUD error for missing %s file, got nil", ConfigPathEnv)
	}
	if !contains(err.Error(), "does not exist") {
		t.Errorf("error = %q, want it to mention the file does not exist", err.Error())
	}
}

func TestFileLayer_NoConfigPathProceedsNormally(t *testing.T) {
	// No --config and no FORGE_CONFIG: NOT a fallback — defaults+env+flags only.
	m := buildFileLayerMessage(t)
	if err := LoadInto(nil, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "info" {
		t.Errorf("log_level = %q, want info (default, no config file)", got)
	}
}

func TestFileLayer_InvalidDocumentErrorsLoudly(t *testing.T) {
	// A document with a key that is not a config field is a loud error
	// (DiscardUnknown=false), never a silent drop.
	path := writeFile(t, "config.yaml", "not_a_real_field: oops\n")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err == nil {
		t.Fatal("LoadInto: want error for unknown config key, got nil")
	}
}

func TestFileLayer_MalformedYAMLErrorsLoudly(t *testing.T) {
	path := writeFile(t, "config.yaml", "log_level: : : not valid yaml\n  bad\n")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err == nil {
		t.Fatal("LoadInto: want error for malformed YAML, got nil")
	}
}

func TestFileLayer_UnsupportedExtensionErrorsLoudly(t *testing.T) {
	path := writeFile(t, "config.toml", "log_level = \"warn\"\n")
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, path); err != nil {
		t.Fatal(err)
	}
	err := LoadInto(cmd, m.Interface())
	if err == nil {
		t.Fatal("LoadInto: want error for unsupported extension, got nil")
	}
	if !contains(err.Error(), "unsupported config file extension") {
		t.Errorf("error = %q, want it to mention unsupported extension", err.Error())
	}
}

func TestFileLayer_FlagPathBeatsEnvPath(t *testing.T) {
	// The --config flag path wins over FORGE_CONFIG.
	flagFile := writeFile(t, "flag.yaml", "log_level: from-flag-file\n")
	envFile := writeFile(t, "env.yaml", "log_level: from-env-file\n")
	t.Setenv(ConfigPathEnv, envFile)
	m := buildFileLayerMessage(t)
	cmd := cmdWithConfigFlag(t, m.Interface())
	if err := cmd.Flags().Set(ConfigFlag, flagFile); err != nil {
		t.Fatal(err)
	}
	if err := LoadInto(cmd, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "from-flag-file" {
		t.Errorf("log_level = %q, want from-flag-file (flag path beats env path)", got)
	}
}

func TestFileLayer_EnvPathLoadsFile(t *testing.T) {
	// FORGE_CONFIG alone (no --config flag, cmd=nil) loads the file.
	envFile := writeFile(t, "env.yaml", "log_level: env-path-loaded\n")
	t.Setenv(ConfigPathEnv, envFile)
	m := buildFileLayerMessage(t)
	if err := LoadInto(nil, m.Interface()); err != nil {
		t.Fatalf("LoadInto: %v", err)
	}
	if got := strOf(m, "log_level"); got != "env-path-loaded" {
		t.Errorf("log_level = %q, want env-path-loaded (FORGE_CONFIG path)", got)
	}
}
