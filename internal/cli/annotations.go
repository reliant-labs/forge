// Package cli — `forge project annotations`: dump forge's authoritative
// entity-authoring annotation spec as structured data.
//
// The command answers "what annotations does forge understand, and what do
// they do?" from forge's OWN definitions rather than from prose that drifts:
//
//   - markers        the `// forge:*` comment markers the proto scanner reads
//     (forge:entity, forge:soft-delete, forge:append-only,
//     forge:server-set, forge:secret).
//   - field_types    the proto→column mapping an entity birth applies,
//     iterated from the real scaffold.ProtoSQLMappings (whose
//     scalar half is projected from the renderer's scalarSQL).
//   - validate_rules the projected protovalidate subset, DISCOVERED by
//     probing forge's own recognizer (codegen.ParseRawValidateOptions)
//     with every rule protovalidate itself declares, and whose db/zod
//     effects are COMPUTED by forge's own projection functions
//     (codegen.FieldConstraints.SQLChecks / .ZodChain).
//   - service_options / method_options
//     the (forge.v1.service) / (forge.v1.method) proto options,
//     enumerated from the compiled forgepb descriptors.
//
// Everything sourced from an iterable structure is derived, not transcribed,
// so the dump tracks the code. The genuinely descriptive fields (a marker's
// effect/example, an option's default) are thin literals whose source is
// cited at the definition site; a package test pins their names against the
// real recognizers/descriptors so a rename can never silently drift the dump.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	validatepb "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/reliant-labs/forge/internal/codegen"
	entityscaffold "github.com/reliant-labs/forge/internal/scaffold"
	forgev1 "github.com/reliant-labs/forge/pkg/forgepb"
)

// MarkerSpec describes one `// forge:*` comment marker. AppliesTo is the
// descriptor level it attaches to ("entity" for the message-level markers,
// "field" for the field-level ones).
type MarkerSpec struct {
	Name      string `json:"name"`
	AppliesTo string `json:"applies_to"`
	Effect    string `json:"effect"`
	Placement string `json:"placement"`
	Example   string `json:"example"`
}

// FieldTypeSpec is one row of the proto→column mapping an entity birth
// applies: the field as declared in the message, the column emitted for it,
// and the qualification where the column is not simply the scalar type.
type FieldTypeSpec struct {
	ProtoType string `json:"proto_type"`
	SQLType   string `json:"sql_type"`
	Notes     string `json:"notes"`
}

// ValidateRuleSpec is one projected protovalidate rule and its three
// enforcement points. Rule is the leaf rule name as protovalidate declares it
// (the name the author types: `min_len`, `len`, `gte`), AppliesTo the proto
// scalar kinds the projection actually fires on, and Example the exact option
// text that produced DBEffect/ZodEffect. Those two are SAMPLE projections
// computed by forge's own FieldConstraints methods (column named "value");
// the real literal follows the bound the author declared.
type ValidateRuleSpec struct {
	Rule       string   `json:"rule"`
	AppliesTo  []string `json:"applies_to"`
	Example    string   `json:"example"`
	DBEffect   string   `json:"db_effect"`
	ZodEffect  string   `json:"zod_effect"`
	WireEffect string   `json:"wire_effect"`
}

// OptionSpec is one proto option field on (forge.v1.service) or
// (forge.v1.method). Name/Type are read from the compiled descriptor.
type OptionSpec struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Effect  string `json:"effect"`
	Default string `json:"default"`
}

// AnnotationsSpec is the whole dump. Sections are omitempty so a `--kind`
// filter yields only the selected section(s).
type AnnotationsSpec struct {
	Markers        []MarkerSpec       `json:"markers,omitempty"`
	FieldTypes     []FieldTypeSpec    `json:"field_types,omitempty"`
	ValidateRules  []ValidateRuleSpec `json:"validate_rules,omitempty"`
	ServiceOptions []OptionSpec       `json:"service_options,omitempty"`
	MethodOptions  []OptionSpec       `json:"method_options,omitempty"`
}

