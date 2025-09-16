package codegen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseConfigProto_BasicFields(t *testing.T) {
	tempDir := t.TempDir()
	protoPath := filepath.Join(tempDir, "config.proto")
	protoContents := `syntax = "proto3";

package config.v1;

option go_package = "example.com/test/gen/config/v1;configv1";

import "forge/options/v1/config.proto";

message AppConfig {
  int32 port = 1 [(forge.options.v1.config_field) = {
    env_var: "PORT",
    flag: "port",
    default_value: "8080",
    description: "HTTP server port"
  }];

  string log_level = 2 [(forge.options.v1.config_field) = {
    env_var: "LOG_LEVEL",
    flag: "log-level",
    default_value: "info",
    description: "Log level (debug, info, warn, error)"
  }];

  string database_url = 3 [(forge.options.v1.config_field) = {
    env_var: "DATABASE_URL",
    flag: "database-url",
    required: true,
    description: "PostgreSQL connection string"
  }];
}
`
	if err := os.WriteFile(protoPath, []byte(protoContents), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	messages, err := ParseConfigProto(protoPath)
	if err != nil {
		t.Fatalf("ParseConfigProto() error = %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}

	msg := messages[0]
	if msg.Name != "AppConfig" {
		t.Errorf("expected message name AppConfig, got %q", msg.Name)
	}

	if len(msg.Fields) != 3 {
		t.Fatalf("expected 3 fields, got %d", len(msg.Fields))
	}

	// Verify port field
	port := msg.Fields[0]
	if port.Name != "port" {
		t.Errorf("expected field name 'port', got %q", port.Name)
	}
	if port.GoName != "Port" {
		t.Errorf("expected GoName 'Port', got %q", port.GoName)
	}
	if port.GoType != "int32" {
		t.Errorf("expected GoType 'int32', got %q", port.GoType)
	}
	if port.EnvVar != "PORT" {
		t.Errorf("expected EnvVar 'PORT', got %q", port.EnvVar)
	}
	if port.Flag != "port" {
		t.Errorf("expected Flag 'port', got %q", port.Flag)
	}
	if port.DefaultValue != "8080" {
		t.Errorf("expected DefaultValue '8080', got %q", port.DefaultValue)
	}
	if port.Description != "HTTP server port" {
		t.Errorf("expected Description 'HTTP server port', got %q", port.Description)
	}
	if port.Required {
		t.Error("expected port.Required to be false")
	}

	// Verify log_level field
	logLevel := msg.Fields[1]
	if logLevel.Name != "log_level" {
		t.Errorf("expected field name 'log_level', got %q", logLevel.Name)
	}
	if logLevel.GoName != "LogLevel" {
		t.Errorf("expected GoName 'LogLevel', got %q", logLevel.GoName)
	}
	if logLevel.GoType != "string" {
		t.Errorf("expected GoType 'string', got %q", logLevel.GoType)
	}
	if logLevel.DefaultValue != "info" {
		t.Errorf("expected DefaultValue 'info', got %q", logLevel.DefaultValue)
	}

	// Verify database_url field
	dbURL := msg.Fields[2]
	if dbURL.Name != "database_url" {
		t.Errorf("expected field name 'database_url', got %q", dbURL.Name)
	}
	if dbURL.GoName != "DatabaseURL" {
		t.Errorf("expected GoName 'DatabaseURL', got %q", dbURL.GoName)
	}
	if !dbURL.Required {
		t.Error("expected database_url.Required to be true")
	}
	if dbURL.DefaultValue != "" {
		t.Errorf("expected empty DefaultValue, got %q", dbURL.DefaultValue)
	}
}

func TestParseConfigProto_BoolField(t *testing.T) {
	tempDir := t.TempDir()
	protoPath := filepath.Join(tempDir, "config.proto")
	protoContents := `syntax = "proto3";
package config.v1;
option go_package = "example.com/test/gen/config/v1;configv1";

import "forge/options/v1/config.proto";

message AppConfig {
  bool debug = 1 [(forge.options.v1.config_field) = {
    env_var: "DEBUG",
    flag: "debug",
    default_value: "false",
    description: "Enable debug mode"
  }];
}
`
	if err := os.WriteFile(protoPath, []byte(protoContents), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	messages, err := ParseConfigProto(protoPath)
	if err != nil {
		t.Fatalf("ParseConfigProto() error = %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if len(messages[0].Fields) != 1 {
		t.Fatalf("expected 1 field, got %d", len(messages[0].Fields))
	}

	debug := messages[0].Fields[0]
	if debug.GoType != "bool" {
		t.Errorf("expected GoType 'bool', got %q", debug.GoType)
	}
	if debug.DefaultValue != "false" {
		t.Errorf("expected DefaultValue 'false', got %q", debug.DefaultValue)
	}
}

func TestParseConfigProto_SkipsFieldsWithoutAnnotation(t *testing.T) {
	tempDir := t.TempDir()
	protoPath := filepath.Join(tempDir, "config.proto")
	protoContents := `syntax = "proto3";
package config.v1;
option go_package = "example.com/test/gen/config/v1;configv1";

message AppConfig {
  int32 port = 1 [(forge.options.v1.config_field) = {
    env_var: "PORT",
    flag: "port",
    default_value: "8080",
    description: "HTTP server port"
  }];

  string internal_name = 2;
}
`
	if err := os.WriteFile(protoPath, []byte(protoContents), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	messages, err := ParseConfigProto(protoPath)
	if err != nil {
		t.Fatalf("ParseConfigProto() error = %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if len(messages[0].Fields) != 1 {
		t.Fatalf("expected 1 annotated field, got %d", len(messages[0].Fields))
	}
	if messages[0].Fields[0].Name != "port" {
		t.Errorf("expected field 'port', got %q", messages[0].Fields[0].Name)
	}
}

func TestParseConfigProto_EmptyMessage(t *testing.T) {
	tempDir := t.TempDir()
	protoPath := filepath.Join(tempDir, "config.proto")
	protoContents := `syntax = "proto3";
package config.v1;
option go_package = "example.com/test/gen/config/v1;configv1";

message EmptyConfig {
  string name = 1;
}
`
	if err := os.WriteFile(protoPath, []byte(protoContents), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	messages, err := ParseConfigProto(protoPath)
	if err != nil {
		t.Fatalf("ParseConfigProto() error = %v", err)
	}

	// No messages should be returned if none have config_field annotations
	if len(messages) != 0 {
		t.Errorf("expected 0 messages (no config_field annotations), got %d", len(messages))
	}
}

func TestParseConfigProtosFromDir(t *testing.T) {
	tempDir := t.TempDir()
	v1Dir := filepath.Join(tempDir, "config", "v1")
	if err := os.MkdirAll(v1Dir, 0o755); err != nil {
		t.Fatal(err)
	}

	protoContents := `syntax = "proto3";
package config.v1;
option go_package = "example.com/test/gen/config/v1;configv1";

import "forge/options/v1/config.proto";

message AppConfig {
  int32 port = 1 [(forge.options.v1.config_field) = {
    env_var: "PORT",
    flag: "port",
    default_value: "8080",
    description: "HTTP server port"
  }];
}
`
	protoPath := filepath.Join(v1Dir, "config.proto")
	if err := os.WriteFile(protoPath, []byte(protoContents), 0o644); err != nil {
		t.Fatal(err)
	}

	messages, err := ParseConfigProtosFromDir(filepath.Join(tempDir, "config"))
	if err != nil {
		t.Fatalf("ParseConfigProtosFromDir() error = %v", err)
	}

	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].Name != "AppConfig" {
		t.Errorf("expected message name AppConfig, got %q", messages[0].Name)
	}
}

func TestProtoFieldToGoName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"port", "Port"},
		{"database_url", "DatabaseURL"},
		{"log_level", "LogLevel"},
		{"some_long_name", "SomeLongName"},
	}

	for _, tt := range tests {
		got := protoFieldToGoName(tt.input)
		if got != tt.want {
			t.Errorf("protoFieldToGoName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestProtoTypeToGoType(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"string", "string"},
		{"int32", "int32"},
		{"int64", "int64"},
		{"bool", "bool"},
		{"float", "float32"},
		{"double", "float64"},
		{"unknown", "string"},
	}

	for _, tt := range tests {
		got := protoTypeToGoType(tt.input)
		if got != tt.want {
			t.Errorf("protoTypeToGoType(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}