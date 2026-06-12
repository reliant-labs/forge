package codegen

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/schemadef"
)

// BuildSchemaEntities is the entity source of truth: it joins the
// APPLIED schema (db/migrations shadow-applied and introspected) with
// the service protos' CRUD method shapes.
//
// An entity exists when BOTH halves exist:
//
//   - a service declares CRUD RPCs for it (Create<X>/Get<X>/List<Xs>/...),
//     giving the wire message shape, and
//   - the applied schema has the matching table (pluralized snake_case
//     of the entity name), giving columns/PK/conventions.
//
// CRUD RPCs without a table generate nothing (the honest-routes
// contract: no pages, no ORM, no nav for entities that don't exist).
// Tables without CRUD RPCs are plain schema — owned by hand-written
// code, invisible to the CRUD/frontend projections.
func BuildSchemaEntities(projectDir string, services []ServiceDef) ([]EntityDef, error) {
	tables, err := schemadef.ApplyAndIntrospect(filepath.Join(projectDir, "db", "migrations"))
	if err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, nil
	}
	tableByName := make(map[string]schemadef.Table, len(tables))
	for _, t := range tables {
		tableByName[t.Name] = t
	}

	seen := map[string]bool{}
	var entities []EntityDef
	for _, svc := range services {
		for _, m := range svc.Methods {
			if m.ClientStreaming || m.ServerStreaming {
				continue
			}
			op, name := parseCRUDOperation(m.Name)
			if op == "" {
				continue
			}
			if op == "list" {
				name = inflection.Singular(name)
			}
			key := strings.ToLower(name)
			if seen[key] {
				continue
			}
			table, ok := tableByName[naming.Pluralize(naming.ToSnakeCase(name))]
			if !ok {
				continue
			}
			seen[key] = true
			entities = append(entities, buildEntityDef(name, table, svc))
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Name < entities[j].Name })
	return entities, nil
}

func buildEntityDef(name string, table schemadef.Table, svc ServiceDef) EntityDef {
	conv := schemadef.DetectConventions(table)

	e := EntityDef{
		Name:          name,
		TableName:     table.Name,
		ProtoFile:     svc.ProtoFile,
		SoftDelete:    conv.SoftDelete,
		Timestamps:    conv.Timestamps,
		SearchColumns: conv.SearchColumns,
	}

	// Columns: the applied schema, verbatim.
	for _, c := range table.Columns {
		e.Columns = append(e.Columns, EntityColumn{
			Name:     c.Name,
			Type:     string(c.Type),
			IsArray:  c.IsArray,
			NotNull:  c.NotNull,
			IsPK:     c.IsPK,
			DeclType: c.DeclType,
			Default:  c.Default,
		})
	}

	// PK: single-column keys only — composite PKs fall back to "id"
	// for the CRUD projection (Get/Delete take one id).
	e.PkField = "id"
	e.PkGoType = "string"
	if len(table.PKCols) == 1 {
		e.PkField = table.PKCols[0]
		for _, c := range table.Columns {
			if c.Name == e.PkField {
				e.PkGoType = canonicalGoType(string(c.Type), c.IsArray)
			}
		}
	}

	if conv.HasTenant {
		e.HasTenant = true
		e.TenantColumnName = conv.TenantColumn
		e.TenantFieldName = conv.TenantColumn
		e.TenantGoName = naming.ToProtoPascalCase(conv.TenantColumn)
	}

	// Wire fields from the service proto's entity message.
	e.Fields = wireEntityFields(svc, name)
	return e
}

// canonicalGoType maps a canonical schema type to the Go type used in
// generated entity structs (NOT NULL variant; nullable adds a pointer).
func canonicalGoType(canonical string, isArray bool) string {
	var base string
	switch canonical {
	case "int64":
		base = "int64"
	case "float64":
		base = "float64"
	case "bool":
		base = "bool"
	case "time":
		base = "time.Time"
	case "bytes":
		base = "[]byte"
	default: // string, json, unknown
		base = "string"
	}
	if isArray {
		return "[]" + base
	}
	return base
}

// wireEntityFields extracts the entity wire-message fields from the
// service descriptor: the deep Schemas map when present, else the
// shallow Messages map (older descriptors).
func wireEntityFields(svc ServiceDef, entityName string) []EntityField {
	if defs, ok := svc.Schemas[svc.Package+"."+entityName]; ok {
		fields := make([]EntityField, 0, len(defs))
		for _, d := range defs {
			fields = append(fields, schemaFieldToEntityField(d))
		}
		return fields
	}
	defs, ok := svc.Messages[entityName]
	if !ok {
		return nil
	}
	fields := make([]EntityField, 0, len(defs))
	for _, d := range defs {
		fields = append(fields, messageFieldToEntityField(d))
	}
	return fields
}

func schemaFieldToEntityField(d SchemaFieldDef) EntityField {
	f := EntityField{
		Name:   d.Name,
		GoName: naming.ToProtoPascalCase(d.Name),
	}
	switch d.Kind {
	case "message":
		f.ProtoType = "message"
		f.MessageType = d.TypeName
		f.GoType = wellKnownGoType(d.TypeName)
		if d.Repeated {
			f.GoType = "[]*" + shortName(d.TypeName)
		}
	case "enum":
		f.ProtoType = "enum"
		f.MessageType = d.TypeName
		f.GoType = shortName(d.TypeName)
	case "map":
		f.ProtoType = "message"
		f.GoType = "map[string]string" // marker; maps need custom handling
	default: // scalar
		f.ProtoType = d.Kind
		f.GoType = ProtoTypeToGoType(d.Kind)
		if d.Repeated {
			f.GoType = "[]" + f.GoType
		}
	}
	f.Kind = DetermineFieldKind(f.ProtoType, f.GoType)
	if d.Kind == "map" {
		f.Kind = FieldKindMap
	}
	return f
}

func messageFieldToEntityField(d MessageFieldDef) EntityField {
	f := EntityField{
		Name:      d.Name,
		GoName:    naming.ToProtoPascalCase(d.Name),
		ProtoType: d.ProtoType,
	}
	if d.ProtoType == "message" {
		f.MessageType = d.MessageType
		f.GoType = wellKnownGoType(d.MessageType)
	} else {
		f.GoType = ProtoTypeToGoType(d.ProtoType)
	}
	f.Kind = DetermineFieldKind(f.ProtoType, f.GoType)
	return f
}

// wellKnownGoType maps well-known proto message types to their Go
// representation; other messages map to a pointer to their short name.
func wellKnownGoType(typeName string) string {
	switch typeName {
	case "google.protobuf.Timestamp":
		return "*timestamppb.Timestamp"
	case "google.protobuf.StringValue":
		return "*string"
	case "google.protobuf.Int32Value":
		return "*int32"
	case "google.protobuf.Int64Value":
		return "*int64"
	case "google.protobuf.UInt32Value":
		return "*uint32"
	case "google.protobuf.UInt64Value":
		return "*uint64"
	case "google.protobuf.BoolValue":
		return "*bool"
	case "google.protobuf.FloatValue":
		return "*float32"
	case "google.protobuf.DoubleValue":
		return "*float64"
	}
	return "*" + shortName(typeName)
}

func shortName(fq string) string {
	if i := strings.LastIndexByte(fq, '.'); i >= 0 {
		return fq[i+1:]
	}
	return fq
}