// newAnnotationsCmd builds `forge project annotations`. The spec is forge's
// built-in vocabulary, not project state, so the command reads no project
// files and runs anywhere.
func newAnnotationsCmd() *cobra.Command {
	var (
		kind   string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "annotations",
		Short: "Dump forge's authoritative entity-authoring annotation spec",
		Long: `Dump forge's authoritative entity-authoring annotation spec.

Emits the vocabulary forge itself understands, sourced from forge's own
definitions so it cannot drift from behavior:

  markers          the // forge:* comment markers the proto scanner reads
  field_types      the proto->column mapping an entity birth applies
  validate_rules   the projected protovalidate subset and its db/zod/wire effects
  service_options  the (forge.v1.service) options
  method_options   the (forge.v1.method) options

Use --kind to limit the dump to one annotation level, and --json for a
machine-readable dump that tools can query instead of re-deriving the spec.

Examples:
  forge project annotations --json
  forge project annotations --kind entity --json
  forge project annotations --kind method`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAnnotations(cmd.OutOrStdout(), kind, asJSON)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "limit to one annotation kind: entity, field, service, or method (default: all)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the spec as JSON")
	return cmd
}

func runAnnotations(w io.Writer, kind string, asJSON bool) error {
	spec, err := buildAnnotationsSpec(kind)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(spec)
	}
	return writeAnnotationsText(w, spec)
}

// buildAnnotationsSpec assembles the full spec and applies the --kind filter.
// Empty kind ⇒ everything; an unknown kind is a user error.
func buildAnnotationsSpec(kind string) (AnnotationsSpec, error) {
	all := AnnotationsSpec{
		Markers:        markerSpecs(),
		FieldTypes:     fieldTypeSpecs(),
		ValidateRules:  validateRuleSpecs(),
		ServiceOptions: serviceOptionSpecs(),
		MethodOptions:  methodOptionSpecs(),
	}
	switch kind {
	case "":
		return all, nil
	case "entity":
		// Only the message-level markers attach to an entity.
		return AnnotationsSpec{Markers: filterMarkers(all.Markers, "entity")}, nil
	case "field":
		// Field-level markers plus the per-field authoring vocabulary.
		return AnnotationsSpec{
			Markers:       filterMarkers(all.Markers, "field"),
			FieldTypes:    all.FieldTypes,
			ValidateRules: all.ValidateRules,
		}, nil
	case "service":
		return AnnotationsSpec{ServiceOptions: all.ServiceOptions}, nil
	case "method":
		// Method options plus the one comment marker that attaches to an rpc.
		return AnnotationsSpec{
			Markers:       filterMarkers(all.Markers, "method"),
			MethodOptions: all.MethodOptions,
		}, nil
	default:
		return AnnotationsSpec{}, fmt.Errorf("unknown --kind %q (want entity, field, service, or method)", kind)
	}
}

func filterMarkers(in []MarkerSpec, appliesTo string) []MarkerSpec {
	var out []MarkerSpec
	for _, m := range in {
		if m.AppliesTo == appliesTo {
			out = append(out, m)
		}
	}
	return out
}

