package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// dumpJSON runs the command body in JSON mode for the given kind and decodes
// the result, failing the test on any error (invalid JSON included).
func dumpJSON(t *testing.T, kind string) AnnotationsSpec {
	t.Helper()
	var buf bytes.Buffer
	if err := runAnnotations(&buf, kind, true); err != nil {
		t.Fatalf("runAnnotations(kind=%q): %v", kind, err)
	}
	var spec AnnotationsSpec
	if err := json.Unmarshal(buf.Bytes(), &spec); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	return spec
}

func TestAnnotations_JSONValidAndComplete(t *testing.T) {
	spec := dumpJSON(t, "")

	// All five markers.
	markerNames := map[string]bool{}
	for _, m := range spec.Markers {
		markerNames[m.Name] = true
	}
	for _, want := range []string{
		"forge:entity", "forge:soft-delete", "forge:append-only",
		"forge:server-set", "forge:secret", "forge:mutation",
	} {
		if !markerNames[want] {
			t.Errorf("markers missing %q", want)
		}
	}
	if len(spec.Markers) != 6 {
		t.Errorf("expected 6 markers, got %d", len(spec.Markers))
	}

	// The proto→column mapping a birth applies: every proto3 scalar kind
	// the renderer maps, plus the shape-driven rows.
	fieldTypes := map[string]FieldTypeSpec{}
	for _, ft := range spec.FieldTypes {
		if ft.SQLType == "" || ft.ProtoType == "" {
			t.Errorf("field type entry is incomplete: %+v", ft)
		}
		fieldTypes[ft.ProtoType] = ft
	}
	for _, want := range []string{
		// scalars, projected from the renderer's own scalarSQL
		"string", "bool", "bytes", "double", "float",
		"int32", "int64", "uint32", "uint64",
		"sint32", "sint64", "fixed32", "fixed64", "sfixed32", "sfixed64",
		// the shape-driven rows
		"optional <scalar>", "repeated <scalar>", "string <x>_id",
		"enum E (same package)", "repeated enum E",
		"google.protobuf.Timestamp", "nested message (same package)",
		"map<K, scalar>",
	} {
		if _, ok := fieldTypes[want]; !ok {
			t.Errorf("field types missing %q", want)
		}
	}
	// Spot-check that the SQL is the real emitted column, not a label:
	// these are the exact strings the birth migration writes.
	for proto, wantSQL := range map[string]string{
		"string": "TEXT NOT NULL DEFAULT ''",
		"int64":  "BIGINT NOT NULL DEFAULT 0",
		"bool":   "BOOLEAN NOT NULL DEFAULT FALSE",
		"bytes":  `BYTEA NOT NULL DEFAULT '\x'`,
	} {
		if got := fieldTypes[proto].SQLType; got != wantSQL {
			t.Errorf("field type %q sql_type = %q, want the renderer's %q", proto, got, wantSQL)
		}
	}

	// Validate rules present with computed effects.
	if len(spec.ValidateRules) == 0 {
		t.Fatal("no validate rules emitted")
	}
	byRule := map[string]ValidateRuleSpec{}
	for _, r := range spec.ValidateRules {
		byRule[r.Rule] = r
	}
	if got := byRule["gte"].DBEffect; got != "CHECK (value >= 1)" {
		t.Errorf("gte db_effect = %q, want the SQLChecks projection", got)
	}
	if got := byRule["gte"].ZodEffect; got != ".gte(1)" {
		t.Errorf("gte zod_effect = %q, want the ZodChain projection", got)
	}
	if byRule["email"].DBEffect == "" || byRule["email"].ZodEffect == "" {
		t.Errorf("email rule has empty effects: %+v", byRule["email"])
	}
	for _, r := range spec.ValidateRules {
		if r.WireEffect == "" {
			t.Errorf("rule %q has empty wire_effect", r.Rule)
		}
		if r.Example == "" || len(r.AppliesTo) == 0 {
			t.Errorf("rule %q is missing its example/applies_to: %+v", r.Rule, r)
		}
	}

	// Method options include auth_required (and all five), each documented.
	methodNames := map[string]OptionSpec{}
	for _, o := range spec.MethodOptions {
		methodNames[o.Name] = o
	}
	if _, ok := methodNames["auth_required"]; !ok {
		t.Error("method_options missing auth_required")
	}
	for _, want := range []string{"auth_required", "idempotent", "timeout", "idempotency_key", "errors"} {
		o, ok := methodNames[want]
		if !ok {
			t.Errorf("method_options missing %q", want)
			continue
		}
		if o.Effect == "" || o.Type == "" {
			t.Errorf("method option %q incomplete: %+v", want, o)
		}
	}
	if got := methodNames["auth_required"].Type; got != "optional bool" {
		t.Errorf("auth_required type = %q, want %q", got, "optional bool")
	}
	if got := methodNames["errors"].Type; got != "repeated string" {
		t.Errorf("errors type = %q, want %q", got, "repeated string")
	}

	// Service options present and documented.
	if len(spec.ServiceOptions) == 0 {
		t.Error("service_options empty")
	}
	svcNames := map[string]bool{}
	for _, o := range spec.ServiceOptions {
		svcNames[o.Name] = true
	}
	for _, want := range []string{"name", "version", "description", "visibility"} {
		if !svcNames[want] {
			t.Errorf("service_options missing %q", want)
		}
	}
}

