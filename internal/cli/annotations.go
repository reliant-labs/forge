// Package cli — `forge project annotations`: dump forge's authoritative
// entity-authoring annotation spec as structured data.
//
// The command answers "what annotations does forge understand, and what do
// they do?" from forge's OWN definitions rather than from prose that drifts:
//
//   - markers        the `// forge:*` comment markers the proto scanner reads
//     (forge:entity, forge:soft-delete, forge:append-only,
//     forge:read-only, forge:secret).
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
	"github.com/reliant-labs/forge/pkg/schemadef"
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
		Long: `Dump forge's authoritative annotation spec.

Emits the vocabulary forge itself understands, sourced from forge's own
definitions so it cannot drift from behavior:

  markers          the // forge:* comment markers, in proto AND Go source,
                   plus the forge:* COMMENT ON COLUMN/CONSTRAINT markers
  field_types      the proto->column mapping an entity birth applies
  validate_rules   the projected protovalidate subset and its db/zod/wire effects
  service_options  the (forge.v1.service) options
  method_options   the (forge.v1.method) options

Use --kind to limit the dump to one annotation level, and --json for a
machine-readable dump that tools can query instead of re-deriving the spec.
--kind column is the forge:* markers declared as a postgres catalog COMMENT
in a migration rather than a proto/Go comment (forge:immutable on a column,
forge:ref on a foreign-key constraint). --kind go is the wiring/observability
/contract vocabulary forge reads out of .go files (forge:optional-dep,
forge:constructor, forge:no-observe, forge:exclude-contract, ...) — the
markers you meet in scaffolded code.

Examples:
  forge project annotations --json
  forge project annotations --kind entity --json
  forge project annotations --kind column
  forge project annotations --kind go`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAnnotations(cmd.OutOrStdout(), kind, asJSON)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "limit to one annotation kind: entity, field, column, service, method, or go (default: all)")
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
	case "column":
		// Column-comment markers (forge:immutable, forge:ref) — declared as
		// postgres catalog COMMENTs, not proto comments, so they get their
		// own kind rather than polluting the entity/field views.
		return AnnotationsSpec{Markers: filterMarkers(all.Markers, "column")}, nil
	case "go":
		// The markers read out of .go source rather than out of proto —
		// the wiring/observability/contract vocabulary. These have no
		// proto counterpart, so they get their own kind rather than
		// polluting the entity/field/method views.
		return AnnotationsSpec{Markers: filterMarkers(all.Markers,
			"package", "contract", "constructor", "deps-field")}, nil
	default:
		return AnnotationsSpec{}, fmt.Errorf("unknown --kind %q (want entity, field, column, service, method, or go)", kind)
	}
}

func filterMarkers(in []MarkerSpec, appliesTo ...string) []MarkerSpec {
	want := make(map[string]bool, len(appliesTo))
	for _, a := range appliesTo {
		want[a] = true
	}
	var out []MarkerSpec
	for _, m := range in {
		if want[m.AppliesTo] {
			out = append(out, m)
		}
	}
	return out
}

