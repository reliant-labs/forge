package codegen

import (
	"fmt"
	"strings"
)

// proto<->entity conversion generation.
//
// Entity structs are projections of the APPLIED schema (time.Time,
// pointers for nullable columns, native slices); wire messages are the
// service-proto truth (timestamppb, wrappers, repeated fields). The
// CRUD ops file carries one generated conversion pair per entity —
// <entity>ToProto / <entity>FromProto — mapping the intersection of
// wire fields and columns by name. Wire-only fields never reach the
// database; column-only fields never leak onto the wire.

// EntityConvTemplateData renders one entity's conversion pair.
type EntityConvTemplateData struct {
	EntityName       string   // "Item"
	EntityLower      string   // "item"
	ToProtoAssigns   []string // statements: "m.Name = e.Name"
	FromProtoAssigns []string
}

// BuildEntityConv builds the conversion data for one entity.
func BuildEntityConv(svc ServiceDef, entity EntityDef) EntityConvTemplateData {
	conv := EntityConvTemplateData{
		EntityName:  entity.Name,
		EntityLower: strings.ToLower(entity.Name),
	}
	colByName := map[string]EntityColumn{}
	for _, c := range entity.Columns {
		colByName[c.Name] = c
	}
	for _, wf := range entity.Fields {
		col, ok := colByName[wf.Name]
		if !ok {
			conv.ToProtoAssigns = append(conv.ToProtoAssigns,
				fmt.Sprintf("// %s: wire-only field (no %q column in the applied schema)", wf.GoName, wf.Name))
			continue
		}
		if to, ok := assignToProto("m", "e", wf, col); ok {
			conv.ToProtoAssigns = append(conv.ToProtoAssigns, to)
		}
		if from, ok := assignToDB("e", "m", wf, col); ok {
			conv.FromProtoAssigns = append(conv.FromProtoAssigns, from)
		}
	}
	return conv
}

// ConvNeedsTimestamppb reports whether any assignment uses timestamppb.
func ConvNeedsTimestamppb(convs []EntityConvTemplateData) bool {
	for _, c := range convs {
		for _, a := range c.ToProtoAssigns {
			if strings.Contains(a, "timestamppb.") {
				return true
			}
		}
	}
	return false
}

// dbBaseGoType is the NOT NULL Go type of a column on the entity struct.
func dbBaseGoType(col EntityColumn) string {
	if col.IsArray {
		if col.Type == "int64" {
			return "[]int64"
		}
		return "[]string"
	}
	switch col.Type {
	case "int64":
		return "int64"
	case "float64":
		return "float64"
	case "bool":
		return "bool"
	case "time":
		return "time.Time"
	case "bytes":
		return "[]byte"
	default: // string, json
		return "string"
	}
}

// dbNullable reports whether the column maps to a pointer struct field.
func dbNullable(col EntityColumn) bool {
	if col.IsArray || col.Type == "bytes" {
		return false
	}
	return !col.NotNull && !col.IsPK
}

// assignToDB emits "dst.<X> = ..." converting a wire field onto the
// entity struct. src is the wire message variable.
func assignToDB(dst, src string, wf EntityField, col EntityColumn) (string, bool) {
	g := wf.GoName
	d, s := dst+"."+g, src+"."+g
	base := dbBaseGoType(col)
	nullable := dbNullable(col)

	switch {
	case wf.Kind == FieldKindTimestamp && col.Type == "time":
		if nullable {
			return fmt.Sprintf("if %s != nil {\n\t\tt := %s.AsTime()\n\t\t%s = &t\n\t}", s, s, d), true
		}
		return fmt.Sprintf("if %s != nil {\n\t\t%s = %s.AsTime()\n\t}", s, d, s), true

	case wf.Kind == FieldKindRepeatedScalar && col.IsArray:
		if wf.GoType == base {
			return fmt.Sprintf("%s = append(%s(nil), %s...)", d, base, s), true
		}
		if base == "[]int64" && (wf.GoType == "[]int32" || wf.GoType == "[]uint32") {
			return fmt.Sprintf("for _, v := range %s {\n\t\t%s = append(%s, int64(v))\n\t}", s, d, d), true
		}
		return fmt.Sprintf("// %s: no conversion from wire %s to column %s", g, wf.GoType, col.DeclType), false

	case wf.Kind == FieldKindWrapper && !col.IsArray:
		elem := strings.TrimPrefix(wf.GoType, "*")
		cast := func(expr string) string {
			if elem == base {
				return expr
			}
			if isNumericGoType(elem) && isNumericGoType(base) {
				return base + "(" + expr + ")"
			}
			return ""
		}
		c := cast("*" + s)
		if c == "" {
			return fmt.Sprintf("// %s: no conversion from wire %s to column %s", g, wf.GoType, col.DeclType), false
		}
		if nullable {
			return fmt.Sprintf("if %s != nil {\n\t\tv := %s\n\t\t%s = &v\n\t}", s, c, d), true
		}
		return fmt.Sprintf("if %s != nil {\n\t\t%s = %s\n\t}", s, d, c), true

	case wf.Kind == FieldKindScalar && !col.IsArray:
		expr := s
		if wf.GoType != base {
			if isNumericGoType(wf.GoType) && isNumericGoType(base) {
				expr = base + "(" + s + ")"
			} else {
				return fmt.Sprintf("// %s: no conversion from wire %s to column %s", g, wf.GoType, col.DeclType), false
			}
		}
		if nullable {
			return fmt.Sprintf("{\n\t\tv := %s\n\t\t%s = &v\n\t}", expr, d), true
		}
		return fmt.Sprintf("%s = %s", d, expr), true
	}
	return fmt.Sprintf("// %s: unmapped (wire kind %s, column %s)", g, wf.Kind, col.DeclType), false
}

