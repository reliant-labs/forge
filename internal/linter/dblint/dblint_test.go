package dblint

import (
	"testing"

	"github.com/reliant-labs/forge/internal/database"
)

func TestCheckMissingPK(t *testing.T) {
	tests := []struct {
		name     string
		entity   database.ProtoEntity
		wantHits int
	}{
		{
			name: "has explicit PK",
			entity: database.ProtoEntity{
				MessageName: "User",
				TableName:   "users",
				Fields: []database.ProtoEntityField{
					{Name: "user_id", IsPrimary: true},
					{Name: "email"},
				},
			},
			wantHits: 0,
		},
		{
			name: "has id field",
			entity: database.ProtoEntity{
				MessageName: "User",
				TableName:   "users",
				Fields: []database.ProtoEntityField{
					{Name: "id"},
					{Name: "email"},
				},
			},
			wantHits: 0,
		},
		{
			name: "no PK at all",
			entity: database.ProtoEntity{
				MessageName: "Metric",
				TableName:   "metrics",
				Fields: []database.ProtoEntityField{
					{Name: "name"},
					{Name: "value"},
				},
			},
			wantHits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := checkMissingPK(tt.entity)
			if len(findings) != tt.wantHits {
				t.Errorf("checkMissingPK() returned %d findings, want %d", len(findings), tt.wantHits)
			}
			for _, f := range findings {
				if f.Rule != "missing-pk" {
					t.Errorf("unexpected rule %q", f.Rule)
				}
			}
		})
	}
}

func TestCheckMissingFKAnnotation(t *testing.T) {
	entity := database.ProtoEntity{
		MessageName: "Order",
		TableName:   "orders",
		Fields: []database.ProtoEntityField{
			{Name: "id"},
			{Name: "user_id"},
			{Name: "product_id"},
			{Name: "quantity"},
		},
	}

	findings := checkMissingFKAnnotation(entity)
	if len(findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(findings))
	}
	if findings[0].Field != "user_id" {
		t.Errorf("expected field user_id, got %s", findings[0].Field)
	}
	if findings[1].Field != "product_id" {
		t.Errorf("expected field product_id, got %s", findings[1].Field)
	}
}

func TestCheckMissingTimestamps(t *testing.T) {
	tests := []struct {
		name     string
		fields   []database.ProtoEntityField
		wantHits int
	}{
		{
			name: "has both timestamps",
			fields: []database.ProtoEntityField{
				{Name: "id"},
				{Name: "created_at"},
				{Name: "updated_at"},
			},
			wantHits: 0,
		},
		{
			name: "missing updated_at",
			fields: []database.ProtoEntityField{
				{Name: "id"},
				{Name: "created_at"},
			},
			wantHits: 1,
		},
		{
			name: "missing both",
			fields: []database.ProtoEntityField{
				{Name: "id"},
				{Name: "name"},
			},
			wantHits: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ent := database.ProtoEntity{
				MessageName: "Test",
				TableName:   "tests",
				Fields:      tt.fields,
			}
			findings := checkMissingTimestamps(ent)
			if len(findings) != tt.wantHits {
				t.Errorf("checkMissingTimestamps() returned %d findings, want %d", len(findings), tt.wantHits)
			}
		})
	}
}

func TestCheckMissingSoftDelete(t *testing.T) {
	withDeletedAt := database.ProtoEntity{
		MessageName: "User",
		Fields:      []database.ProtoEntityField{{Name: "deleted_at"}},
	}
	if findings := checkMissingSoftDelete(withDeletedAt); len(findings) != 0 {
		t.Errorf("expected 0 findings for entity with deleted_at, got %d", len(findings))
	}

	withoutDeletedAt := database.ProtoEntity{
		MessageName: "User",
		Fields:      []database.ProtoEntityField{{Name: "id"}},
	}
	if findings := checkMissingSoftDelete(withoutDeletedAt); len(findings) != 1 {
		t.Errorf("expected 1 finding for entity without deleted_at, got %d", len(findings))
	}
}

func TestCheckFlatStructure(t *testing.T) {
	entity := database.ProtoEntity{
		MessageName: "User",
		Fields: []database.ProtoEntityField{
			{Name: "id"},
			{Name: "address_line1"},
			{Name: "address_line2"},
			{Name: "address_city"},
			{Name: "address_zip"},
		},
	}

	findings := checkFlatStructure(entity)
	if len(findings) != 1 {
		t.Fatalf("expected 1 flat-structure finding, got %d", len(findings))
	}
	if findings[0].Rule != "flat-structure" {
		t.Errorf("unexpected rule %q", findings[0].Rule)
	}
}

func TestCheckFlatStructureSkipsCommonPrefixes(t *testing.T) {
	entity := database.ProtoEntity{
		MessageName: "Audit",
		Fields: []database.ProtoEntityField{
			{Name: "created_at"},
			{Name: "created_by"},
			{Name: "created_reason"},
		},
	}

	findings := checkFlatStructure(entity)
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for common prefixes, got %d", len(findings))
	}
}

func TestLintEntities(t *testing.T) {
	entities := []database.ProtoEntity{
		{
			MessageName: "Patient",
			TableName:   "patients",
			Fields: []database.ProtoEntityField{
				{Name: "id", IsPrimary: true},
				{Name: "name"},
				{Name: "email"},
				{Name: "doctor_id"},
				{Name: "created_at"},
				{Name: "updated_at"},
				{Name: "deleted_at"},
			},
		},
	}

	result := LintEntities(entities)
	// Should have: missing-fk-annotation for doctor_id, missing-fk-index for doctor_id
	hasFK := false
	hasFKIndex := false
	for _, f := range result.Findings {
		if f.Rule == "missing-fk-annotation" && f.Field == "doctor_id" {
			hasFK = true
		}
		if f.Rule == "missing-fk-index" && f.Field == "doctor_id" {
			hasFKIndex = true
		}
	}
	if !hasFK {
		t.Error("expected missing-fk-annotation finding for doctor_id")
	}
	if !hasFKIndex {
		t.Error("expected missing-fk-index finding for doctor_id")
	}
}

func TestPluralize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"user", "users"},
		{"category", "categories"},
		{"bus", "buses"},
		{"key", "keys"},
		{"box", "boxes"},
		{"patient", "patients"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := pluralize(tt.input)
			if got != tt.want {
				t.Errorf("pluralize(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