// markerSpecs is the one thin literal projection in this file. The marker
// NAMES and their placement grammar are owned by the regexes in
// internal/codegen/proto_rawscan.go (entityMarkerRE, softDeleteMarkerRE,
// appendOnlyMarkerRE, serverSetFieldMarkerRE) and, for forge:secret, by
// secretFieldMarkerRE in forge_descriptor.go. The effects are the birth/read
// behaviors in internal/scaffold/entityproto.go and
// internal/codegen/crud_convert.go. TestAnnotations_MarkerNames pins every
// name here against those real recognizers so a rename cannot drift the dump.
func markerSpecs() []MarkerSpec {
	return []MarkerSpec{
		{
			Name:      "forge:entity",
			AppliesTo: "entity",
			Effect:    "Marks a top-level proto message as a database entity: forge births the create-table migration and the CRUD quintet (Create/Get/Update/Delete/List) at scaffold time, and DECLARES the managed fields it doesn't already have (id, created_at, updated_at) on the message itself — appended at free field numbers, never renumbering yours.",
			Placement: "full-line leading comment immediately above `message X {`",
			Example:   "// forge:entity\nmessage Order { ... }",
		},
		{
			Name:      "forge:soft-delete",
			AppliesTo: "entity",
			Effect:    "Entity is born with a nullable deleted_at column, declared on the message alongside the other managed fields; Delete becomes an UPDATE and reads filter `deleted_at IS NULL`. Also tablizes the message (implies forge:entity).",
			Placement: "full-line leading comment immediately above `message X {`",
			Example:   "// forge:soft-delete\nmessage Note { ... }",
		},
		{
			Name:      "forge:append-only",
			AppliesTo: "entity",
			Effect:    "Entity is born WITHOUT Update/Delete RPCs and with a DB trigger that rejects every UPDATE/DELETE (an immutable ledger). Also tablizes the message (implies forge:entity).",
			Placement: "full-line leading comment immediately above `message X {`",
			Example:   "// forge:append-only\nmessage AuditEvent { ... }",
		},
		{
			Name:      "forge:server-set",
			AppliesTo: "field",
			Effect:    "Field is kept on the entity message and Get/List responses but OMITTED from the born Create/Update request messages — a server-authoritative value the client must not set.",
			Placement: "leading full-line comment above the field, or a trailing comment after the field's terminating `;` — inline `[...]` options, including multi-line braced protovalidate values, sit between them harmlessly. A marker no field consumes REFUSES the birth, naming file and line.",
			Example:   "int64 unit_price_cents = 4 [(buf.validate.field).int64.gte = 0]; // forge:server-set",
		},
		{
			Name:      "forge:mutation",
			AppliesTo: "method",
			Effect:    "Forces the generated React Query hook for this rpc to be a useMutation. Classification is otherwise by leading verb — Get/List/Search/Find/Check/Has/Is/Count/Exists (whole-word) are reads, EVERYTHING else is a mutation — so this is needed only for an imperative rpc that happens to open with a read word (FindAndHoldSeat, IssueRefund). A write generated as useQuery re-fires on every component remount. Unlike the entity/field markers this one is read on every `forge generate`, not just at birth.",
			Placement: "leading full-line comment above the `rpc` line, or a trailing comment on it",
			Example:   "// forge:mutation — holds inventory, so it must never be a cached read.\nrpc FindAndHoldSeat(FindAndHoldSeatRequest) returns (FindAndHoldSeatResponse);",
		},
		{
			Name:      "forge:secret",
			AppliesTo: "field",
			Effect:    "Column stays real and writable on Create/Update but is stripped from read responses — the generated <entity>ToProto never packs it, so Get/List omit it and it is excluded from the list search span.",
			Placement: "leading full-line comment above the field, or a trailing comment on the field line",
			Example:   "string api_token = 2; // forge:secret",
		},
	}
}

// fieldTypeSpecs iterates the authoritative entity-birth mapping —
// scaffold.ProtoSQLMappings, whose scalar half is projected straight from the
// renderer's own scalarSQL — so the dump tracks what a birth actually emits.
func fieldTypeSpecs() []FieldTypeSpec {
	var out []FieldTypeSpec
	for _, m := range entityscaffold.ProtoSQLMappings() {
		out = append(out, FieldTypeSpec{
			ProtoType: m.Proto,
			SQLType:   m.SQL,
			Notes:     m.Notes,
		})
	}
	return out
}

// ─── validate rules: discovered, never transcribed ──────────────────────

// validateRuleWireEffect is the one sentence that is the same for every rule.
const validateRuleWireEffect = "Enforced at the wire by the protovalidate interceptor, which reads the same (buf.validate.field) option off the compiled descriptor."

// validateSampleColumn is the column name the sample DB projection uses.
const validateSampleColumn = "value"

// validateRuleSpecs DISCOVERS the protovalidate rules forge projects instead
// of listing them. Two derived halves, no hand-maintained rule names:
//
//   - The candidate universe is every rule protovalidate itself declares:
//     the top-level scalar fields of buf.validate.FieldRules (`required`)
//     plus every field of every per-type rules message reachable through
//     FieldRules' `type` oneof (StringRules.len, Int64Rules.gte, …). Truth
//     source: the compiled buf.validate descriptors.
//   - The verdict is forge's OWN recognizer: each candidate is spelled as
//     the author would write it, run through codegen.ParseRawValidateOptions,
//     and projected with codegen.FieldConstraints.SQLChecks / .ZodChain. A
//     candidate appears in the dump only when that projection is non-empty.
//
// So a rule forge starts (or stops) projecting changes this dump with no edit
// here — which is what lets the skills call the dump authoritative. `len` went
// missing from the old transcribed list for exactly this reason.
//
// Rules protovalidate supports but forge does not PROJECT (const, uuid,
// in/not_in, CEL, …) are absent by construction; they are still enforced at
// the wire, which writeAnnotationsText and the JSON wire_effect both say.
func validateRuleSpecs() []ValidateRuleSpec {
	var (
		out   []ValidateRuleSpec
		index = map[string]int{} // leaf rule name → position in out
	)
	for _, cand := range validateRuleCandidates() {
		kinds, dbEffect, zodEffect := probeValidateRule(cand)
		if len(kinds) == 0 {
			continue
		}
		if at, seen := index[cand.rule]; seen {
			out[at].AppliesTo = append(out[at].AppliesTo, kinds...)
			continue
		}
		index[cand.rule] = len(out)
		out = append(out, ValidateRuleSpec{
			Rule:       cand.rule,
			AppliesTo:  kinds,
			Example:    cand.option(),
			DBEffect:   dbEffect,
			ZodEffect:  zodEffect,
			WireEffect: validateRuleWireEffect,
		})
	}
	return out
}

