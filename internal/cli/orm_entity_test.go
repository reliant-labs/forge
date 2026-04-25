package cli

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

func TestProtoKindToString(t *testing.T) {
	tests := []struct {
		kind protoreflect.Kind
		want string
	}{
		{protoreflect.BoolKind, "bool"},
		{protoreflect.Int32Kind, "int32"},
		{protoreflect.Sint32Kind, "sint32"},
		{protoreflect.Sfixed32Kind, "sfixed32"},
		{protoreflect.Uint32Kind, "uint32"},
		{protoreflect.Fixed32Kind, "fixed32"},
		{protoreflect.Int64Kind, "int64"},
		{protoreflect.Sint64Kind, "sint64"},
		{protoreflect.Sfixed64Kind, "sfixed64"},
		{protoreflect.Uint64Kind, "uint64"},
		{protoreflect.Fixed64Kind, "fixed64"},
		{protoreflect.FloatKind, "float"},
		{protoreflect.DoubleKind, "double"},
		{protoreflect.StringKind, "string"},
		{protoreflect.BytesKind, "bytes"},
		{protoreflect.MessageKind, "message"},
		{protoreflect.EnumKind, "enum"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := protoKindToString(tt.kind)
			if got != tt.want {
				t.Errorf("protoKindToString(%v) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

func TestSplitRef(t *testing.T) {
	tests := []struct {
		ref      string
		wantLen  int
		wantPart []string
	}{
		{"organizations.id", 2, []string{"organizations", "id"}},
		{"users.email", 2, []string{"users", "email"}},
		{"table_name.column_name", 2, []string{"table_name", "column_name"}},
		{"no_dot", 1, []string{"no_dot"}},
		{"a.b.c", 2, []string{"a.b", "c"}}, // splits on last dot
		{"", 1, []string{""}},
	}

	for _, tt := range tests {
		t.Run(tt.ref, func(t *testing.T) {
			got := splitRef(tt.ref)
			if len(got) != tt.wantLen {
				t.Fatalf("splitRef(%q) returned %d parts, want %d: %v", tt.ref, len(got), tt.wantLen, got)
			}
			for i, want := range tt.wantPart {
				if got[i] != want {
					t.Errorf("splitRef(%q)[%d] = %q, want %q", tt.ref, i, got[i], want)
				}
			}
		})
	}
}

func TestToSnake(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"PatientStatus", "patient_status"},
		{"ID", "i_d"},
		{"SimpleField", "simple_field"},
		{"already_snake", "already_snake"},
		{"HTMLParser", "h_t_m_l_parser"},
		{"A", "a"},
		{"", ""},
		{"lowercase", "lowercase"},
		{"camelCase", "camel_case"},
		{"OrgId", "org_id"},
		{"MyHTTPClient", "my_h_t_t_p_client"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := toSnake(tt.input)
			if got != tt.want {
				t.Errorf("toSnake(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestInferTableName(t *testing.T) {
	// inferTableName uses toSnake() then a simple pluralization that only
	// handles -y → -ies and -s → -ses. Other suffixes just get +s.
	tests := []struct {
		messageName string
		want        string
	}{
		{"Patient", "patients"},
		{"Category", "categories"},  // -y → -ies
		{"Address", "addresses"},    // -s → -ses
		{"User", "users"},
		{"Status", "statuses"},      // -s → -ses
		{"Company", "companies"},    // -y → -ies
		{"Policy", "policies"},      // -y → -ies
		{"Key", "keies"},            // simple -y → -ies (no vowel check)
		{"Bus", "buses"},            // toSnake("Bus")="bus" → bus+es
		{"Tax", "taxs"},             // no special -x handling
		{"Dish", "dishs"},           // no special -sh handling
		{"Match", "matchs"},         // no special -ch handling
		{"Boy", "boies"},            // simple -y → -ies (no vowel check)
		{"Day", "daies"},            // simple -y → -ies (no vowel check)
	}

	for _, tt := range tests {
		t.Run(tt.messageName, func(t *testing.T) {
			got := inferTableName(tt.messageName)
			if got != tt.want {
				t.Errorf("inferTableName(%q) = %q, want %q", tt.messageName, got, tt.want)
			}
		})
	}
}

func TestPluralize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"patient", "patients"},
		{"category", "categories"},
		{"address", "addresses"},
		{"user", "users"},
		{"status", "statuses"},
		{"company", "companies"},
		{"policy", "policies"},
		{"key", "keys"},
		{"boy", "boys"},
		{"day", "days"},
		{"bus", "buses"},
		{"box", "boxes"},
		{"dish", "dishes"},
		{"match", "matches"},
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

func TestScalarGoType(t *testing.T) {
	tests := []struct {
		kind protoreflect.Kind
		want string
	}{
		{protoreflect.StringKind, "string"},
		{protoreflect.Int32Kind, "int32"},
		{protoreflect.Sint32Kind, "int32"},
		{protoreflect.Sfixed32Kind, "int32"},
		{protoreflect.Int64Kind, "int64"},
		{protoreflect.Sint64Kind, "int64"},
		{protoreflect.Sfixed64Kind, "int64"},
		{protoreflect.Uint32Kind, "uint32"},
		{protoreflect.Fixed32Kind, "uint32"},
		{protoreflect.Uint64Kind, "uint64"},
		{protoreflect.Fixed64Kind, "uint64"},
		{protoreflect.BoolKind, "bool"},
		{protoreflect.FloatKind, "float32"},
		{protoreflect.DoubleKind, "float64"},
		{protoreflect.BytesKind, "[]byte"},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := scalarGoType(tt.kind)
			if got != tt.want {
				t.Errorf("scalarGoType(%v) = %q, want %q", tt.kind, got, tt.want)
			}
		})
	}
}

func TestIsIntegerKind(t *testing.T) {
	intKinds := []protoreflect.Kind{
		protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind,
	}
	for _, k := range intKinds {
		if !isIntegerKind(k) {
			t.Errorf("isIntegerKind(%v) = false, want true", k)
		}
	}

	nonIntKinds := []protoreflect.Kind{
		protoreflect.StringKind, protoreflect.BoolKind, protoreflect.FloatKind,
		protoreflect.DoubleKind, protoreflect.BytesKind, protoreflect.MessageKind,
		protoreflect.EnumKind,
	}
	for _, k := range nonIntKinds {
		if isIntegerKind(k) {
			t.Errorf("isIntegerKind(%v) = true, want false", k)
		}
	}
}

func TestIsCommonNotNullField(t *testing.T) {
	notNullFields := []string{
		"name", "title", "status", "type", "slug",
		"username", "first_name", "last_name",
		"role", "state", "kind", "code",
	}
	for _, f := range notNullFields {
		if !isCommonNotNullField(f) {
			t.Errorf("isCommonNotNullField(%q) = false, want true", f)
		}
	}

	nullableFields := []string{
		"description", "notes", "avatar", "middle_name",
		"phone", "address", "metadata",
	}
	for _, f := range nullableFields {
		if isCommonNotNullField(f) {
			t.Errorf("isCommonNotNullField(%q) = true, want false", f)
		}
	}
}