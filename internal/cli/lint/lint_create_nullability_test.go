// File: internal/cli/lint/lint_create_nullability_test.go
//
// Tests for forgeconv-create-request-nullability. Every case is a real
// .proto on disk run through the real scanner — the check's whole value is
// that it reads what the author actually wrote, so a test that fed it
// synthetic structs would not exercise the part that can be wrong.

package lint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeNullabilityProto lays down one .proto in a temp dir and returns the
// directory to scan.
func writeNullabilityProto(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	protoDir := filepath.Join(dir, "services", "crm", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoDir, "crm.proto"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const nullabilityProtoHeader = `syntax = "proto3";

package services.crm.v1;

`

// TestCreateNullability_TripsOnDroppedOptional is the motivating case: the
// entity declares a nullable FK, the Create request re-declares it without
// the label. This is the exact shape that writes "" into a nullable FK
// column and surfaces as a postgres foreign-key violation.
func TestCreateNullability_TripsOnDroppedOptional(t *testing.T) {
	root := writeNullabilityProto(t, nullabilityProtoHeader+`
// forge:entity
message Customer {
  string name = 1;
  optional string source_lead_id = 2;
  string id = 3;
}

message CreateCustomerRequest {
  string name = 1;
  string source_lead_id = 2;
}
`)
	findings, err := collectCreateNullabilityFindings(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Field != "source_lead_id" || f.Entity != "Customer" {
		t.Errorf("wrong field flagged: %+v", f)
	}
	if !f.EntityOptional || f.CreateOptional {
		t.Errorf("directions recorded backwards: %+v", f)
	}
	// The finding must point at the CREATE request's declaration (line
	// 15), not the entity's (line 9) — the create side is the one to
	// edit, and a finding that pointed at the entity would send the
	// author to delete the label they meant to keep.
	if f.Line != 15 {
		t.Errorf("want the Create request line 15, got %d", f.Line)
	}
	if !strings.Contains(createNullabilityFixHint(f), "Add `optional`") {
		t.Errorf("fix hint should say what to add: %s", createNullabilityFixHint(f))
	}
}

// TestCreateNullability_TripsOnAddedOptional pins the inverse direction:
// the entity says always-present, the Create request lets it be omitted.
func TestCreateNullability_TripsOnAddedOptional(t *testing.T) {
	root := writeNullabilityProto(t, nullabilityProtoHeader+`
// forge:entity
message Customer {
  string name = 1;
  string id = 2;
}

message CreateCustomerRequest {
  optional string name = 1;
}
`)
	findings, err := collectCreateNullabilityFindings(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("want exactly 1 finding, got %d: %+v", len(findings), findings)
	}
	if findings[0].EntityOptional || !findings[0].CreateOptional {
		t.Errorf("directions recorded backwards: %+v", findings[0])
	}
	if !strings.Contains(createNullabilityFixHint(findings[0]), "Drop `optional`") {
		t.Errorf("fix hint should say what to drop: %s", createNullabilityFixHint(findings[0]))
	}
}

// TestCreateNullability_NoFalsePositives is the guard that matters most: a
// rule that fires on correct protos gets disabled, and then it protects
// nothing. Every construct here is legitimate and must produce ZERO
// findings.
func TestCreateNullability_NoFalsePositives(t *testing.T) {
	root := writeNullabilityProto(t, nullabilityProtoHeader+`
// forge:entity
message Customer {
  // Agreeing labels, both directions.
  string name = 1;
  optional string source_lead_id = 2;

  // repeated is not optional and never comparable to it — proto3
  // forbids `+"`optional repeated`"+`, so these agree with themselves.
  repeated string tags = 3;

  // Read-only and computed fields are ABSENT from Create by design.
  // Absent must never read as disagreement.
  int64 lifetime_value_cents = 4; // forge:read-only
  int64 score = 5; // forge:computed

  // A field the author deliberately keeps off the write envelope.
  string internal_ref = 6;

  // Managed lifecycle fields: server-owned, never on a write envelope,
  // and here declared WITHOUT optional while the create omits them.
  string id = 7;
}

message CreateCustomerRequest {
  string name = 1;
  optional string source_lead_id = 2;
  repeated string tags = 3;
}

// A message with no Create request at all — a filter, not an entity.
message CustomerFilter {
  optional string name = 1;
  string page_token = 2;
}

// An envelope that flattens nothing.
message GetCustomerRequest {
  string id = 1;
}
`)
	findings, err := collectCreateNullabilityFindings(root)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("rule fired on a correct proto — a check that cries wolf gets disabled:\n%+v", findings)
	}
}

// TestCreateNullability_MissingProtoDirIsClean pins that CLI and library
// projects, which have no proto tree at all, lint clean rather than erroring.
func TestCreateNullability_MissingProtoDirIsClean(t *testing.T) {
	findings, err := collectCreateNullabilityFindings(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing proto dir must not error: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("want no findings, got %+v", findings)
	}
}

// TestCreateNullability_ReportFormatting pins the two report shapes: the
// clean line, and a finding rendered with its rule id so the output is
// greppable and the rule is attributable.
func TestCreateNullability_ReportFormatting(t *testing.T) {
	var clean strings.Builder
	formatCreateNullability(&clean, nil)
	if !strings.Contains(clean.String(), "create-nullability clean") {
		t.Errorf("clean report missing: %q", clean.String())
	}

	var dirty strings.Builder
	formatCreateNullability(&dirty, []createNullabilityFinding{{
		File: "proto/services/crm/v1/crm.proto", Line: 10,
		Entity: "Customer", Field: "source_lead_id", EntityOptional: true,
	}})
	out := dirty.String()
	for _, want := range []string{
		"forgeconv-create-request-nullability",
		"proto/services/crm/v1/crm.proto:10",
		"source_lead_id",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
}