// validateRuleCandidate is one rule spelling to probe: the dotted option path
// the author writes, a sample value of the rule's declared proto type, and the
// proto scalar kinds the rule could apply to (one for a per-type rule, all of
// them for a type-independent FieldRules rule like `required`).
type validateRuleCandidate struct {
	rule   string   // leaf rule name, e.g. "min_len"
	path   string   // dotted option path, e.g. "string.min_len" or "required"
	sample string   // value literal, already quoted when the rule takes a string
	kinds  []string // proto scalar kinds to project against
}

func (c validateRuleCandidate) option() string {
	return fmt.Sprintf("[(buf.validate.field).%s = %s]", c.path, c.sample)
}

// validateRuleCandidates walks the buf.validate descriptors and returns every
// rule spelling worth probing, in declaration order (so the dump's order is
// protovalidate's own, not an author's opinion).
func validateRuleCandidates() []validateRuleCandidate {
	fieldRules := (&validatepb.FieldRules{}).ProtoReflect().Descriptor()

	// Pass 1: the `type` oneof — one rules message per proto scalar kind.
	// The oneof field's NAME is the proto type token the author writes
	// (`string` in `(buf.validate.field).string.min_len`).
	var (
		typed     []validateRuleCandidate
		allKinds  []string
		typeOneof = fieldRules.Oneofs().ByName("type")
	)
	if typeOneof != nil {
		for i := 0; i < typeOneof.Fields().Len(); i++ {
			fd := typeOneof.Fields().Get(i)
			rules := fd.Message()
			if rules == nil {
				continue
			}
			token := string(fd.Name())
			allKinds = append(allKinds, token)
			for j := 0; j < rules.Fields().Len(); j++ {
				rf := rules.Fields().Get(j)
				sample, ok := validateSampleValue(rf)
				if !ok {
					continue
				}
				typed = append(typed, validateRuleCandidate{
					rule:   string(rf.Name()),
					path:   token + "." + string(rf.Name()),
					sample: sample,
					kinds:  []string{token},
				})
			}
		}
	}

	// Pass 2: the type-independent rules that sit directly on FieldRules
	// (`required`). They carry no type token and could fire on any kind.
	var plain []validateRuleCandidate
	for i := 0; i < fieldRules.Fields().Len(); i++ {
		fd := fieldRules.Fields().Get(i)
		if fd.ContainingOneof() != nil {
			continue // handled above
		}
		sample, ok := validateSampleValue(fd)
		if !ok {
			continue
		}
		plain = append(plain, validateRuleCandidate{
			rule:   string(fd.Name()),
			path:   string(fd.Name()),
			sample: sample,
			kinds:  allKinds,
		})
	}
	return append(plain, typed...)
}

// validateSampleValue renders a probe value for a rule field from its DECLARED
// proto type, so no rule name is special-cased. Rules whose value is a message
// or a list (duration bounds, in/not_in, CEL) are not probeable through the
// scalar option grammar and are skipped — forge projects none of them, and a
// skipped candidate can only ever under-report, never invent.
func validateSampleValue(fd protoreflect.FieldDescriptor) (string, bool) {
	if fd.IsList() || fd.IsMap() {
		return "", false
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return "true", true
	case protoreflect.StringKind, protoreflect.BytesKind:
		return `"abc"`, true
	case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Uint32Kind,
		protoreflect.Uint64Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind,
		protoreflect.Fixed32Kind, protoreflect.Fixed64Kind, protoreflect.Sfixed32Kind,
		protoreflect.Sfixed64Kind, protoreflect.FloatKind, protoreflect.DoubleKind:
		return "1", true
	default:
		return "", false
	}
}

