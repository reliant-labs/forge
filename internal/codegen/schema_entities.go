package codegen

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/shadowdb"
	"github.com/reliant-labs/forge/pkg/schemadef"
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
	tables, err := schemadef.ApplyAndIntrospectAt(
		filepath.Join(projectDir, "db", "migrations"), shadowdb.Resolve(projectDir))
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
			// A composite PK has no single column the CRUD projection's
			// Get/Update/Delete (which take ONE id) can key on. Fabricating
			// "id" here used to silently name a column that does not exist
			// on the table, producing broken generated code with no signal
			// to the user. Skip the entity entirely instead — its RPCs fall
			// through to the ordinary stub path, exactly as if no matching
			// table existed at all — and say so once, by name, so the user
			// knows to reach it via the repo_ext seam rather than wonder why
			// CRUD never showed up.
			if len(table.PKCols) > 1 {
				fmt.Printf("ℹ️  %s has a composite primary key (%s) — CRUD projection and ULID id-generation are skipped; the table is plain schema (query it via the repo_ext seam).\n",
					table.Name, strings.Join(table.PKCols, ", "))
				continue
			}
			entities = append(entities, buildEntityDef(name, table, svc))
		}
	}
	sort.Slice(entities, func(i, j int) bool { return entities[i].Name < entities[j].Name })
	return entities, nil
}

func buildEntityDef(name string, table schemadef.Table, svc ServiceDef) EntityDef {
	conv := schemadef.DetectConventions(table)

	// ProtoFile must name the file that DECLARES the entity wire message,
	// not necessarily the service's own proto file. When protos are split
	// (domain entity messages in a shared file, CRUD services in per-service
	// files) the two differ, and downstream codegen (mock data, CRUD pages)
	// imports the entity's `*Schema` / type from this file's generated _pb
	// module. SchemaFiles carries that provenance, keyed by FQ message name;
	// fall back to the service file for older descriptors / same-file cases.
	protoFile := svc.ProtoFile
	if svc.SchemaFiles != nil {
		if f, ok := svc.SchemaFiles[svc.Package+"."+name]; ok && f != "" {
			protoFile = f
		}
	}

	e := EntityDef{
		Name:          name,
		TableName:     table.Name,
		ProtoFile:     protoFile,
		SoftDelete:    conv.SoftDelete,
		Timestamps:    conv.Timestamps,
		SearchColumns: conv.SearchColumns,
	}

	// Columns: the applied schema, verbatim.
	for _, c := range table.Columns {
		fillStrategy, _ := c.FillStrategy()
		e.Columns = append(e.Columns, EntityColumn{
			Name:         c.Name,
			Type:         string(c.Type),
			IsArray:      c.IsArray,
			NotNull:      c.NotNull,
			IsPK:         c.IsPK,
			DeclType:     c.DeclType,
			Default:      c.Default,
			IsGenerated:  c.IsGenerated,
			Immutable:    c.HasMarker(schemadef.ColumnMarkerImmutable),
			Version:      c.HasMarker(schemadef.ColumnMarkerVersion),
			FillStrategy: fillStrategy,
		})
	}

	// PK: single-column keys only. BuildSchemaEntities filters out
	// composite-PK tables before calling this function (see there), so
	// table.PKCols is always length 0 or 1 here. The "id" fallback below
	// covers only the length-0 case — a table with no declared PK at all.
	e.PkField = "id"
	e.PkGoType = "string"
	if len(table.PKCols) == 1 {
		e.PkField = table.PKCols[0]
		for _, c := range table.Columns {
			if c.Name == e.PkField {
				e.PkGoType = canonicalGoTypeMust(string(c.Type), c.IsArray)
			}
		}
	}

	// Wire fields from the service proto's entity message.
	e.Fields = WireEntityFields(svc, name)

	// forge:secret — a secret column is stripped from read responses
	// (crud_convert's toProto skip); it must also stay OUT of the generated
	// list `search` filter, or an ILIKE over it becomes an oracle for probing
	// the secret value. Drop any secret field's column from SearchColumns.
	e.SearchColumns = dropSecretSearchColumns(e.SearchColumns, e.Fields)
	return e
}

