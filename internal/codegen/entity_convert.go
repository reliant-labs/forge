package codegen

import (
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// EntityDefToPlanEntity converts an EntityDef to the PlanEntity shape
// the ORM generator consumes.
//
// The field source is the entity's COLUMNS — the introspected applied
// schema — never the wire message. A column added by a hand-written
// migration appears here (and on the generated struct) without any
// proto involvement; a wire-only field never reaches the database
// layer. SQL is the schema truth.
func EntityDefToPlanEntity(entity EntityDef) config.PlanEntity {
	pe := config.PlanEntity{
		Name:       entity.Name,
		TableName:  entity.TableName,
		SoftDelete: entity.SoftDelete,
		Timestamps: entity.Timestamps,
		Fields:     make([]config.PlanEntityField, 0, len(entity.Columns)),
	}

	// A `// forge:secret` marker lives on the WIRE field, not the column
	// (the column is schema truth and always stores the value). Match wire
	// secret fields to their column by name so the ORM's Spec.SecretColumns
	// can preserve them on a maskless full-replace Update.
	secretCols := make(map[string]bool, len(entity.Fields))
	for _, f := range entity.Fields {
		if f.Secret {
			secretCols[f.Name] = true
		}
	}

	for _, c := range entity.Columns {
		pf := config.PlanEntityField{
			Name:       c.Name,
			Type:       planTypeForColumn(c),
			PrimaryKey: c.IsPK,
			NotNull:    c.NotNull,
			Default:    c.Default,
			Generated:  c.IsGenerated,
			Secret:     secretCols[c.Name],
		}
		pe.Fields = append(pe.Fields, pf)
	}

	return pe
}

// planTypeForColumn maps an introspected column to the plan type
// vocabulary: the canonical schema type verbatim (schemadef.MapDeclaredType
// produces the closed set), with a "[]" prefix for an array column.
//
// It used to answer "[]string" for every array column that was not
// BIGINT[], so a BOOLEAN[] / DOUBLE PRECISION[] / BYTEA[] / TIMESTAMPTZ[]
// column reached the ORM generator claiming to be text. Passing the type
// through instead of re-deciding it is what makes the ORM struct field and
// the conversion generator's pairing the same projection (see
// CanonicalGoTypeOK) rather than two opinions that happen to agree.
func planTypeForColumn(c EntityColumn) string {
	if c.IsArray {
		return "[]" + c.Type
	}
	return c.Type
}

// EntityDefsToPlanEntities converts a slice of EntityDef to a slice of PlanEntity.
func EntityDefsToPlanEntities(entities []EntityDef) []config.PlanEntity {
	result := make([]config.PlanEntity, len(entities))
	for i, e := range entities {
		result[i] = EntityDefToPlanEntity(e)
	}
	return result
}

// ServiceNameFromProtoFile extracts the service name (snake_case) from an
// entity's proto file path. For example, "proto/services/patients/v1/patients.proto"
// returns "patients".
func ServiceNameFromProtoFile(protoFile string) string {
	// Normalise separators.
	p := filepath.ToSlash(protoFile)
	parts := strings.Split(p, "/")
	// Look for the segment after "services/".
	for i, part := range parts {
		if part == "services" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}
