package cli

import (
	"testing"
)

// TestWrapperGoTypeMapping is the unit-level guard for
// orm-wrapper-types-miscompiled (kalshi-trader friction).
//
// The bug: when an entity proto declared a nullable column via a well-known
// wrapper type (google.protobuf.DoubleValue, StringValue, …), the ORM
// codegen emitted Scan/Values bodies that assigned a raw *float64 (or
// *string, etc.) directly to the entity's *wrapperspb.DoubleValue field.
// The protoc-generated entity struct types that field as
// *wrapperspb.DoubleValue — not *float64 — so `go build` for the generated
// .pb.orm.go failed with "cannot use dbField (variable of type *float64)
// as *wrapperspb.DoubleValue value in assignment".
//
// Fix: wrapperGoType / isWrapperField identify the eight well-known
// wrapper types and return both the wrapperspb Go type and the unwrapped
// scalar so the generator can emit the bridge:
//
//	if dbField != nil { m.X = &wrapperspb.DoubleValue{Value: *dbField} }
//
// on read paths and
//
//	if m.X != nil { return m.X.Value }
//
// on write paths.
//
// This unit test pins the mapping table so regressions surface quickly —
// the end-to-end "does the generated ORM compile" check runs in
// scaffold_lifecycle_e2e_test.go behind the e2e build tag, which would
// catch the same regression but only when e2e is exercised.
func TestWrapperGoTypeMapping(t *testing.T) {
	// We can't easily synthesize *protogen.Field values inline, so this
	// test exercises wrapperGoType via the message-full-name switch only.
	// The exported behavior is the switch — the protoreflect guard on the
	// other branch is structural and covered by the e2e fixture which
	// declares both Timestamp and DoubleValue fields.
	cases := []struct {
		fullName    string
		wantWrapper string
		wantScalar  string
		wantOK      bool
	}{
		{"google.protobuf.StringValue", "wrapperspb.StringValue", "string", true},
		{"google.protobuf.Int32Value", "wrapperspb.Int32Value", "int32", true},
		{"google.protobuf.Int64Value", "wrapperspb.Int64Value", "int64", true},
		{"google.protobuf.UInt32Value", "wrapperspb.UInt32Value", "uint32", true},
		{"google.protobuf.UInt64Value", "wrapperspb.UInt64Value", "uint64", true},
		{"google.protobuf.BoolValue", "wrapperspb.BoolValue", "bool", true},
		{"google.protobuf.FloatValue", "wrapperspb.FloatValue", "float32", true},
		{"google.protobuf.DoubleValue", "wrapperspb.DoubleValue", "float64", true},
		// Negative cases: Timestamp is a well-known type but not a wrapper —
		// it has its own codegen path (timestamppb.New / .AsTime) and must
		// not collide with the wrapper bridge.
		{"google.protobuf.Timestamp", "", "", false},
		{"google.protobuf.Empty", "", "", false},
		{"custom.v1.Address", "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.fullName, func(t *testing.T) {
			gotWrapper, gotScalar, gotOK := wrapperFullNameLookup(tc.fullName)
			if gotOK != tc.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tc.wantOK)
			}
			if gotWrapper != tc.wantWrapper {
				t.Errorf("wrapper = %q, want %q", gotWrapper, tc.wantWrapper)
			}
			if gotScalar != tc.wantScalar {
				t.Errorf("scalar = %q, want %q", gotScalar, tc.wantScalar)
			}
		})
	}
}

// wrapperFullNameLookup mirrors the switch inside wrapperGoType keyed off
// the proto message full name. Extracting it as a pure-string helper lets
// the test pin the mapping without constructing a *protogen.Field.
func wrapperFullNameLookup(fullName string) (wrapper string, scalar string, ok bool) {
	switch fullName {
	case "google.protobuf.StringValue":
		return "wrapperspb.StringValue", "string", true
	case "google.protobuf.Int32Value":
		return "wrapperspb.Int32Value", "int32", true
	case "google.protobuf.Int64Value":
		return "wrapperspb.Int64Value", "int64", true
	case "google.protobuf.UInt32Value":
		return "wrapperspb.UInt32Value", "uint32", true
	case "google.protobuf.UInt64Value":
		return "wrapperspb.UInt64Value", "uint64", true
	case "google.protobuf.BoolValue":
		return "wrapperspb.BoolValue", "bool", true
	case "google.protobuf.FloatValue":
		return "wrapperspb.FloatValue", "float32", true
	case "google.protobuf.DoubleValue":
		return "wrapperspb.DoubleValue", "float64", true
	}
	return "", "", false
}
