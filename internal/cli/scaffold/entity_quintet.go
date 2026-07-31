// File: internal/cli/scaffold/entity_quintet.go
//
// CRUD-quintet COMPLETION — the one-time proto injection for an entity
// whose wire MESSAGE already exists (authored by hand, typically under a
// `// forge:entity` marker) but whose Create/Get/Update/Delete/List
// surface is missing or partial. It reuses the exact same piece builders
// `forge scaffold entity` uses for its proto half
// (buildEntityCRUDRPCPieces / buildEntityCRUDMessagePieces in
// entity.go), injecting ONLY the pieces the raw proto lacks — an rpc
// or message the user already declared is never touched. Like every
// birth affordance, this is one-time: after injection the wire contract
// is the user's, and forge never re-derives or reconciles it
// (docs/design/VERTICAL_SCAFFOLDING.md §6).

package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// entityFieldsFromSchemaDefs maps a message's SchemaFieldDef list (from
// the compiled descriptor OR the raw-proto scan — one vocabulary) onto
// the entityField shape the CRUD piece builders consume. protoPkg is the
// service's own package (same-package type names render relative).
// The managed fields (id / created_at / updated_at / deleted_at) are
// skipped silently: the entity message declares them (birth injects them
// — entity_managed_fields.go) and the server owns their values, so the
// FLATTENING write envelopes never take them. Unprojectable fields (oneof
// members) are skipped with a note.
func entityFieldsFromSchemaDefs(protoPkg string, defs []codegen.SchemaFieldDef) (fields []entityField, notes []string) {
	for _, f := range defs {
		switch f.Name {
		case "id", "created_at", "updated_at", "deleted_at":
			continue // server-owned: declared on the entity, never on a write envelope
		}
		if f.Oneof != "" {
			notes = append(notes, fmt.Sprintf("field %s: member of oneof %q — add it to the Create/List envelopes by hand if it belongs there", f.Name, f.Oneof))
			continue
		}
		decl, ok := protoFieldDecl(f, protoPkg)
		if !ok {
			notes = append(notes, fmt.Sprintf("field %s: kind %q has no mechanical envelope projection — add it by hand if it belongs there", f.Name, f.Kind))
			continue
		}
		ef := entityField{
			Name: f.Name,
			Type: entityFieldType{Proto: decl, Repeated: f.Repeated},
			// The field's `(buf.validate.field)` options ride along so the
			// born Create request — which flattens this field rather than
			// nesting the entity — enforces the SAME rules on the wire. Safe
			// to copy verbatim because protoFieldDecl above renders the
			// flattened field with the entity field's exact label and type,
			// so every rule that type-checks on one type-checks on the other.
			ValidateOptions: f.ValidateOptions,
			ServerSet:       f.ServerSet,
		}
		// Decl feeds the List-request affordances (search on string
		// entities, optional bool filters) — plain scalars only.
		if !f.Repeated && !f.Optional {
			switch f.Kind {
			case "string":
				ef.Decl = "string"
			case "bool":
				ef.Decl = "bool"
			}
		}
		fields = append(fields, ef)
	}
	return fields, notes
}

// protoFieldDecl renders one SchemaFieldDef back into proto3 field-type
// syntax ("optional string", "repeated Order.Item", "map<string, int64>").
// Same-package names render relative (the injected messages live in the
// same package); google.protobuf.* and cross-package names stay fully
// qualified — their imports are already present in the file, because the
// entity message itself references them.
func protoFieldDecl(f codegen.SchemaFieldDef, protoPkg string) (string, bool) {
	rel := func(fq string) string {
		return strings.TrimPrefix(fq, protoPkg+".")
	}
	var base string
	switch f.Kind {
	case "map":
		valType := f.MapValueKind
		if f.MapValueTypeName != "" {
			valType = rel(f.MapValueTypeName)
		}
		return fmt.Sprintf("map<%s, %s>", f.MapKeyKind, valType), true
	case "message", "enum":
		if f.TypeName == "" {
			return "", false
		}
		base = rel(f.TypeName)
	default:
		if !codegen.IsProtoScalarKind(f.Kind) {
			return "", false
		}
		base = f.Kind
	}
	switch {
	case f.Repeated:
		return "repeated " + base, true
	case f.Optional:
		return "optional " + base, true
	default:
		return base, true
	}
}

// quintetCompletionResult reports what one completion did, for the
// per-item phase lines.
type quintetCompletionResult struct {
	// AddedRPCs / AddedMessages name the injected pieces; both empty
	// means the quintet (and its envelopes) was already complete.
	AddedRPCs     []string
	AddedMessages []string
	// ProtoPath is the file the injection targeted (the service file).
	ProtoPath string
}

