// protoc-gen-forge-orm is a protoc plugin that generates type-safe ORM
// code for proto messages annotated with EntityOptions and FieldOptions.
//
// For each annotated message it produces a <name>.pb.orm.go file containing:
//   - Model and Scanner interface implementations
//   - CRUD functions: Create, GetByID, List, Update, Delete
//
// Generated code uses github.com/reliant-labs/forge/pkg/orm.
package main

import (
	"strings"
	"unicode"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
)

// Extension field numbers from proto/forge/options/v1/entity.proto and field.proto.
const (
	entityOptionsFieldNum protoreflect.FieldNumber = 50200
	fieldOptionsFieldNum  protoreflect.FieldNumber = 50300
)

func main() {
	protogen.Options{}.Run(func(p *protogen.Plugin) error {
		for _, f := range p.Files {
			if !f.Generate {
				continue
			}
			if err := generateFile(p, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// entityInfo holds parsed entity metadata for a single annotated message.
type entityInfo struct {
	msg        *protogen.Message
	tableName  string
	softDelete bool
	timestamps bool
	fields     []fieldInfo
	pkField    *fieldInfo
	inferred   bool // true when entity was inferred from conventions (no explicit annotation)
}

// fieldInfo holds parsed column metadata for a single field.
type fieldInfo struct {
	field         *protogen.Field
	columnName    string
	isPK          bool
	notNull       bool
	isTimestamp   bool
	unique        bool
	autoIncrement bool
	references    string // FK reference in "table.column" format
}

func generateFile(p *protogen.Plugin, file *protogen.File) error {
	var entities []entityInfo

	for _, msg := range file.Messages {
		ent, ok := parseEntity(msg)
		if !ok {
			continue
		}
		entities = append(entities, ent)
	}

	if len(entities) == 0 {
		return nil
	}

	// Check if any entity has timestamps for the shared file.
	anyHasTimestamp := false
	for _, ent := range entities {
		for _, f := range ent.fields {
			if f.isTimestamp {
				anyHasTimestamp = true
				break
			}
		}
		if anyHasTimestamp {
			break
		}
	}

	// Generate a shared file with package-level declarations (ormTracer, etc.)
	sharedFilename := file.GeneratedFilenamePrefix + "_orm_shared.pb.orm.go"
	shared := p.NewGeneratedFile(sharedFilename, file.GoImportPath)
	generateSharedHeader(shared, file, anyHasTimestamp)

	// Generate per-entity files.
	for _, ent := range entities {
		entHasTimestamp := false
		for _, f := range ent.fields {
			if f.isTimestamp {
				entHasTimestamp = true
				break
			}
		}

		filename := file.GeneratedFilenamePrefix + "_" + toSnake(string(ent.msg.Desc.Name())) + ".pb.orm.go"
		g := p.NewGeneratedFile(filename, file.GoImportPath)

		generateEntityHeader(g, file, entHasTimestamp, ent.softDelete)
		generateEntityCode(g, ent, entHasTimestamp)
	}

	return nil
}

func parseEntity(msg *protogen.Message) (entityInfo, bool) {
	var tableName string
	var softDelete bool
	var timestamps bool
	found := false
	inferred := false

	opts := msg.Desc.Options()
	if opts != nil {
		if msgOpts, ok := opts.(*descriptorpb.MessageOptions); ok {
			// Look for the entity_options extension (field 50200).
			msgOpts.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
				if fd.Number() == entityOptionsFieldNum {
					found = true
					entMsg := v.Message()
					if tn := entMsg.Get(entMsg.Descriptor().Fields().ByName("table_name")); tn.IsValid() {
						tableName = tn.String()
					}
					if sd := entMsg.Get(entMsg.Descriptor().Fields().ByName("soft_delete")); sd.IsValid() {
						softDelete = sd.Bool()
					}
					if ts := entMsg.Get(entMsg.Descriptor().Fields().ByName("timestamps")); ts.IsValid() {
						timestamps = ts.Bool()
					}
					return false
				}
				return true
			})

			// If Range didn't find it, check unknown fields (raw extension bytes).
			if !found {
				raw := msgOpts.ProtoReflect().GetUnknown()
				var rawTN string
				var rawSD bool
				found, rawTN, rawSD = parseRawEntityOptions(raw, entityOptionsFieldNum)
				if found {
					if rawTN != "" {
						tableName = rawTN
					}
					softDelete = rawSD
				}
			}
		}
	}

	// Convention-based inference: if no explicit annotation, check if the message
	// looks like a DB entity (has an "id" field and is in a db proto path).
	if !found {
		if looksLikeEntity(msg) {
			found = true
			inferred = true
		} else {
			return entityInfo{}, false
		}
	}

	if tableName == "" {
		tableName = inferTableName(string(msg.Desc.Name()))
	}

	ent := entityInfo{
		msg:        msg,
		tableName:  tableName,
		softDelete: softDelete,
		timestamps: timestamps,
		inferred:   inferred,
	}

	for _, f := range msg.Fields {
		fi := parseField(f)
		ent.fields = append(ent.fields, fi)
		if fi.isPK {
			pk := fi
			ent.pkField = &pk
		}
	}

	// Default to "id" as PK if none explicitly annotated.
	if ent.pkField == nil {
		for i, f := range ent.fields {
			if f.columnName == "id" {
				ent.fields[i].isPK = true
				ent.fields[i].notNull = true
				// Infer auto_increment for integer PK fields.
				if isIntegerKind(ent.fields[i].field.Desc.Kind()) {
					ent.fields[i].autoIncrement = true
				}
				pk := ent.fields[i]
				ent.pkField = &pk
				break
			}
		}
	}

	// Apply convention-based field inferences.
	applyFieldInferences(&ent)

	// Infer entity-level options from field presence.
	inferEntityOptions(&ent)

	return ent, true
}

// parseRawEntityOptions scans raw unknown proto bytes for the entity_options extension
// and extracts the table_name (field 1, string) and soft_delete (field 3, bool) values.
func parseRawEntityOptions(b []byte, fieldNum protoreflect.FieldNumber) (found bool, tableName string, softDelete bool) {
	for len(b) > 0 {
		v, n := consumeVarint(b)
		if n < 0 {
			return false, "", false
		}
		num := protoreflect.FieldNumber(v >> 3)
		wtype := v & 0x7
		b = b[n:]

		if num == fieldNum && wtype == 2 {
			// Length-delimited: this is the embedded EntityOptions message.
			length, vn := consumeVarint(b)
			if vn < 0 || vn+int(length) > len(b) {
				return false, "", false
			}
			msgBytes := b[vn : vn+int(length)]
			tn, sd := parseEntityOptionsMsg(msgBytes)
			return true, tn, sd
		}

		// Skip the field value based on wire type.
		switch wtype {
		case 0: // varint
			_, n = consumeVarint(b)
		case 1: // 64-bit
			n = 8
		case 2: // length-delimited
			length, vn := consumeVarint(b)
			if vn < 0 {
				return false, "", false
			}
			n = vn + int(length)
		case 5: // 32-bit
			n = 4
		default:
			return false, "", false
		}

		if n < 0 || n > len(b) {
			return false, "", false
		}
		b = b[n:]
	}
	return false, "", false
}

// parseEntityOptionsMsg parses the inner EntityOptions message bytes.
// Field 1 (table_name) = string, Field 3 (soft_delete) = bool/varint.
func parseEntityOptionsMsg(b []byte) (tableName string, softDelete bool) {
	for len(b) > 0 {
		v, n := consumeVarint(b)
		if n < 0 {
			return
		}
		num := v >> 3
		wtype := v & 0x7
		b = b[n:]

		switch {
		case num == 1 && wtype == 2: // table_name (string, length-delimited)
			length, vn := consumeVarint(b)
			if vn < 0 || vn+int(length) > len(b) {
				return
			}
			tableName = string(b[vn : vn+int(length)])
			b = b[vn+int(length):]
		case num == 3 && wtype == 0: // soft_delete (bool/varint)
			val, vn := consumeVarint(b)
			if vn < 0 {
				return
			}
			softDelete = val != 0
			b = b[vn:]
		default:
			// Skip unknown field.
			switch wtype {
			case 0:
				_, n = consumeVarint(b)
			case 1:
				n = 8
			case 2:
				length, vn := consumeVarint(b)
				if vn < 0 {
					return
				}
				n = vn + int(length)
			case 5:
				n = 4
			default:
				return
			}
			if n < 0 || n > len(b) {
				return
			}
			b = b[n:]
		}
	}
	return
}

// parseRawFieldOptions scans raw unknown proto bytes for the field_options extension
// and extracts the primary_key (field 1, bool) and not_null (field 2, bool) values.
func parseRawFieldOptions(b []byte, fieldNum protoreflect.FieldNumber) (found bool, primaryKey bool, notNull bool) {
	for len(b) > 0 {
		v, n := consumeVarint(b)
		if n < 0 {
			return false, false, false
		}
		num := protoreflect.FieldNumber(v >> 3)
		wtype := v & 0x7
		b = b[n:]

		if num == fieldNum && wtype == 2 {
			// Length-delimited: this is the embedded FieldOptions message.
			length, vn := consumeVarint(b)
			if vn < 0 || vn+int(length) > len(b) {
				return false, false, false
			}
			msgBytes := b[vn : vn+int(length)]
			pk, nn := parseFieldOptionsMsg(msgBytes)
			return true, pk, nn
		}

		// Skip the field value based on wire type.
		switch wtype {
		case 0: // varint
			_, n = consumeVarint(b)
		case 1: // 64-bit
			n = 8
		case 2: // length-delimited
			length, vn := consumeVarint(b)
			if vn < 0 {
				return false, false, false
			}
			n = vn + int(length)
		case 5: // 32-bit
			n = 4
		default:
			return false, false, false
		}

		if n < 0 || n > len(b) {
			return false, false, false
		}
		b = b[n:]
	}
	return false, false, false
}

// parseFieldOptionsMsg parses the inner FieldOptions message bytes.
// Field 1 (primary_key) = bool/varint, Field 3 (not_null) = bool/varint.
func parseFieldOptionsMsg(b []byte) (primaryKey bool, notNull bool) {
	for len(b) > 0 {
		v, n := consumeVarint(b)
		if n < 0 {
			return
		}
		num := v >> 3
		wtype := v & 0x7
		b = b[n:]

		switch {
		case num == 1 && wtype == 0: // primary_key (bool/varint)
			val, vn := consumeVarint(b)
			if vn < 0 {
				return
			}
			primaryKey = val != 0
			b = b[vn:]
		case num == 3 && wtype == 0: // not_null (bool/varint)
			val, vn := consumeVarint(b)
			if vn < 0 {
				return
			}
			notNull = val != 0
			b = b[vn:]
		default:
			// Skip unknown field.
			switch wtype {
			case 0:
				_, n = consumeVarint(b)
			case 1:
				n = 8
			case 2:
				length, vn := consumeVarint(b)
				if vn < 0 {
					return
				}
				n = vn + int(length)
			case 5:
				n = 4
			default:
				return
			}
			if n < 0 || n > len(b) {
				return
			}
			b = b[n:]
		}
	}
	return
}

func consumeVarint(b []byte) (uint64, int) {
	var v uint64
	for i, c := range b {
		if i >= 10 {
			return 0, -1
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, -1
}

func parseField(f *protogen.Field) fieldInfo {
	fi := fieldInfo{
		field:      f,
		columnName: toSnake(string(f.Desc.Name())),
	}

	if f.Desc.Message() != nil && f.Desc.Message().FullName() == "google.protobuf.Timestamp" {
		fi.isTimestamp = true
	}

	opts := f.Desc.Options()
	if opts == nil {
		return fi
	}

	fOpts, ok := opts.(*descriptorpb.FieldOptions)
	if !ok {
		return fi
	}

	found := false
	fOpts.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Number() == fieldOptionsFieldNum {
			found = true
			foMsg := v.Message()
			desc := foMsg.Descriptor()

			if pkFd := desc.Fields().ByName("primary_key"); pkFd != nil {
				fi.isPK = foMsg.Get(pkFd).Bool()
			}
			if nnFd := desc.Fields().ByName("not_null"); nnFd != nil {
				fi.notNull = foMsg.Get(nnFd).Bool()
			}
			return false
		}
		return true
	})

	// If Range didn't find it, check unknown fields (raw extension bytes).
	if !found {
		raw := fOpts.ProtoReflect().GetUnknown()
		var rawPK, rawNN bool
		var rawFound bool
		rawFound, rawPK, rawNN = parseRawFieldOptions(raw, fieldOptionsFieldNum)
		if rawFound {
			fi.isPK = rawPK
			fi.notNull = rawNN
		}
	}

	return fi
}

// looksLikeEntity returns true if a message without explicit entity_options
// appears to be a database entity based on naming conventions.
// A message is considered an entity if it has an "id" field as its first field
// and is located within a proto/db/ path.
func looksLikeEntity(msg *protogen.Message) bool {
	// Check if in a db proto path.
	path := string(msg.Desc.ParentFile().Path())
	if !strings.Contains(path, "db/") && !strings.Contains(path, "db\\") {
		return false
	}
	// Must have at least one field, and the first field should be named "id".
	fields := msg.Desc.Fields()
	if fields.Len() == 0 {
		return false
	}
	return string(fields.Get(0).Name()) == "id"
}

// isIntegerKind returns true for proto field kinds that map to integer DB types.
func isIntegerKind(k protoreflect.Kind) bool {
	switch k {
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return true
	}
	return false
}

// applyFieldInferences applies convention-based annotations to fields that lack
// explicit proto annotations. This runs after initial parsing.
func applyFieldInferences(ent *entityInfo) {
	for i := range ent.fields {
		f := &ent.fields[i]

		// Skip fields that already have explicit annotations (PK already handled).
		if f.isPK {
			continue
		}

		name := f.columnName

		// Fields ending in _id → infer foreign key reference and not_null.
		if strings.HasSuffix(name, "_id") && name != "id" {
			if f.references == "" {
				refTable := pluralize(strings.TrimSuffix(name, "_id"))
				f.references = refTable + ".id"
			}
			f.notNull = true
		}

		// Field named "email" → infer unique.
		if name == "email" {
			f.unique = true
			f.notNull = true
		}

		// Common not_null fields.
		if isCommonNotNullField(name) {
			f.notNull = true
		}
	}
}

// inferEntityOptions infers entity-level options from field presence when not
// explicitly set via annotations.
func inferEntityOptions(ent *entityInfo) {
	hasCreatedAt := false
	hasUpdatedAt := false
	hasDeletedAt := false

	for _, f := range ent.fields {
		switch f.columnName {
		case "created_at":
			hasCreatedAt = true
		case "updated_at":
			hasUpdatedAt = true
		case "deleted_at":
			hasDeletedAt = true
		}
	}

	// If timestamps option wasn't explicitly set and fields are missing, infer it.
	if !ent.timestamps && (!hasCreatedAt || !hasUpdatedAt) {
		ent.timestamps = true
	}

	// If soft_delete wasn't explicitly set and deleted_at is missing, infer it.
	if !ent.softDelete && !hasDeletedAt {
		ent.softDelete = true
	}
}

// isCommonNotNullField returns true for field names that conventionally should be NOT NULL.
func isCommonNotNullField(name string) bool {
	switch name {
	case "name", "title", "status", "type", "slug",
		"username", "first_name", "last_name",
		"role", "state", "kind", "code":
		return true
	}
	return false
}

// pluralize applies simple English pluralization rules.
func pluralize(s string) string {
	if s == "" {
		return s
	}
	if strings.HasSuffix(s, "y") {
		// Check if preceded by a vowel — if so, just add "s".
		if len(s) >= 2 {
			prev := s[len(s)-2]
			if prev == 'a' || prev == 'e' || prev == 'i' || prev == 'o' || prev == 'u' {
				return s + "s"
			}
		}
		return s[:len(s)-1] + "ies"
	}
	if strings.HasSuffix(s, "s") || strings.HasSuffix(s, "x") ||
		strings.HasSuffix(s, "sh") || strings.HasSuffix(s, "ch") {
		return s + "es"
	}
	return s + "s"
}

// generateSharedHeader emits the shared, once-per-proto-file declarations:
// package, imports needed for ormTracer, the ormTracer var, and blank-identifier guards.
func generateSharedHeader(g *protogen.GeneratedFile, file *protogen.File, hasTimestamp bool) {
	g.P("// Code generated by protoc-gen-forge-orm. DO NOT EDIT.")
	g.P()
	g.P("package ", file.GoPackageName)
	g.P()
	g.P("import (")
	g.P(`	"go.opentelemetry.io/otel"`)
	g.P(")")
	g.P()
	g.P(`var ormTracer = otel.Tracer("orm")`)
	g.P()
}

// generateEntityHeader emits per-entity file declarations:
// package and imports needed by entity code (NO ormTracer, NO blank-identifier guards).
func generateEntityHeader(g *protogen.GeneratedFile, file *protogen.File, hasTimestamp bool, softDelete bool) {
	g.P("// Code generated by protoc-gen-forge-orm. DO NOT EDIT.")
	g.P()
	g.P("package ", file.GoPackageName)
	g.P()
	g.P("import (")
	g.P(`	"context"`)
	g.P(`	"fmt"`)
	if softDelete {
		g.P(`	"strings"`)
	}
	if hasTimestamp {
		g.P(`	"time"`)
	}
	g.P()
	g.P(`	"github.com/reliant-labs/forge/pkg/orm"`)
	g.P(`	"go.opentelemetry.io/otel/attribute"`)
	g.P(`	"go.opentelemetry.io/otel/codes"`)
	g.P(`	"go.opentelemetry.io/otel/trace"`)
	if hasTimestamp {
		g.P(`	"google.golang.org/protobuf/types/known/timestamppb"`)
	}
	g.P(")")
	g.P()
}


func generateEntityCode(g *protogen.GeneratedFile, ent entityInfo, hasTimestamp bool) {
	msgName := ent.msg.GoIdent.GoName

	columns := make([]string, 0, len(ent.fields))
	for _, f := range ent.fields {
		columns = append(columns, f.columnName)
	}

	pkCol := "id"
	if ent.pkField != nil {
		pkCol = ent.pkField.columnName
	}

	// --- Interface assertions ---
	g.P("// Compile-time checks that ", msgName, " implements orm.Model and orm.Scanner.")
	g.P("var (")
	g.P("	_ orm.Model   = (*", msgName, ")(nil)")
	g.P("	_ orm.Scanner = (*", msgName, ")(nil)")
	g.P(")")
	g.P()

	// --- Field name constants ---
	g.P("// Field name constants for ", msgName, ".")
	g.P("const ", msgName, "TableName = ", `"`, ent.tableName, `"`)
	g.P()
	g.P("const (")
	for _, f := range ent.fields {
		constName := msgName + "Field" + f.field.GoName
		g.P("	", constName, ` = "`, f.columnName, `"`)
	}
	g.P(")")
	g.P()

	// --- TableName ---
	g.P("// TableName returns the database table name for ", msgName, ".")
	g.P("func (*", msgName, ") TableName() string {")
	g.P(`	return "`, ent.tableName, `"`)
	g.P("}")
	g.P()

	// --- Schema ---
	generateSchema(g, ent, msgName)

	// --- PrimaryKey ---
	g.P("// PrimaryKey returns the primary key value.")
	if ent.pkField != nil {
		g.P("func (m *", msgName, ") PrimaryKey() any {")
		g.P("	return m.", ent.pkField.field.GoName)
	} else {
		g.P("func (m *", msgName, ") PrimaryKey() any {")
		g.P("	return nil")
	}
	g.P("}")
	g.P()

	// --- Values ---
	generateValues(g, ent, msgName)

	// --- Scan ---
	generateScan(g, ent, msgName)

	// --- CRUD functions ---
	generateCreate(g, ent, msgName, columns)
	generateGetByID(g, ent, msgName, pkCol)
	generateList(g, ent, msgName)
	generateUpdate(g, ent, msgName, columns, pkCol)
	generateDelete(g, ent, msgName, pkCol)
}

func generateSchema(g *protogen.GeneratedFile, ent entityInfo, msgName string) {
	g.P("// Schema returns the complete table schema for ", msgName, ".")
	g.P("func (*", msgName, ") Schema() orm.TableSchema {")
	g.P("	return orm.TableSchema{")
	g.P(`		Name: "`, ent.tableName, `",`)
	g.P("		Fields: []orm.FieldSchema{")

	for _, f := range ent.fields {
		ormType := protoKindToOrmType(f.field, f.isTimestamp)
		// Use SERIAL/BIGSERIAL for auto-increment integer PKs.
		if f.autoIncrement && f.isPK {
			switch f.field.Desc.Kind() {
			case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
				protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
				ormType = "orm.TypeBigSerial"
			default:
				ormType = "orm.TypeSerial"
			}
		}
		g.P("			{")
		g.P(`				Name:    "`, f.columnName, `",`)
		g.P("				Type:    ", ormType, ",")
		if f.isPK {
			g.P("				PrimaryKey: true,")
		}
		if f.notNull || f.isPK {
			g.P("				NotNull: true,")
		}
		if f.unique {
			g.P("				Unique: true,")
		}
		g.P("			},")
	}

	g.P("		},")

	// Generate indexes for FK fields (inferred references).
	var fkCols []string
	for _, f := range ent.fields {
		if f.references != "" && !f.isPK {
			fkCols = append(fkCols, f.columnName)
		}
	}
	if len(fkCols) > 0 {
		g.P("		Indexes: []orm.IndexSchema{")
		for _, col := range fkCols {
			idxName := "idx_" + ent.tableName + "_" + col
			g.P("			{")
			g.P(`				Name:   "`, idxName, `",`)
			g.P(`				Fields: []string{"`, col, `"},`)
			g.P("			},")
		}
		g.P("		},")
	}

	g.P("	}")
	g.P("}")
	g.P()
}

func generateValues(g *protogen.GeneratedFile, ent entityInfo, msgName string) {
	g.P("// Values returns the column names and values for this model.")
	g.P("func (m *", msgName, ") Values() ([]string, []any) {")
	g.P("	columns := []string{")
	for _, f := range ent.fields {
		g.P(`		"`, f.columnName, `",`)
	}
	g.P("	}")
	g.P()
	g.P("	values := []any{")
	for _, f := range ent.fields {
		accessor := "m." + f.field.GoName
		if f.isTimestamp {
			if f.notNull || f.isPK {
				// Not-null timestamps: return zero time if nil (DB DEFAULT will handle it).
				g.P("		func() any { if ", accessor, " != nil { return ", accessor, ".AsTime() }; return time.Time{} }(),")
			} else {
				// Nullable timestamps: return nil when protobuf field is nil.
				g.P("		func() any { if ", accessor, " != nil { return ", accessor, ".AsTime() }; return nil }(),")
			}
		} else {
			g.P("		", accessor, ",")
		}
	}
	g.P("	}")
	g.P()
	g.P("	return columns, values")
	g.P("}")
	g.P()
}

func generateScan(g *protogen.GeneratedFile, ent entityInfo, msgName string) {
	g.P("// Scan scans a database row into the message.")
	g.P("func (m *", msgName, ") Scan(scanner interface{ Scan(...interface{}) error }) error {")

	// Declare temp variables for scanning
	g.P("	var (")
	for _, f := range ent.fields {
		varName := "db" + f.field.GoName
		if f.isTimestamp {
			if f.notNull || f.isPK {
				g.P("		", varName, " time.Time")
			} else {
				g.P("		", varName, " *time.Time")
			}
		} else {
			goType := goTypeForField(f.field)
			if !f.notNull && !f.isPK && isNullableType(f.field) {
				goType = "*" + goType
			}
			g.P("		", varName, " ", goType)
		}
	}
	g.P("	)")
	g.P()

	// Scan call
	g.P("	err := scanner.Scan(")
	for _, f := range ent.fields {
		g.P("		&db", f.field.GoName, ",")
	}
	g.P("	)")
	g.P("	if err != nil {")
	g.P("		return err")
	g.P("	}")
	g.P()

	// Assignment
	for _, f := range ent.fields {
		varName := "db" + f.field.GoName
		if f.isTimestamp {
			if f.notNull || f.isPK {
				g.P("	m.", f.field.GoName, " = timestamppb.New(", varName, ")")
			} else {
				g.P("	if ", varName, " != nil {")
				g.P("		m.", f.field.GoName, " = timestamppb.New(*", varName, ")")
				g.P("	}")
			}
		} else if !f.notNull && !f.isPK && isNullableType(f.field) {
			g.P("	if ", varName, " != nil {")
			g.P("		m.", f.field.GoName, " = *", varName)
			g.P("	}")
		} else {
			g.P("	m.", f.field.GoName, " = ", varName)
		}
	}
	g.P()
	g.P("	return nil")
	g.P("}")
	g.P()
}

func generateCreate(g *protogen.GeneratedFile, ent entityInfo, msgName string, columns []string) {
	g.P("// Create", msgName, " inserts a new ", msgName, " row into the database.")
	g.P("func Create", msgName, "(ctx context.Context, db orm.Context, msg *", msgName, ") error {")
	g.P(`	ctx, span := ormTracer.Start(ctx, "orm.Create`, msgName, `",`)
	g.P(`		trace.WithAttributes(attribute.String("table", "`, ent.tableName, `")))`)
	g.P(`	defer span.End()`)
	g.P()
	g.P("	err := orm.Save(ctx, db, msg)")
	g.P("	if err != nil {")
	g.P(`		span.RecordError(err)`)
	g.P(`		span.SetStatus(codes.Error, err.Error())`)
	g.P(`		return fmt.Errorf("create `, ent.tableName, `: %w", err)`)
	g.P("	}")
	g.P("	return nil")
	g.P("}")
	g.P()
}

func generateGetByID(g *protogen.GeneratedFile, ent entityInfo, msgName string, pkCol string) {
	pkGoType := "string"
	if ent.pkField != nil {
		pkGoType = goTypeForField(ent.pkField.field)
	}

	g.P("// Get", msgName, "ByID retrieves a ", msgName, " by its primary key.")
	g.P("func Get", msgName, "ByID(ctx context.Context, db orm.Context, id ", pkGoType, ") (*", msgName, ", error) {")
	g.P(`	ctx, span := ormTracer.Start(ctx, "orm.Get`, msgName, `ByID",`)
	g.P(`		trace.WithAttributes(`)
	g.P(`			attribute.String("table", "`, ent.tableName, `"),`)
	g.P(`			attribute.String("id", fmt.Sprint(id)),`)
	g.P(`		))`)
	g.P(`	defer span.End()`)
	g.P()
	if ent.softDelete {
		// For soft-delete entities, use List with PK filter to exclude deleted rows.
		g.P(`	results, err := List`, msgName, `(ctx, db, orm.WhereEq("`, pkCol, `", id), orm.WithLimit(1))`)
		g.P("	if err != nil {")
		g.P(`		span.RecordError(err)`)
		g.P(`		span.SetStatus(codes.Error, err.Error())`)
		g.P("		return nil, err")
		g.P("	}")
		g.P("	if len(results) == 0 {")
		g.P(`		return nil, fmt.Errorf("get `, ent.tableName, ` by id: not found")`)
		g.P("	}")
		g.P("	return results[0], nil")
	} else {
		g.P("	msg := &", msgName, "{}")
		g.P("	if err := orm.Get[", msgName, "](ctx, db, msg, id); err != nil {")
		g.P(`		span.RecordError(err)`)
		g.P(`		span.SetStatus(codes.Error, err.Error())`)
		g.P(`		return nil, fmt.Errorf("get `, ent.tableName, ` by id: %w", err)`)
		g.P("	}")
		g.P("	return msg, nil")
	}
	g.P("}")
	g.P()
}

func generateList(g *protogen.GeneratedFile, ent entityInfo, msgName string) {
	g.P("// List", msgName, " retrieves ", msgName, " rows with optional filtering.")
	g.P("func List", msgName, "(ctx context.Context, db orm.Context, opts ...orm.QueryOption) ([]*", msgName, ", error) {")
	g.P(`	ctx, span := ormTracer.Start(ctx, "orm.List`, msgName, `",`)
	g.P(`		trace.WithAttributes(attribute.String("table", "`, ent.tableName, `")))`)
	g.P(`	defer span.End()`)
	g.P()

	if ent.softDelete {
		g.P("	// Prepend soft-delete filter.")
		g.P(`	opts = append([]orm.QueryOption{orm.WhereIsNull("deleted_at")}, opts...)`)
		g.P()
	}

	g.P("	results, err := orm.List[", msgName, "](ctx, db, opts...)")
	g.P("	if err != nil {")
	g.P(`		span.RecordError(err)`)
	g.P(`		span.SetStatus(codes.Error, err.Error())`)
	g.P(`		return nil, fmt.Errorf("list `, ent.tableName, `: %w", err)`)
	g.P("	}")
	g.P("	return results, nil")
	g.P("}")
	g.P()

	if ent.softDelete {
		generateListAll(g, ent, msgName)
	}

	// Also generate Count
	g.P("// Count", msgName, " returns the number of matching ", msgName, " rows.")
	g.P("func Count", msgName, "(ctx context.Context, db orm.Context, opts ...orm.QueryOption) (int64, error) {")
	g.P(`	ctx, span := ormTracer.Start(ctx, "orm.Count`, msgName, `",`)
	g.P(`		trace.WithAttributes(attribute.String("table", "`, ent.tableName, `")))`)
	g.P(`	defer span.End()`)
	g.P()

	if ent.softDelete {
		g.P(`	opts = append([]orm.QueryOption{orm.WhereIsNull("deleted_at")}, opts...)`)
		g.P()
	}

	g.P("	count, err := orm.Count[", msgName, "](ctx, db, opts...)")
	g.P("	if err != nil {")
	g.P(`		span.RecordError(err)`)
	g.P(`		span.SetStatus(codes.Error, err.Error())`)
	g.P(`		return 0, fmt.Errorf("count `, ent.tableName, `: %w", err)`)
	g.P("	}")
	g.P("	return count, nil")
	g.P("}")
	g.P()
}

func generateListAll(g *protogen.GeneratedFile, ent entityInfo, msgName string) {
	g.P("// ListAll", msgName, " retrieves all ", msgName, " rows including soft-deleted ones.")
	g.P("func ListAll", msgName, "(ctx context.Context, db orm.Context, opts ...orm.QueryOption) ([]*", msgName, ", error) {")
	g.P("\tctx, span := ormTracer.Start(ctx, \"orm.ListAll", msgName, "\",")
	g.P("\t\ttrace.WithAttributes(attribute.String(\"table\", \"", ent.tableName, "\")))")
	g.P("\tdefer span.End()")
	g.P()
	g.P("\tresults, err := orm.List[", msgName, "](ctx, db, opts...)")
	g.P("\tif err != nil {")
	g.P("\t\tspan.RecordError(err)")
	g.P("\t\tspan.SetStatus(codes.Error, err.Error())")
	g.P("\t\treturn nil, fmt.Errorf(\"list all ", ent.tableName, ": %w\", err)")
	g.P("\t}")
	g.P("\treturn results, nil")
	g.P("}")
	g.P()
}

func generateUpdate(g *protogen.GeneratedFile, ent entityInfo, msgName string, columns []string, pkCol string) {
	g.P("// Update", msgName, " updates an existing ", msgName, " by its primary key.")
	g.P("func Update", msgName, "(ctx context.Context, db orm.Context, msg *", msgName, ") error {")
	g.P(`	ctx, span := ormTracer.Start(ctx, "orm.Update`, msgName, `",`)
	g.P(`		trace.WithAttributes(attribute.String("table", "`, ent.tableName, `")))`)
	g.P(`	defer span.End()`)
	g.P()

	if ent.softDelete {
		// For soft-delete entities, build an explicit UPDATE that excludes deleted_at
		// and adds a WHERE deleted_at IS NULL guard.
		g.P("	dialect := db.Dialect()")
		g.P("	_, values := msg.Values()")
		g.P()

		// Build SET clause excluding PK and deleted_at columns.
		var setCols []string
		var setIdxes []int
		for i, f := range ent.fields {
			if f.isPK || f.columnName == "deleted_at" {
				continue
			}
			setCols = append(setCols, f.columnName)
			setIdxes = append(setIdxes, i)
		}

		// Find PK value index.
		pkIdx := 0
		for i, f := range ent.fields {
			if f.isPK {
				pkIdx = i
				break
			}
		}

		g.P("	setParts := []string{")
		for phIdx, col := range setCols {
			_ = setIdxes[phIdx]
			g.P(`		fmt.Sprintf("%s = %s", dialect.QuoteIdentifier("`, col, `"), dialect.Placeholder(`, phIdx, `)),`)
		}
		g.P("	}")
		g.P()

		pkPlaceholderIdx := len(setCols)
		g.P(`	query := fmt.Sprintf("UPDATE %s SET %s WHERE %s = %s AND %s IS NULL",`)
		g.P(`		dialect.QuoteIdentifier("`, ent.tableName, `"),`)
		g.P(`		strings.Join(setParts, ", "),`)
		g.P(`		dialect.QuoteIdentifier("`, pkCol, `"),`)
		g.P(`		dialect.Placeholder(`, pkPlaceholderIdx, `),`)
		g.P(`		dialect.QuoteIdentifier("deleted_at"),`)
		g.P("	)")
		g.P()

		g.P("	args := []any{")
		for _, idx := range setIdxes {
			g.P("		values[", idx, "],")
		}
		g.P("		values[", pkIdx, "],")
		g.P("	}")
		g.P()
		g.P("	_, err := db.Exec(ctx, query, args...)")
	} else {
		g.P("	// Save performs an upsert \u2014 it will update if the primary key exists.")
		g.P("	err := orm.Save(ctx, db, msg)")
	}

	g.P("	if err != nil {")
	g.P(`		span.RecordError(err)`)
	g.P(`		span.SetStatus(codes.Error, err.Error())`)
	g.P(`		return fmt.Errorf("update `, ent.tableName, `: %w", err)`)
	g.P("	}")
	g.P("	return nil")
	g.P("}")
	g.P()
}

func generateDelete(g *protogen.GeneratedFile, ent entityInfo, msgName string, pkCol string) {
	pkGoType := "string"
	if ent.pkField != nil {
		pkGoType = goTypeForField(ent.pkField.field)
	}

	if ent.softDelete {
		g.P("// Delete", msgName, " soft-deletes a ", msgName, " by setting deleted_at.")
	} else {
		g.P("// Delete", msgName, " permanently deletes a ", msgName, " by its primary key.")
	}
	g.P("func Delete", msgName, "(ctx context.Context, db orm.Context, id ", pkGoType, ") error {")
	g.P(`	ctx, span := ormTracer.Start(ctx, "orm.Delete`, msgName, `",`)
	g.P(`		trace.WithAttributes(`)
	g.P(`			attribute.String("table", "`, ent.tableName, `"),`)
	g.P(`			attribute.String("id", fmt.Sprint(id)),`)
	g.P(`		))`)
	g.P(`	defer span.End()`)
	g.P()

	if ent.softDelete {
		// Soft delete: UPDATE SET deleted_at = CURRENT_TIMESTAMP
		g.P("	// Soft delete: set deleted_at instead of removing the row.")
		g.P("	dialect := db.Dialect()")
		g.P(`	query := fmt.Sprintf("UPDATE %s SET %s = CURRENT_TIMESTAMP WHERE %s = %s AND %s IS NULL",`)
		g.P(`		dialect.QuoteIdentifier("`, ent.tableName, `"),`)
		g.P(`		dialect.QuoteIdentifier("deleted_at"),`)
		g.P(`		dialect.QuoteIdentifier("`, pkCol, `"),`)
		g.P(`		dialect.Placeholder(0),`)
		g.P(`		dialect.QuoteIdentifier("deleted_at"),`)
		g.P("	)")
		g.P("	_, err := db.Exec(ctx, query, id)")
	} else {
		// Hard delete: construct a minimal model with just the PK set and use orm.Delete.
		g.P("	msg := &", msgName, "{}")
		generatePKAssignment(g, ent, "id")
		g.P("	err := orm.Delete(ctx, db, msg)")
	}

	g.P("	if err != nil {")
	g.P(`		span.RecordError(err)`)
	g.P(`		span.SetStatus(codes.Error, err.Error())`)
	g.P(`		return fmt.Errorf("delete `, ent.tableName, `: %w", err)`)
	g.P("	}")
	g.P("	return nil")
	g.P("}")
	g.P()
}

// generatePKAssignment emits code to assign a value to the primary key field of a message.
func generatePKAssignment(g *protogen.GeneratedFile, ent entityInfo, srcVar string) {
	if ent.pkField == nil {
		return
	}
	goType := goTypeForField(ent.pkField.field)
	switch goType {
	case "string":
		g.P("	msg.", ent.pkField.field.GoName, " = ", srcVar)
	case "int32", "int64", "uint32", "uint64":
		g.P("	msg.", ent.pkField.field.GoName, " = ", srcVar)
	default:
		g.P("	msg.", ent.pkField.field.GoName, " = ", srcVar)
	}
}

// protoKindToOrmType maps proto field kinds to orm.FieldType constants.
func protoKindToOrmType(f *protogen.Field, isTimestamp bool) string {
	if isTimestamp {
		return "orm.TypeTimestampTZ"
	}
	switch f.Desc.Kind() {
	case protoreflect.StringKind:
		return "orm.TypeText"
	case protoreflect.BoolKind:
		return "orm.TypeBoolean"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "orm.TypeInteger"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "orm.TypeBigInt"
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return "orm.TypeText" // No dedicated float type; use TEXT for now
	case protoreflect.BytesKind:
		return "orm.TypeBytea"
	default:
		return "orm.TypeText"
	}
}

// isNullableType returns true if the proto field type maps to a Go type that
// should be scanned as a pointer when the column is nullable.
func isNullableType(f *protogen.Field) bool {
	switch f.Desc.Kind() {
	case protoreflect.StringKind,
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
		protoreflect.BoolKind,
		protoreflect.FloatKind, protoreflect.DoubleKind:
		return true
	default:
		return false
	}
}

// goTypeForField returns the Go type string for a proto field.
func goTypeForField(f *protogen.Field) string {
	switch f.Desc.Kind() {
	case protoreflect.StringKind:
		return "string"
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return "int32"
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return "int64"
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return "uint32"
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return "uint64"
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.FloatKind:
		return "float32"
	case protoreflect.DoubleKind:
		return "float64"
	case protoreflect.BytesKind:
		return "[]byte"
	default:
		return "string"
	}
}

func inferTableName(messageName string) string {
	s := toSnake(messageName)
	if strings.HasSuffix(s, "y") {
		return s[:len(s)-1] + "ies"
	}
	if strings.HasSuffix(s, "s") {
		return s + "es"
	}
	return s + "s"
}

func toSnake(s string) string {
	var result []rune
	for i, r := range s {
		if i > 0 && unicode.IsUpper(r) {
			result = append(result, '_')
		}
		result = append(result, unicode.ToLower(r))
	}
	return string(result)
}