func TestAnnotations_KindFilters(t *testing.T) {
	// entity → only entity-level markers.
	entity := dumpJSON(t, "entity")
	if len(entity.FieldTypes) != 0 || len(entity.ValidateRules) != 0 ||
		len(entity.MethodOptions) != 0 || len(entity.ServiceOptions) != 0 {
		t.Errorf("--kind entity leaked non-marker sections: %+v", entity)
	}
	if len(entity.Markers) == 0 {
		t.Fatal("--kind entity returned no markers")
	}
	for _, m := range entity.Markers {
		if m.AppliesTo != "entity" {
			t.Errorf("--kind entity returned a %s marker: %s", m.AppliesTo, m.Name)
		}
	}

	// field → field markers + field_types + validate_rules, no options.
	field := dumpJSON(t, "field")
	if len(field.FieldTypes) == 0 || len(field.ValidateRules) == 0 {
		t.Errorf("--kind field missing field_types/validate_rules: %+v", field)
	}
	if len(field.MethodOptions) != 0 || len(field.ServiceOptions) != 0 {
		t.Errorf("--kind field leaked options")
	}
	for _, m := range field.Markers {
		if m.AppliesTo != "field" {
			t.Errorf("--kind field returned a %s marker: %s", m.AppliesTo, m.Name)
		}
	}

	// service → only service_options.
	service := dumpJSON(t, "service")
	if len(service.ServiceOptions) == 0 {
		t.Error("--kind service returned no service_options")
	}
	if len(service.Markers) != 0 || len(service.FieldTypes) != 0 ||
		len(service.ValidateRules) != 0 || len(service.MethodOptions) != 0 {
		t.Errorf("--kind service leaked other sections: %+v", service)
	}

	// method → method_options plus the rpc-level comment marker.
	method := dumpJSON(t, "method")
	if len(method.MethodOptions) == 0 {
		t.Error("--kind method returned no method_options")
	}
	if len(method.Markers) != 1 || method.Markers[0].Name != "forge:mutation" {
		t.Errorf("--kind method must carry the rpc-level marker: %+v", method.Markers)
	}
	if len(method.FieldTypes) != 0 ||
		len(method.ValidateRules) != 0 || len(method.ServiceOptions) != 0 {
		t.Errorf("--kind method leaked other sections: %+v", method)
	}

	// bogus → error.
	if err := runAnnotations(&bytes.Buffer{}, "bogus", true); err == nil {
		t.Error("--kind bogus should error")
	}
}