func (r *quintetCompletionResult) Complete() bool {
	return len(r.AddedRPCs) == 0 && len(r.AddedMessages) == 0
}

// completeEntityCRUDProto injects the MISSING CRUD quintet pieces for
// entity into the service proto file (the file carrying the `service {}`
// block). fields is the entity message's field list (already mapped via
// entityFieldsFromSchemaDefs). entityDeclFile is the file declaring the
// entity message; when it differs from the service file, the service
// file gains an import of it (relative to the proto root) so the
// injected envelopes' entity references resolve.
//
// Presence checks run against the RAW proto text — an rpc or message the
// user already declared (any shape) is skipped, never rewritten.
func completeEntityCRUDProto(root, serviceProtoPath, entityDeclFile, entity string, fields []entityField, appendOnly bool) (*quintetCompletionResult, error) {
	raw, err := os.ReadFile(serviceProtoPath)
	if err != nil {
		return nil, err
	}
	content := string(raw)
	res := &quintetCompletionResult{ProtoPath: serviceProtoPath}

	var rpcTexts []string
	for _, p := range appendOnlyFilter(buildEntityCRUDRPCPieces(entity), entity, appendOnly) {
		if rpcDeclRE(p.name).MatchString(content) {
			continue
		}
		rpcTexts = append(rpcTexts, p.text)
		res.AddedRPCs = append(res.AddedRPCs, p.name)
	}
	var msgTexts []string
	for _, p := range appendOnlyFilter(buildEntityCRUDMessagePieces(entity, fields), entity, appendOnly) {
		if strings.Contains(content, "message "+p.name+" {") {
			continue
		}
		msgTexts = append(msgTexts, p.text)
		res.AddedMessages = append(res.AddedMessages, p.name)
	}
	if res.Complete() {
		return res, nil
	}

	// Imports the injected pieces rely on. forge/v1/forge.proto is
	// load-bearing for the (forge.v1.method) options (see
	// injectEntityCRUDProto's identical block).
	for _, imp := range []string{
		"forge/v1/forge.proto",
		"google/protobuf/timestamp.proto",
		"google/protobuf/field_mask.proto",
	} {
		content = ensureProtoImport(content, imp)
	}
	// The Create request repeats the entity's field rules, so the SERVICE
	// file needs buf/validate/validate.proto even when the annotated entity
	// message is declared in another file (split protos) that already
	// imports it. Keyed on what was actually emitted — a rule-free entity
	// never drags the import in.
	if strings.Contains(strings.Join(msgTexts, ""), "buf.validate.field") {
		content = ensureProtoImport(content, "buf/validate/validate.proto")
	}
	// Cross-file entity declaration (split protos): the service file must
	// import the declaring file for the envelopes' entity references.
	if entityDeclFile != "" && entityDeclFile != serviceProtoPath {
		if rel, rerr := filepath.Rel(filepath.Join(root, "proto"), entityDeclFile); rerr == nil && !strings.HasPrefix(rel, "..") {
			content = ensureProtoImport(content, filepath.ToSlash(rel))
		}
	}

	if len(rpcTexts) > 0 {
		block := fmt.Sprintf("\n  // %s CRUD — quintet completed by forge (one-time); the wire contract is yours.\n%s",
			entity, strings.Join(rpcTexts, ""))
		content, err = insertIntoServiceBlock(content, block)
		if err != nil {
			return nil, err
		}
	}
	if len(msgTexts) > 0 {
		content = strings.TrimRight(content, "\n") + "\n\n" + strings.Join(msgTexts, "\n")
	}
	if err := os.WriteFile(serviceProtoPath, []byte(content), 0o644); err != nil {
		return nil, err
	}
	return res, nil
}

// rpcDeclRE matches a declaration of rpc <name> in raw proto text.
func rpcDeclRE(name string) *regexp.Regexp {
	return regexp.MustCompile(`\brpc\s+` + regexp.QuoteMeta(name) + `\s*\(`)
}

// appendOnlyFilter drops the Update/Delete CRUD pieces (rpc blocks and
// their request/response messages) when the entity is `// forge:append-only`,
// leaving only the Create/Get/List quintet subset — the wire half of the
// immutable-ledger contract. A no-op when appendOnly is false. Both piece
// builders name Update/Delete pieces "Update<Entity>*" / "Delete<Entity>*",
// so a prefix test isolates exactly those verbs.
func appendOnlyFilter(pieces []crudProtoPiece, entity string, appendOnly bool) []crudProtoPiece {
	if !appendOnly {
		return pieces
	}
	drop := []string{"Update" + entity, "Delete" + entity}
	out := pieces[:0:0]
	for _, p := range pieces {
		skip := false
		for _, pre := range drop {
			if strings.HasPrefix(p.name, pre) {
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, p)
		}
	}
	return out
}