// assignToProto emits "dst.<X> = ..." converting an entity struct field
// onto the wire message. src is the entity variable.
func assignToProto(dst, src string, wf EntityField, col EntityColumn) (string, bool) {
	g := wf.GoName
	d, s := dst+"."+g, src+"."+g
	base := dbBaseGoType(col)
	nullable := dbNullable(col)

	switch {
	case wf.Kind == FieldKindTimestamp && col.Type == "time":
		if nullable {
			return fmt.Sprintf("if %s != nil {\n\t\t%s = timestamppb.New(*%s)\n\t}", s, d, s), true
		}
		return fmt.Sprintf("if !%s.IsZero() {\n\t\t%s = timestamppb.New(%s)\n\t}", s, d, s), true

	case wf.Kind == FieldKindRepeatedScalar && col.IsArray:
		if wf.GoType == base {
			return fmt.Sprintf("%s = append(%s(nil), %s...)", d, wf.GoType, s), true
		}
		if base == "[]int64" && (wf.GoType == "[]int32" || wf.GoType == "[]uint32") {
			elem := strings.TrimPrefix(wf.GoType, "[]")
			return fmt.Sprintf("for _, v := range %s {\n\t\t%s = append(%s, %s(v))\n\t}", s, d, d, elem), true
		}
		return fmt.Sprintf("// %s: no conversion from column %s to wire %s", g, col.DeclType, wf.GoType), false

	case wf.Kind == FieldKindWrapper && !col.IsArray:
		elem := strings.TrimPrefix(wf.GoType, "*")
		castVal := func(expr string) string {
			if elem == base {
				return expr
			}
			if isNumericGoType(elem) && isNumericGoType(base) {
				return elem + "(" + expr + ")"
			}
			return ""
		}
		if nullable {
			c := castVal("*" + s)
			if c == "" {
				return fmt.Sprintf("// %s: no conversion from column %s to wire %s", g, col.DeclType, wf.GoType), false
			}
			return fmt.Sprintf("if %s != nil {\n\t\tv := %s\n\t\t%s = &v\n\t}", s, c, d), true
		}
		c := castVal(s)
		if c == "" {
			return fmt.Sprintf("// %s: no conversion from column %s to wire %s", g, col.DeclType, wf.GoType), false
		}
		return fmt.Sprintf("{\n\t\tv := %s\n\t\t%s = &v\n\t}", c, d), true

	case wf.Kind == FieldKindScalar && !col.IsArray:
		expr := s
		if nullable {
			if wf.GoType != base {
				if !(isNumericGoType(wf.GoType) && isNumericGoType(base)) {
					return fmt.Sprintf("// %s: no conversion from column %s to wire %s", g, col.DeclType, wf.GoType), false
				}
				return fmt.Sprintf("if %s != nil {\n\t\t%s = %s(*%s)\n\t}", s, d, wf.GoType, s), true
			}
			return fmt.Sprintf("if %s != nil {\n\t\t%s = *%s\n\t}", s, d, s), true
		}
		if wf.GoType != base {
			if isNumericGoType(wf.GoType) && isNumericGoType(base) {
				expr = wf.GoType + "(" + s + ")"
			} else {
				return fmt.Sprintf("// %s: no conversion from column %s to wire %s", g, col.DeclType, wf.GoType), false
			}
		}
		return fmt.Sprintf("%s = %s", d, expr), true
	}
	return fmt.Sprintf("// %s: unmapped (wire kind %s, column %s)", g, wf.Kind, col.DeclType), false
}

func isNumericGoType(t string) bool {
	switch t {
	case "int32", "int64", "uint32", "uint64", "float32", "float64":
		return true
	}
	return false
}

// buildCreateAssigns maps the create-request fields onto the entity
// struct. Request fields are matched to columns by name; unmatched
// fields are dropped with a comment so the generated code says why.
func buildCreateAssigns(svc ServiceDef, m Method, entity EntityDef) []string {
	wireFields := requestWireFields(svc, m)
	colByName := map[string]EntityColumn{}
	for _, c := range entity.Columns {
		colByName[c.Name] = c
	}
	var out []string
	for _, wf := range wireFields {
		col, ok := colByName[wf.Name]
		if !ok {
			out = append(out, fmt.Sprintf("// %s: request-only field (no %q column in the applied schema)", wf.GoName, wf.Name))
			continue
		}
		if a, ok := assignToDB("e", "req", wf, col); ok {
			out = append(out, a)
		} else {
			out = append(out, a)
		}
	}
	return out
}

// requestWireFields extracts a request message's fields, preferring the
// deep Schemas map (which knows about repeated/optional) over the
// shallow Messages map.
func requestWireFields(svc ServiceDef, m Method) []EntityField {
	if m.InputTypeFQ != "" {
		if defs, ok := svc.Schemas[m.InputTypeFQ]; ok {
			fields := make([]EntityField, 0, len(defs))
			for _, d := range defs {
				f := schemaFieldToEntityField(d)
				// proto3 optional scalars surface as wrapper-like
				// pointers on the Go struct.
				if d.Optional && f.Kind == FieldKindScalar {
					f.GoType = "*" + f.GoType
					f.Kind = FieldKindWrapper
				}
				fields = append(fields, f)
			}
			return fields
		}
	}
	defs, ok := svc.Messages[m.InputType]
	if !ok {
		return nil
	}
	fields := make([]EntityField, 0, len(defs))
	for _, d := range defs {
		f := messageFieldToEntityField(d)
		if d.IsOptional && f.Kind == FieldKindScalar {
			f.GoType = "*" + f.GoType
			f.Kind = FieldKindWrapper
		}
		fields = append(fields, f)
	}
	return fields
}