// markerSpecs is the one thin literal projection in this file — the
// DESCRIPTIONS are literals; the marker NAMES are not.
//
// Proto marker names come from codegen.KnownProtoMarkers and column marker
// names from schemadef.KnownColumnMarkers, the same two registries the
// scanners spell their markers from and `forge lint`'s unknown-marker checks
// enforce, so this dump cannot advertise a vocabulary that differs from the
// one forge actually reads. The placement grammar behind each proto name is
// owned by the regexes those constants build (proto_rawscan.go's
// entityMarkerRE / softDeleteMarkerRE / appendOnlyMarkerRE /
// readOnlyFieldMarkerRE, forge_descriptor.go's secretFieldMarkerRE, and
// generate_frontend_hooks.go's mutationMarkerRE). The effects are the
// birth/read behaviors in internal/scaffold/entityproto.go and
// internal/codegen/crud_convert.go.
//
// Go-source marker names remain literals here: they have no registry,
// because each is read by a single recognizer in its own package rather than
// by a vocabulary shared across passes. TestAnnotations_MarkerNames pins
// every one of them against its real recognizer.
func markerSpecs() []MarkerSpec {
	return []MarkerSpec{
		{
			Name:      codegen.ProtoMarkerEntity,
			AppliesTo: "entity",
			Effect:    "Marks a top-level proto message as a database entity: forge births the create-table migration and the CRUD quintet (Create/Get/Update/Delete/List) at scaffold time, and DECLARES the managed fields it doesn't already have (id, created_at, updated_at) on the message itself — appended at free field numbers, never renumbering yours.",
			Placement: "full-line leading comment immediately above `message X {`",
			Example:   "// forge:entity\nmessage Order { ... }",
		},
		{
			Name:      codegen.ProtoMarkerSoftDelete,
			AppliesTo: "entity",
			Effect:    "Entity is born with a nullable deleted_at column, declared on the message alongside the other managed fields; Delete becomes an UPDATE and reads filter `deleted_at IS NULL`. Also tablizes the message (implies forge:entity).",
			Placement: "full-line leading comment immediately above `message X {`",
			Example:   "// forge:soft-delete\nmessage Note { ... }",
		},
		{
			Name:      codegen.ProtoMarkerAppendOnly,
			AppliesTo: "entity",
			Effect:    "Entity is born WITHOUT Update/Delete RPCs and with a DB trigger that rejects every UPDATE/DELETE (an immutable ledger). Also tablizes the message (implies forge:entity).",
			Placement: "full-line leading comment immediately above `message X {`",
			Example:   "// forge:append-only\nmessage AuditEvent { ... }",
		},
		{
			Name:      codegen.ProtoMarkerReadOnly,
			AppliesTo: "field",
			Effect:    "Field is kept on the entity message and Get/List responses but OMITTED from the born Create/Update request messages — readable, not client-writable. The write-side mirror of forge:secret. Nothing assigns the value for you: give the column a DB DEFAULT (or set it in your handler), or an insert lands the zero value. If the value is DERIVED rather than defaulted, use forge:computed instead — it does the same thing and lint holds you to the derivation.",
			Placement: "leading full-line comment above the field, or a trailing comment after the field's terminating `;` — inline `[...]` options, including multi-line braced protovalidate values, sit between them harmlessly. A marker no field consumes REFUSES the birth, naming file and line.",
			Example:   "int64 unit_price_cents = 4 [(buf.validate.field).int64.gte = 0]; // forge:read-only",
		},
		{
			Name:      codegen.ProtoMarkerComputed,
			AppliesTo: "field",
			Effect:    "Everything forge:read-only does (kept on the entity + Get/List, OMITTED from the born Create/Update requests), PLUS a declared obligation that your app derives the value. `forge lint --computed-fields` fails the field when no non-generated Go file assigns it. Use this instead of forge:read-only whenever the value is calculated rather than defaulted: a read-only field nothing computes takes the column DEFAULT, which for a money column is 0 — no constraint is violated, no test fails, and the only symptom is a screen showing $0.00.",
			Placement: "leading full-line comment above the field, or a trailing comment after the field's terminating `;` — same two positions as forge:read-only, and a marker no field consumes REFUSES the birth.",
			Example:   "// quantity_milli * unit_price_cents / 1000, maintained on write.\nint64 amount_cents = 7; // forge:computed",
		},
		{
			Name:      codegen.ProtoMarkerMutation,
			AppliesTo: "method",
			Effect:    "Forces the generated React Query hook for this rpc to be a useMutation. Classification is otherwise by leading verb — Get/List/Search/Find/Check/Has/Is/Count/Exists (whole-word) are reads, EVERYTHING else is a mutation — so this is needed only for an imperative rpc that happens to open with a read word (FindAndHoldSeat, IssueRefund). A write generated as useQuery re-fires on every component remount. Unlike the entity/field markers this one is read on every `forge generate`, not just at birth.",
			Placement: "leading full-line comment above the `rpc` line, or a trailing comment on it",
			Example:   "// forge:mutation — holds inventory, so it must never be a cached read.\nrpc FindAndHoldSeat(FindAndHoldSeatRequest) returns (FindAndHoldSeatResponse);",
		},
		{
			Name:      codegen.ProtoMarkerSecret,
			AppliesTo: "field",
			Effect:    "Column stays real schema truth but is stripped from read responses — the generated <entity>ToProto never packs it, so Get/List omit it and it is excluded from the list search span. Because a client therefore reads it back as \"\", the column is also tagged ,skipupdate: a maskless full-replace Update leaves the stored value alone rather than clobbering the credential with that \"\". Settable on Create, and on an explicit update_mask that names it.",
			Placement: "leading full-line comment above the field, or a trailing comment on the field line",
			Example:   "string api_token = 2; // forge:secret",
		},

		// ── Column-comment markers ──
		//
		// Declared as a postgres catalog COMMENT applied by a migration, not
		// as a `//` proto comment — these attach to the APPLIED schema, which
		// is forge's one source of truth for columns and constraints (see the
		// `db` skill). schemadef.Column.HasMarker / schemadef.KnownColumnMarkers
		// is the single registry both this dump and `forge lint`'s
		// unknown-column-marker check read, so the two cannot drift.
		{
			Name:      schemadef.ColumnMarkerImmutable,
			AppliesTo: "column",
			Effect:    "The column is omitted from a full-replace UPDATE's SET clause — projects to Bun's `,skipupdate` tag on the generated struct — while an explicit update_mask naming it still writes it. Use for a value the server owns that a client round-trip must not clobber.",
			Placement: "COMMENT ON COLUMN <table>.<col> IS 'forge:immutable'; in a migration",
			Example:   "COMMENT ON COLUMN invoices.total_cents IS 'forge:immutable';",
		},
		{
			Name:      schemadef.ColumnMarkerRef,
			AppliesTo: "column",
			Effect:    "Declares which route is authoritative when a table reaches the same parent two ways (a direct FK and a longer FK chain) — `authoritative` (this edge is the truth), `derived-from=<column>` (seed this column from the named edge), or `independent` (unrelated facts). Consumed by `forge db seed`'s diamond resolution; undeclared diamonds are refused rather than silently seeded inconsistently. Full grammar and worked examples: the `db/seeding` skill.",
			Placement: "COMMENT ON CONSTRAINT <fk_name> ON <table> IS 'forge:ref ...'; in a migration — on the FOREIGN KEY constraint, NOT a column comment.",
			Example:   "COMMENT ON CONSTRAINT orders_patient_id_fkey ON orders IS 'forge:ref derived-from=prescription_id';",
		},
		{
			Name:      schemadef.ColumnMarkerVersion,
			AppliesTo: "column",
			Effect:    "Opts the entity into optimistic concurrency control: Update/UpdateMasked add this column to the WHERE clause (matched against the caller's last-read value) and increment it on a successful write. A write that loses the race to a concurrent writer fails with Connect code Aborted instead of silently overwriting the other writer's change. REQUIRES a matching field on the entity's wire message (`int64 version = N;  // forge:read-only`) — the check compares the version the CALLER presents, so a column the caller cannot read back and return is always the zero value, and every update after a row's first one fails Aborted; `forge generate` refuses the column-only half rather than emit that. The repo owns the increment, so the client's proposed value is never trusted: forge:read-only keeps the field off the Create/Update request shapes while leaving it on the entity and the read responses, and an update_mask naming the column is an unknown_field error.",
			Placement: "BOTH halves, in the same change: ALTER TABLE <table> ADD COLUMN <col> BIGINT NOT NULL DEFAULT 0; plus COMMENT ON COLUMN <table>.<col> IS 'forge:version'; in a migration, AND `int64 <col> = N;  // forge:read-only` on the entity message in the proto",
			Example:   "COMMENT ON COLUMN crews.version IS 'forge:version';  (with `int64 version = 12;  // forge:read-only` on message Crew)",
		},
		{
			Name:      schemadef.ColumnMarkerFill,
			AppliesTo: "column",
			Effect:    "Declares WHO fills a forge:read-only column the generated Create/Update never carries a value for — `ulid` (forge generates one at Create, the same chokepoint that ULID-generates an empty string PK; non-PK columns only) or `handler` (pure acknowledgement — no codegen behavior changes, but the create shim scaffolds an op.Entity wrapper with a FORGE_SCAFFOLD reminder naming the column). Suppresses `forge lint`'s unsatisfiable-column check, which otherwise fails the build for a NOT NULL column with no DB DEFAULT and no forge:fill declaration — that combination cannot be inserted through the generated CRUD path at all.",
			Placement: "COMMENT ON COLUMN <table>.<col> IS 'forge:fill=ulid'; or 'forge:fill=handler'; in a migration",
			Example:   "COMMENT ON COLUMN customers.company_id IS 'forge:fill=handler';",
		},

		// ── Go-source markers ──
		//
		// These are read out of the project's .go files, not its protos.
		// Both the spaced (`// forge:x`) and unspaced (`//forge:x`) forms are
		// accepted, and the marker must be the WHOLE comment line after
		// stripping comment syntax and whitespace — so prose that merely
		// mentions a directive is never mistaken for one.
		{
			Name:      "forge:optional-dep",
			AppliesTo: "deps-field",
			Effect:    "The Deps field may legitimately be nil at construction: validateDeps must not enforce it, and composition emits the typed zero silently rather than reporting an unresolved dependency. Guard each use (`if s.deps.X != nil`). Without the marker, a required non-scalar Deps field that resolves to nothing is a loud generate-time or compile-time error — forge never emits a silent typed-zero for a required field.",
			Placement: "leading full-line comment above the Deps struct field, or a trailing comment on it",
			Example:   "type Deps struct {\n\t// forge:optional-dep\n\tStripe StripeClient\n}",
		},
		{
			Name:      "forge:optional-checked",
			AppliesTo: "deps-field",
			Effect:    "Suppresses `forge lint --optional-deps-guard` on one dereference of a `forge:optional-dep` field that is provably safe by reasoning the analyzer cannot follow.",
			Placement: "on the dereferencing line, or the line above it",
			Example:   "return s.deps.Stripe.Charge(ctx, amt) // forge:optional-checked",
		},
		{
			Name:      "forge:constructor",
			AppliesTo: "constructor",
			Effect:    "Names this function as the package's component constructor when it is not the canonical `New`, and opts the package IN to the generated observability decorator (middleware_gen.go), so every component-to-component call on the interface gets a span, a metric and panic recovery. Scaffolds stamp it by default, so a generated package is born instrumented.",
			Placement: "on the constructor's doc comment",
			Example:   "// NewClient builds the client.\n// forge:constructor\nfunc NewClient(d Deps) (Service, error)",
		},
		{
			Name:      "forge:no-observe",
			AppliesTo: "constructor",
			Effect:    "Opts out of the generated observability decorator. On the constructor it exempts the whole package; on a single interface method it routes just that method around the chain. The opt-out wins over every opt-in.",
			Placement: "on the constructor's doc comment (whole package), or above one interface method",
			Example:   "type Service interface {\n\t// forge:no-observe\n\tHealthz() error\n}",
		},
		{
			Name:      "forge:service",
			AppliesTo: "contract",
			Effect:    "Declares this interface to be the package's contract when it is not named `Service`. `forge:contract` is an accepted synonym. Names are deliberately free — `Service` is a poor name for a mailer or a store — but the signature is not: the constructor must take Deps and return the contract, or there is nothing to wire.",
			Placement: "on the interface's doc comment",
			Example:   "// forge:service\ntype Mailer interface { Send(context.Context, Message) error }",
		},
		{
			Name:      "forge:exclude-contract",
			AppliesTo: "package",
			Effect:    "Opts the package out of contract codegen entirely — no canonical-shape check, no wiring, and NO generated mock. The per-package equivalent of listing it in forge.yaml `contracts.exclude`. For packages that are genuinely not contract-shaped; the trade is that you hand-roll any test double yourself.",
			Placement: "package doc comment, or a free-standing comment in any of the package's .go files",
			Example:   "// forge:exclude-contract\npackage strategyregistry",
		},
		{
			Name:      "forge:external-component",
			AppliesTo: "package",
			Effect:    "This component is hand-constructed in providers.go / OpenInfra rather than by the injector, so the injector skips it — but the package STILL gets its contract and mock codegen. For a package that is contract-shaped and wants its mock, but whose construction is bespoke (adapter wrapping, two-phase setters, a dialer nil'd on unset env) and so cannot be a plain New(Deps) node. `forge:provided` is an accepted synonym.",
			Placement: "package doc comment, or a free-standing comment in any of the package's .go files",
			Example:   "// forge:external-component\npackage natsbus",
		},
		{
			Name:      "forge:outbound-io",
			AppliesTo: "package",
			Effect:    "Asserts the package is an outbound boundary — it talks OUT to a third-party system and must not serve inbound RPCs. `forge lint` enforces that with forgeconv-outbound-io-no-rpc.",
			Placement: "package doc comment, or a free-standing comment in any of the package's .go files",
			Example:   "// forge:outbound-io\npackage stripeadapter",
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
