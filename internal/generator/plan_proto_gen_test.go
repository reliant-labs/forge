package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestGeneratePlanProtoFileBasic(t *testing.T) {
	root := t.TempDir()

	rpcs := []config.PlanRPC{
		{
			Name: "SubmitIntakeForm",
			Request: []config.PlanField{
				{Name: "patient_name", Type: "string"},
				{Name: "email", Type: "string"},
				{Name: "phone", Type: "string"},
			},
			Response: []config.PlanField{
				{Name: "patient_id", Type: "string"},
			},
		},
		{
			Name: "GetPatient",
			Request: []config.PlanField{
				{Name: "patient_id", Type: "string"},
			},
			Response: []config.PlanField{
				{Name: "patient", Type: "Patient"},
			},
		},
	}

	if err := GeneratePlanProtoFile(root, "example.com/myapp", "careflow", rpcs); err != nil {
		t.Fatalf("GeneratePlanProtoFile() error = %v", err)
	}

	protoPath := filepath.Join(root, "proto", "services", "careflow", "v1", "careflow.proto")
	content, err := os.ReadFile(protoPath)
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	proto := string(content)

	// Check header
	if !strings.Contains(proto, `syntax = "proto3";`) {
		t.Error("missing proto3 syntax declaration")
	}
	if !strings.Contains(proto, "package services.careflow.v1;") {
		t.Error("missing package declaration")
	}
	if !strings.Contains(proto, `option go_package = "example.com/myapp/gen/services/careflow/v1;careflowv1";`) {
		t.Error("missing go_package option")
	}

	// Check service block
	if !strings.Contains(proto, "service CareflowService {") {
		t.Error("missing service declaration")
	}
	if !strings.Contains(proto, "rpc SubmitIntakeForm(SubmitIntakeFormRequest) returns (SubmitIntakeFormResponse) {}") {
		t.Error("missing SubmitIntakeForm RPC")
	}
	if !strings.Contains(proto, "rpc GetPatient(GetPatientRequest) returns (GetPatientResponse) {}") {
		t.Error("missing GetPatient RPC")
	}

	// Check messages
	if !strings.Contains(proto, "message SubmitIntakeFormRequest {") {
		t.Error("missing SubmitIntakeFormRequest message")
	}
	if !strings.Contains(proto, "string patient_name = 1;") {
		t.Error("missing patient_name field")
	}
	if !strings.Contains(proto, "string email = 2;") {
		t.Error("missing email field")
	}
	if !strings.Contains(proto, "string phone = 3;") {
		t.Error("missing phone field")
	}
	if !strings.Contains(proto, "message SubmitIntakeFormResponse {") {
		t.Error("missing SubmitIntakeFormResponse message")
	}
	if !strings.Contains(proto, "string patient_id = 1;") {
		t.Error("missing patient_id field in response")
	}
	if !strings.Contains(proto, "Patient patient = 1;") {
		t.Error("missing patient field in GetPatientResponse")
	}

	// Should NOT have timestamp import
	if strings.Contains(proto, "timestamp.proto") {
		t.Error("timestamp import should not be present when no timestamp fields exist")
	}
}

func TestGeneratePlanProtoFileTimestampImport(t *testing.T) {
	root := t.TempDir()

	rpcs := []config.PlanRPC{
		{
			Name: "CreateEvent",
			Request: []config.PlanField{
				{Name: "name", Type: "string"},
				{Name: "start_time", Type: "timestamp"},
			},
			Response: []config.PlanField{
				{Name: "id", Type: "string"},
			},
		},
	}

	if err := GeneratePlanProtoFile(root, "example.com/myapp", "events", rpcs); err != nil {
		t.Fatalf("GeneratePlanProtoFile() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "proto", "services", "events", "v1", "events.proto"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	proto := string(content)

	if !strings.Contains(proto, `import "google/protobuf/timestamp.proto";`) {
		t.Error("missing timestamp import")
	}
	if !strings.Contains(proto, "google.protobuf.Timestamp start_time = 2;") {
		t.Error("missing timestamp field mapping")
	}
}

func TestGeneratePlanProtoFileRepeatedField(t *testing.T) {
	root := t.TempDir()

	rpcs := []config.PlanRPC{
		{
			Name: "ListTags",
			Request: []config.PlanField{
				{Name: "resource_id", Type: "string"},
			},
			Response: []config.PlanField{
				{Name: "tags", Type: "repeated string"},
			},
		},
	}

	if err := GeneratePlanProtoFile(root, "example.com/myapp", "tags", rpcs); err != nil {
		t.Fatalf("GeneratePlanProtoFile() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "proto", "services", "tags", "v1", "tags.proto"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	proto := string(content)

	if !strings.Contains(proto, "repeated string tags = 1;") {
		t.Error("missing repeated string field")
	}
}

func TestGeneratePlanProtoFileRPCDescription(t *testing.T) {
	root := t.TempDir()

	rpcs := []config.PlanRPC{
		{
			Name:        "Ping",
			Description: "Health check endpoint",
			Request:     []config.PlanField{},
			Response:    []config.PlanField{},
		},
	}

	if err := GeneratePlanProtoFile(root, "example.com/myapp", "health", rpcs); err != nil {
		t.Fatalf("GeneratePlanProtoFile() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "proto", "services", "health", "v1", "health.proto"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	proto := string(content)

	if !strings.Contains(proto, "// Health check endpoint") {
		t.Error("missing RPC description comment")
	}
}

func TestGeneratePlanProtoFileOverwritesExisting(t *testing.T) {
	root := t.TempDir()

	// Pre-create a proto stub
	protoDir := filepath.Join(root, "proto", "services", "orders", "v1")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "orders.proto"), []byte("// old stub"), 0644); err != nil {
		t.Fatal(err)
	}

	rpcs := []config.PlanRPC{
		{
			Name:    "CreateOrder",
			Request: []config.PlanField{{Name: "item", Type: "string"}},
		},
	}

	if err := GeneratePlanProtoFile(root, "example.com/myapp", "orders", rpcs); err != nil {
		t.Fatalf("GeneratePlanProtoFile() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(protoDir, "orders.proto"))
	if err != nil {
		t.Fatal(err)
	}
	proto := string(content)

	if strings.Contains(proto, "old stub") {
		t.Error("old stub should have been overwritten")
	}
	if !strings.Contains(proto, "rpc CreateOrder") {
		t.Error("new proto should contain CreateOrder RPC")
	}
}
