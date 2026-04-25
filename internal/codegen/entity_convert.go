package codegen

import (
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// EntityDefToPlanEntity converts an EntityDef (from proto parsing) to a
// PlanEntity (used by ORM generation).
func EntityDefToPlanEntity(entity EntityDef) config.PlanEntity {
	pe := config.PlanEntity{
		Name:      entity.Name,
		TableName: entity.TableName,
		Fields:    make([]config.PlanEntityField, 0, len(entity.Fields)),
	}

	for _, f := range entity.Fields {
		pf := config.PlanEntityField{
			Name: f.Name,
			Type: f.ProtoType,
		}

		if f.Name == "id" {
			pf.PrimaryKey = true
			pf.NotNull = true
		}

		if entity.HasTenant && f.Name == entity.TenantFieldName {
			pf.TenantKey = true
			pf.NotNull = true
		}

		if f.IsFK {
			pf.References = f.FKTable + ".id"
		}

		pe.Fields = append(pe.Fields, pf)
	}

	return pe
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
