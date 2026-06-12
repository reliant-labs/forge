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

	for _, c := range entity.Columns {
		pf := config.PlanEntityField{
			Name:       c.Name,
			Type:       planTypeForColumn(c),
			PrimaryKey: c.IsPK,
			NotNull:    c.NotNull,
			Default:    c.Default,
		}
		if entity.HasTenant && c.Name == entity.TenantColumnName {
			pf.TenantKey = true
		}
		pe.Fields = append(pe.Fields, pf)
	}

	return pe
}

// planTypeForColumn maps an introspected column to the plan type
// vocabulary (canonical schema types; see planFieldGoType in
// internal/generator).
func planTypeForColumn(c EntityColumn) string {
	if c.IsArray {
		if c.Type == "int64" {
			return "[]int64"
		}
		return "[]string"
	}
	switch c.Type {
	case "int64", "float64", "bool", "time", "json", "bytes":
		return c.Type
	default:
		return "string"
	}
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