// TestAnnotations_MarkerNamesMatchRecognizers pins every marker name in the
// dump against the REAL recognizers (the raw proto scanner for the entity /
// field markers, the secret field regex for forge:secret), so renaming a
// marker in forge breaks this test rather than silently drifting the dump.
func TestAnnotations_MarkerNamesMatchRecognizers(t *testing.T) {
	dir := t.TempDir()
	proto := `syntax = "proto3";
package services.demo.v1;

// forge:entity
message WidgetEntity {
  string id = 1;
  string status = 2; // forge:server-set
  string api_token = 3; // forge:secret
}

// forge:soft-delete
message NoteEntity {
  string id = 1;
}

// forge:append-only
message LedgerEntity {
  string id = 1;
}
`
	if err := os.WriteFile(filepath.Join(dir, "demo.proto"), []byte(proto), 0o644); err != nil {
		t.Fatal(err)
	}
	scan, err := codegen.ScanRawProtoDir(dir)
	if err != nil {
		t.Fatalf("ScanRawProtoDir: %v", err)
	}

	if m, ok := scan.MessageByName("WidgetEntity"); !ok || !m.Marked {
		t.Error("forge:entity not recognized by the raw scanner")
	}
	if m, ok := scan.MessageByName("NoteEntity"); !ok || !m.SoftDeleteMarked {
		t.Error("forge:soft-delete not recognized by the raw scanner")
	}
	if m, ok := scan.MessageByName("LedgerEntity"); !ok || !m.AppendOnly {
		t.Error("forge:append-only not recognized by the raw scanner")
	}
	widget, _ := scan.MessageByName("WidgetEntity")
	serverSetSeen := false
	for _, f := range widget.Fields {
		if f.Name == "status" && f.ServerSet {
			serverSetSeen = true
		}
	}
	if !serverSetSeen {
		t.Error("forge:server-set not recognized on the field")
	}
	// buf strips the `//` before protogen sees the comment, so the secret
	// regex matches the comment text WITHOUT slashes — mirror that form here.
	if !secretFieldMarkerRE.MatchString(" forge:secret\n") {
		t.Error("forge:secret regex no longer matches the canonical marker")
	}

	// The dump lists exactly the five recognized names.
	got := map[string]bool{}
	for _, m := range markerSpecs() {
		got[m.Name] = true
	}
	for _, want := range []string{
		"forge:entity", "forge:soft-delete", "forge:append-only",
		"forge:server-set", "forge:secret", "forge:mutation",
	} {
		if !got[want] {
			t.Errorf("markerSpecs missing recognized marker %q", want)
		}
	}
	if len(got) != 6 {
		t.Errorf("expected 6 marker specs, got %d", len(got))
	}

	// forge:mutation is recognized by the hook generator, not the entity
	// scanner — check it against ITS recognizer, or the catalog could
	// advertise a marker nothing reads.
	if !mutationMarkerRE.MatchString("// forge:mutation") {
		t.Error("forge:mutation regex no longer matches the canonical marker")
	}
	if marked := mutationMarkedMethods(filepath.Join(dir, "hooks.proto")); marked["FindAndHoldSeat"] {
		t.Error("mutationMarkedMethods invented a marker for a file that does not exist")
	}
	hooksProto := `syntax = "proto3";
package services.demo.v1;
service DemoService {
  // forge:mutation
  rpc FindAndHoldSeat(Req) returns (Resp);
  rpc FindSeat(Req) returns (Resp);
}
`
	if err := os.WriteFile(filepath.Join(dir, "hooks.proto"), []byte(hooksProto), 0o644); err != nil {
		t.Fatal(err)
	}
	marked := mutationMarkedMethods(filepath.Join(dir, "hooks.proto"))
	if !marked["FindAndHoldSeat"] {
		t.Error("forge:mutation not recognized on the rpc it labels")
	}
	if marked["FindSeat"] {
		t.Error("forge:mutation leaked onto an unmarked rpc")
	}
}