// dropSecretSearchColumns removes columns backed by a `// forge:secret` wire
// field from the list-search span. Search columns are text columns by
// convention (schemadef), which cannot see the proto-level marker — this is
// the join point where the marker prunes the search set.
func dropSecretSearchColumns(cols []string, fields []EntityField) []string {
	secret := map[string]bool{}
	for _, f := range fields {
		if f.Secret {
			secret[f.Name] = true
		}
	}
	if len(secret) == 0 {
		return cols
	}
	out := cols[:0:0]
	for _, c := range cols {
		if !secret[c] {
			out = append(out, c)
		}
	}
	return out
}

// CanonicalGoTypeOK maps a canonical schema type (the closed vocabulary
// schemadef.MapDeclaredType produces) to the Go type a column projects to
// on the generated entity struct — the NOT NULL variant; a nullable column
// adds a pointer, and an array column never does because a slice already
// carries nil.
//
// This is the SINGLE definition. There used to be three: this one, the CRUD
// conversion generator's dbBaseGoType, and internal/generator's
// planFieldGoType. They disagreed exactly where it mattered — dbBaseGoType
// collapsed every array column that was not BIGINT[] to []string, so a
// BYTEA[] column projected to []string in the half that decides whether a
// wire field pairs with it and to something else in the half that declares
// the struct field. Two projections of one column is one projection too
// many; the other two now route here.
//
// A json ARRAY column (jsonb[]) projects to []string: each element is a
// document, the same way a scalar json column projects to the document
// text.
//
// ok=false means the string is not a canonical schema type and the "string"
// answer is a FALLBACK rather than a mapping — the same distinction, for
// the same reason, that schemadef.MapDeclaredType draws with its `known`
// return. Every caller holds a type that is supposed to be canonical, so
// ok=false is a forge bug and each one reports it by name rather than
// declaring the field as text.
//
// The `default:` arm this used to end with covered "string, json, unknown"
// in one breath, which is the defect in miniature: `string` and `json` are
// MAPPED to Go string, and "unknown" is a type forge has no projection for.
// Collapsing the three meant a canonical type added upstream without a
// projection here became a Go `string` field in silence.
func CanonicalGoTypeOK(canonical string, isArray bool) (goType string, ok bool) {
	var base string
	switch canonical {
	case "string":
		base = "string"
	case "json":
		// A json column holds a document; the entity carries its text. A
		// json ARRAY column (jsonb[]) is []string for the same reason —
		// each element is one document.
		base = "string"
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
	default:
		base, ok = "string", false
		if isArray {
			return "[]" + base, ok
		}
		return base, ok
	}
	if isArray {
		return "[]" + base, true
	}
	return base, true
}

// canonicalGoTypeMust projects a column type that is known to be canonical
// — it came out of schemadef — so an unmapped one is a forge bug and stops
// generation by name rather than declaring the field as text.
func canonicalGoTypeMust(canonical string, isArray bool) string {
	goType, ok := CanonicalGoTypeOK(canonical, isArray)
	if !ok {
		panic("codegen: no Go projection for canonical schema type " + canonical)
	}
	return goType
}

// WireEntityFields extracts the entity wire-message fields from the
// service descriptor: the deep Schemas map when present, else the
// shallow Messages map (older descriptors). It is the WIRE half of an
// entity, the half BuildEntityConv pairs against the applied schema's
// columns — exported so the birth↔conversion contract can be asserted from
// internal/scaffold, which owns the other half.
func WireEntityFields(svc ServiceDef, entityName string) []EntityField {
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
		Name:     d.Name,
		GoName:   naming.ToProtoPascalCase(d.Name),
		Optional: d.Optional,
		Secret:   d.Secret,
		ReadOnly: d.ReadOnly,
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
		// The message and scalar arms both record `repeated` in GoType; the
		// enum arm did not, and the descriptor's repeated bit was simply
		// dropped. Every consumer then read a `repeated Status` field as a
		// singular one: the frontend mocked the number 1 into a Status[]
		// and rendered <StatusBadge value={item.tags}> against a prop typed
		// `string | number`. Kind deliberately stays FieldKindEnum — the
		// conversion generator branches on it, and a repeated enum is still
		// an enum.
		if d.Repeated {
			f.GoType = "[]" + f.GoType
		}
	case "map":
		f.ProtoType = "message"
		f.GoType = "map[string]string" // marker; the real key/value types live on the descriptor
		f.MapValueKind = d.MapValueKind
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
		Optional:  d.IsOptional,
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
