package codegen

import (
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// EntityDefToPlanEntity converts an EntityDef (from proto parsing) to a
// PlanEntity (used by ORM generation).
//
// Field semantics (primary key, tenant key, foreign key) are sourced from
// the EntityDef itself, which was already populated from explicit
// (forge.v1.entity) / (forge.v1.field) annotations during proto parsing.
// No name-based heuristics are applied here — `entity.PkField` is the
// only thing that determines which field is marked PrimaryKey, regardless
// of whether it happens to be named "id" or something else.
func EntityDefToPlanEntity(entity EntityDef) config.PlanEntity {
	pe := config.PlanEntity{
		Name:       entity.Name,
		TableName:  entity.TableName,
		SoftDelete: entity.SoftDelete,
		Timestamps: entity.Timestamps,
		Fields:     make([]config.PlanEntityField, 0, len(entity.Fields)),
	}

	for _, f := range entity.Fields {
		// Message-typed fields carry their real type name in MessageType
		// (ProtoType is the unmappable literal "message"). Using it here is
		// what lets google.protobuf.Timestamp columns map to TIMESTAMPTZ /
		// *timestamppb.Timestamp instead of degrading to TEXT/string.
		// The "repeated " prefix (set by the descriptor for list fields)
		// is preserved around the substitution so the ORM/migration
		// generators see e.g. "repeated string" (stored as JSON) or
		// "repeated google.protobuf.Timestamp" (rejected loudly with the
		// join-table workaround named).
		base, repeated := strings.CutPrefix(f.ProtoType, "repeated ")
		if base == "message" && f.MessageType != "" {
			base = f.MessageType
		}
		fieldType := base
		if repeated {
			fieldType = "repeated " + base
		}
		pf := config.PlanEntityField{
			Name: f.Name,
			Type: fieldType,
		}

		if entity.PkField != "" && f.Name == entity.PkField {
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