// TestAnnotations_ValidateRulesAreDiscoveredNotTranscribed pins the property
// the `db` skill's "it cannot drift" claim rests on: the dump's rule set is
// whatever forge's own recognizer accepts, not a list someone maintains.
//
// The regression it guards: `string.len = 3` really does project to
// `CHECK (char_length(col) = 3)`, but the hand-written table omitted it, so
// the "authoritative" dump told agents a working rule did not exist.
func TestAnnotations_ValidateRulesAreDiscoveredNotTranscribed(t *testing.T) {
	// The candidate universe has to reach protovalidate's whole rule surface;
	// a walk that silently found nothing would make the dump empty-but-green.
	cands := validateRuleCandidates()
	if len(cands) < 100 {
		t.Fatalf("only %d rule candidates discovered from the buf.validate descriptors — the walk has broken", len(cands))
	}
	byPath := map[string]validateRuleCandidate{}
	for _, c := range cands {
		byPath[c.path] = c
	}
	for _, want := range []string{"string.len", "string.min_len", "int64.gte", "string.const", "string.uuid", "required"} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("candidate universe is missing %q — probing cannot classify what it never sees", want)
		}
	}

	rules := map[string]ValidateRuleSpec{}
	for _, r := range validateRuleSpecs() {
		rules[r.Rule] = r
	}

	// Rules forge really projects must be listed — verified against the real
	// projection functions rather than against a copy of this expectation.
	for _, want := range []string{"gte", "gt", "lte", "lt", "min_len", "max_len", "len", "pattern", "email", "required"} {
		spec, ok := rules[want]
		if !ok {
			t.Errorf("validate rules omit %q, which codegen.ParseRawValidateOptions accepts", want)
			continue
		}
		fc := codegen.ParseRawValidateOptions(spec.Example)
		if fc == nil {
			t.Errorf("rule %q advertises example %q, which forge's recognizer rejects", want, spec.Example)
			continue
		}
		if got := strings.Join(fc.SQLChecks(validateSampleColumn, spec.AppliesTo[0]), " "); got != spec.DBEffect {
			t.Errorf("rule %q db_effect = %q, but re-projecting its own example gives %q", want, spec.DBEffect, got)
		}
	}
	if got := rules["len"].DBEffect; got != "CHECK (char_length(value) = 1)" {
		t.Errorf("string.len db_effect = %q, want the exact-length CHECK", got)
	}
	if got := rules["min_len"].AppliesTo; len(got) != 1 || got[0] != "string" {
		t.Errorf("min_len applies_to = %v, want just [string]", got)
	}
	if got := rules["gte"].AppliesTo; len(got) < 8 || !slices.Contains(got, "int64") {
		t.Errorf("gte applies_to = %v, want every numeric proto kind", got)
	}

	// Rules protovalidate declares but forge does NOT project must stay out:
	// listing one would be the same class of lie in the other direction.
	for _, unprojected := range []string{"const", "uuid", "prefix", "in", "not_in", "defined_only"} {
		if spec, ok := rules[unprojected]; ok {
			t.Errorf("validate rules advertise %q (db %q), which forge projects nowhere", unprojected, spec.DBEffect)
		}
	}
	for _, r := range validateRuleSpecs() {
		if r.DBEffect == "" {
			t.Errorf("rule %q is listed with no db projection", r.Rule)
		}
	}
}

// TestAnnotations_OptionsFullyDocumented asserts every live (forge.v1.method)
// and (forge.v1.service) descriptor field carries effect + type prose, so a
// new proto option cannot ship undocumented in the dump.
func TestAnnotations_OptionsFullyDocumented(t *testing.T) {
	check := func(kind string, specs []OptionSpec) {
		if len(specs) == 0 {
			t.Fatalf("%s: no options emitted", kind)
		}
		for _, o := range specs {
			if o.Effect == "" {
				t.Errorf("%s option %q has no effect prose", kind, o.Name)
			}
			if o.Type == "" {
				t.Errorf("%s option %q has no type", kind, o.Name)
			}
		}
	}
	check("method", methodOptionSpecs())
	check("service", serviceOptionSpecs())
}