// probeValidateRule runs one candidate through forge's real recognizer and
// projections. It returns the proto scalar kinds the projection actually fires
// on plus the sample db/zod effects for the first such kind. Empty kinds means
// forge does not project this rule.
//
// SQLChecks is the kind-aware half (it switches on the proto scalar kind);
// ZodChain only knows "number input vs string input", so it would happily
// answer .min(1) for a bool or bytes field forge never puts a length check on.
// A kind therefore counts as projected when the MIGRATION projection fires,
// and the zod chain is reported alongside it.
func probeValidateRule(c validateRuleCandidate) (kinds []string, dbEffect, zodEffect string) {
	fc := codegen.ParseRawValidateOptions(c.option())
	if fc == nil {
		return nil, "", ""
	}
	for _, kind := range c.kinds {
		db := strings.Join(fc.SQLChecks(validateSampleColumn, kind), " ")
		if db == "" {
			continue
		}
		if len(kinds) == 0 {
			dbEffect, zodEffect = db, fc.ZodChain(zodFormFor(kind))
		}
		kinds = append(kinds, kind)
	}
	return kinds, dbEffect, zodEffect
}

// numericValidateKinds is the set of proto type tokens whose rules take
// numeric bounds, derived from the buf.validate descriptors: a token is
// numeric when its own rules message declares a numeric `const`. That is a
// fact about the proto type system, not a list of forge's rule names, so it
// cannot drift the way a transcribed rule table can.
var numericValidateKinds = func() map[string]bool {
	out := map[string]bool{}
	fieldRules := (&validatepb.FieldRules{}).ProtoReflect().Descriptor()
	typeOneof := fieldRules.Oneofs().ByName("type")
	if typeOneof == nil {
		return out
	}
	for i := 0; i < typeOneof.Fields().Len(); i++ {
		fd := typeOneof.Fields().Get(i)
		rules := fd.Message()
		if rules == nil {
			continue
		}
		constFD := rules.Fields().ByName("const")
		if constFD == nil || constFD.IsList() {
			continue
		}
		switch constFD.Kind() {
		case protoreflect.Int32Kind, protoreflect.Int64Kind, protoreflect.Uint32Kind,
			protoreflect.Uint64Kind, protoreflect.Sint32Kind, protoreflect.Sint64Kind,
			protoreflect.Fixed32Kind, protoreflect.Fixed64Kind, protoreflect.Sfixed32Kind,
			protoreflect.Sfixed64Kind, protoreflect.FloatKind, protoreflect.DoubleKind:
			out[string(fd.Name())] = true
		}
	}
	return out
}()

// zodFormFor maps a proto scalar kind to the zod base builder the frontend
// emitter uses, which is the argument ZodChain expects.
func zodFormFor(kind string) string {
	if numericValidateKinds[kind] {
		return "number"
	}
	return "string"
}

// serviceOptionSpecs / methodOptionSpecs enumerate the (forge.v1.service) /
// (forge.v1.method) option fields straight off the compiled forgepb
// descriptors — names and proto types cannot drift from the .proto. Reserved
// field numbers are absent from the descriptor, so they are excluded for free.
func serviceOptionSpecs() []OptionSpec {
	return optionSpecsFromDescriptor(
		(&forgev1.ServiceOptions{}).ProtoReflect().Descriptor().Fields(), serviceOptionMeta)
}

func methodOptionSpecs() []OptionSpec {
	return optionSpecsFromDescriptor(
		(&forgev1.MethodOptions{}).ProtoReflect().Descriptor().Fields(), methodOptionMeta)
}

// optionMeta is the descriptive half of an option — effect + default — that
// has no iterable source; a package test asserts every live descriptor field
// has an entry so a new option cannot ship undocumented.
type optionMeta struct{ effect, deflt string }

// Source: internal/assets/proto/forge/v1/forge.proto (MethodOptions) +
// internal/cli/forge_descriptor.go (applyMethodOptions defaults).
var methodOptionMeta = map[string]optionMeta{
	"auth_required":   {"Whether the method requires authentication; overrides the service AuthConfig default. Live metadata in forge map/graph.", "true (fail-closed: auth required when unset)"},
	"idempotent":      {"Marks the method safe to retry without side effects.", "false"},
	"timeout":         {"Server-side per-method timeout; zero means use the service default.", "0s (service default)"},
	"idempotency_key": {"Convention marker advising callers to supply an idempotency key; forge does not enforce or inspect it.", "false"},
	"errors":          {"Declared Connect/gRPC error codes the method may return (e.g. NotFound, InvalidArgument); informational, surfaced to handler authors.", "[] (no declared errors)"},
}

// Source: internal/assets/proto/forge/v1/forge.proto (ServiceOptions) +
// internal/generator/service_gen.go (the scaffolded option block).
var serviceOptionMeta = map[string]optionMeta{
	"name":         {"Human-readable service name used in codegen and logging; scaffolded as <Handler>Service at birth.", ""},
	"version":      {"API version string (e.g. \"1.0.0\"); scaffolded at birth.", ""},
	"description":  {"Brief description of the service's purpose; scaffolded at birth.", ""},
	"visibility":   {"How the service is exposed: SERVICE_VISIBILITY_API (Connect/HTTP gateway) or SERVICE_VISIBILITY_INTERNAL (inter-service only).", "SERVICE_VISIBILITY_UNSPECIFIED (defaults to API)"},
	"dependencies": {"Other services this service depends on.", "[]"},
	"auth":         {"Service-wide authentication defaults (AuthConfig.auth_required / auth_provider); individual methods override via (forge.v1.method).auth_required.", ""},
}

func optionSpecsFromDescriptor(fields protoreflect.FieldDescriptors, meta map[string]optionMeta) []OptionSpec {
	out := make([]OptionSpec, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		name := string(fd.Name())
		m := meta[name]
		out = append(out, OptionSpec{
			Name:    name,
			Type:    protoOptionType(fd),
			Effect:  m.effect,
			Default: m.deflt,
		})
	}
	return out
}

// protoOptionType renders a descriptor field's proto type readably: message
// fields resolve to the referenced message's full name (Duration/AuthConfig),
// list fields carry a "repeated " prefix, proto3-optional a "optional " one.
func protoOptionType(fd protoreflect.FieldDescriptor) string {
	base := fd.Kind().String()
	if m := fd.Message(); m != nil {
		base = string(m.FullName())
	}
	switch {
	case fd.IsList():
		return "repeated " + base
	case fd.HasOptionalKeyword():
		return "optional " + base
	default:
		return base
	}
}

// writeAnnotationsText renders the non-JSON view — a readable summary of the
// same spec, so the command is useful at a terminal as well as to tooling.
func writeAnnotationsText(w io.Writer, spec AnnotationsSpec) error {
	p := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	if len(spec.Markers) > 0 {
		if err := p("MARKERS\n"); err != nil {
			return err
		}
		for _, m := range spec.Markers {
			if err := p("  %-18s (%s) %s\n", m.Name, m.AppliesTo, m.Effect); err != nil {
				return err
			}
		}
	}
	if len(spec.FieldTypes) > 0 {
		if err := p("\nFIELD TYPES — how a proto field is born as a column\n" +
			"  Applied once, at birth, by `forge scaffold` / `forge scaffold entity\n" +
			"  --from-proto`. The migration is yours from that moment: adjust any\n" +
			"  column below freely, and evolve with a new migration.\n"); err != nil {
			return err
		}
		for _, ft := range spec.FieldTypes {
			if err := p("  %-30s %-46s %s\n", ft.ProtoType, ft.SQLType, ft.Notes); err != nil {
				return err
			}
		}
	}
	if len(spec.ValidateRules) > 0 {
		if err := p("\nVALIDATE RULES — the (buf.validate.field) rules forge PROJECTS to db + zod\n" +
			"  Every other protovalidate rule (const, uuid, in/not_in, CEL, …) is still\n" +
			"  enforced on the wire by the protovalidate interceptor; it is simply not\n" +
			"  projected onto the migration or the generated form.\n"); err != nil {
			return err
		}
		for _, r := range spec.ValidateRules {
			if err := p("\n  %s   (%s)\n", r.Rule, strings.Join(r.AppliesTo, " ")); err != nil {
				return err
			}
			if err := p("    e.g. %s\n    db   %s\n    zod  %s\n", r.Example, r.DBEffect, r.ZodEffect); err != nil {
				return err
			}
		}
	}
	if err := writeOptionsText(w, "SERVICE OPTIONS (forge.v1.service)", spec.ServiceOptions); err != nil {
		return err
	}
	return writeOptionsText(w, "METHOD OPTIONS (forge.v1.method)", spec.MethodOptions)
}

func writeOptionsText(w io.Writer, heading string, opts []OptionSpec) error {
	if len(opts) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\n%s\n", heading); err != nil {
		return err
	}
	for _, o := range opts {
		if _, err := fmt.Fprintf(w, "  %-16s %-28s %s\n", o.Name, o.Type, o.Effect); err != nil {
			return err
		}
	}
	return nil
}
