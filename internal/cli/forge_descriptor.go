package cli

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/reliant-labs/forge/internal/codegen"
	forgev1 "github.com/reliant-labs/forge/pkg/forgepb"
)

// descriptorStageDir is the per-invocation staging directory under the
// descriptor output directory where each buf-plugin process drops a JSON
// fragment named after the proto package(s) it processed. The parent
// process (runDescriptorGenerate) merges these fragments into
// forge_descriptor.json after buf finishes.
const descriptorStageDir = ".descriptor.d"

// ForgeDescriptor is the top-level JSON structure written by mode=descriptor.
// It aggregates all data the generate.go pipeline needs from proto descriptors.
type ForgeDescriptor struct {
	Services []codegen.ServiceDef    `json:"services"`
	Configs  []codegen.ConfigMessage `json:"configs"`
}

// generateDescriptor extracts services, entities, and configs from all proto
// files this plugin invocation was given, and writes a per-invocation
// fragment under <descriptorOut>/<descriptorStageDir>/<hash>.json.
//
// The output directory is passed via the "descriptor_out" plugin option
// (set by runDescriptorGenerate in generate_orm.go). The fragments are
// merged into forge_descriptor.json by mergeDescriptorFragments() in the
// parent process after buf finishes.
//
// Why per-invocation fragments instead of a shared file: buf spawns one
// plugin process per proto module, so the previous read-modify-write
// approach (with an in-process syscall.Flock) silently raced when two
// plugin processes were active at the same time, producing concatenated
// or truncated JSON. Per-process fragment writes are atomic with respect
// to each other (each process owns its own filename) and the parent
// process is the single writer of the final descriptor.
func generateDescriptor(p *protogen.Plugin, descriptorOut string) error {
	if err := os.MkdirAll(descriptorOut, 0o755); err != nil {
		return fmt.Errorf("create descriptor output directory: %w", err)
	}
	stageDir := filepath.Join(descriptorOut, descriptorStageDir)
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return fmt.Errorf("create descriptor stage directory: %w", err)
	}

	var desc ForgeDescriptor
	var fragmentKeyParts []string

	for _, f := range p.Files {
		if !f.Generate {
			continue
		}
		fragmentKeyParts = append(fragmentKeyParts, f.Desc.Path())

		// Extract services
		for _, svc := range f.Services {
			sd := extractService(f, svc)
			desc.Services = append(desc.Services, sd)
		}

		// Entity protos are dead: SQL is the schema language and entities
		// are projected from the applied db/migrations schema. A message
		// still carrying the legacy (forge.v1.entity) annotation gets a
		// one-line migration notice — the annotation is otherwise IGNORED.
		for _, msg := range f.Messages {
			noticeLegacyEntityAnnotation(f, msg)
		}

		// Extract config messages. Walks nested message declarations too
		// so a component config block declared inside AppConfig (instead
		// of the conventional top-level declaration) still generates.
		for _, msg := range f.Messages {
			appendConfigMessages(&desc.Configs, msg)
		}
	}

	// Skip writing an empty fragment — saves a no-op file per plugin
	// invocation and keeps the merge step's directory listing clean.
	if len(desc.Services) == 0 && len(desc.Configs) == 0 {
		return nil
	}

	// Stable, content-addressable filename so a second invocation against
	// the same proto files overwrites rather than duplicating fragments.
	sort.Strings(fragmentKeyParts)
	h := sha1.New()
	for _, p := range fragmentKeyParts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{'\x00'})
	}
	fragmentName := hex.EncodeToString(h.Sum(nil)) + ".json"

	descBytes, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return err
	}

	// Atomic write: write to a unique temp file in the same directory,
	// then rename. Rename is atomic on POSIX filesystems even across
	// concurrent processes.
	tmp, err := os.CreateTemp(stageDir, "frag-*.tmp")
	if err != nil {
		return fmt.Errorf("create descriptor fragment tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(descBytes); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write descriptor fragment: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close descriptor fragment: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(stageDir, fragmentName)); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename descriptor fragment: %w", err)
	}

	return nil
}

// MergeDescriptorFragments combines all per-invocation fragments under
// <descriptorOut>/<descriptorStageDir>/ into a single forge_descriptor.json
// at <descriptorOut>/forge_descriptor.json, then removes the staging dir.
// Called by runDescriptorGenerate in the parent process after buf returns.
//
// Idempotent: running it twice is safe (the second run sees an empty stage
// dir and is a no-op apart from rewriting the final descriptor with what's
// already there). Returns nil silently when no fragments exist (clean
// projects with no services/entities/configs).
func MergeDescriptorFragments(descriptorOut string) error {
	stageDir := filepath.Join(descriptorOut, descriptorStageDir)
	entries, err := os.ReadDir(stageDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read descriptor stage dir: %w", err)
	}

	// Sort fragment names so the merged descriptor has a deterministic
	// order regardless of OS-specific directory iteration order.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	var merged ForgeDescriptor
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(stageDir, name))
		if err != nil {
			return fmt.Errorf("read descriptor fragment %s: %w", name, err)
		}
		var frag ForgeDescriptor
		if err := json.Unmarshal(data, &frag); err != nil {
			return fmt.Errorf("parse descriptor fragment %s: %w", name, err)
		}
		merged.Services = append(merged.Services, frag.Services...)
		merged.Configs = append(merged.Configs, frag.Configs...)
	}

	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	outPath := filepath.Join(descriptorOut, "forge_descriptor.json")
	if err := os.WriteFile(outPath, out, 0o644); err != nil {
		return fmt.Errorf("write forge_descriptor.json: %w", err)
	}

	// Best-effort cleanup of the staging dir; failure here doesn't matter
	// for correctness because the next run starts with rm -rf.
	_ = os.RemoveAll(stageDir)
	return nil
}

// applyMethodOptions copies the proto-level (forge.v1.method) annotations
// onto a codegen.Method. Kept as a tiny helper so the descriptor's
// MethodOptions handling stays unit-testable without standing up a full
// protogen graph — see TestApplyMethodOptions_*.
func applyMethodOptions(method *codegen.Method, mo *forgev1.MethodOptions) {
	if mo == nil {
		return
	}
	if mo.AuthRequired != nil {
		method.AuthRequired = *mo.AuthRequired
	}
	if len(mo.Errors) > 0 {
		method.Errors = append([]string(nil), mo.Errors...)
	}
	// authz_custom delegates the authorization decision to a hand-written
	// authorizer; the method carries no role allow-list. Carry the flag so the
	// authorizer generator can emit it FAIL-CLOSED in the role table rather than
	// with empty roles (which would read as an any-authenticated grant).
	if mo.GetAuthzCustom() {
		method.AuthzCustom = true
	}
}

// extractService builds a codegen.ServiceDef from a protogen.Service.
// goPkgName derives the short Go package name from a protogen.File.
// For a go_package like "github.com/example/gen/services/api/v1;apiv1" it
// returns "apiv1". When no alias is present it falls back to the last path
// segment of the import path.
func goPkgName(file *protogen.File) string {
	// protogen gives us the package name directly — it already resolves the
	// alias from the go_package option.
	if pn := string(file.GoPackageName); pn != "" {
		return pn
	}
	// Fallback: last path segment of the import path.
	p := string(file.GoImportPath)
	if idx := strings.LastIndex(p, "/"); idx >= 0 {
		return p[idx+1:]
	}
	return p
}

func extractService(file *protogen.File, svc *protogen.Service) codegen.ServiceDef {
	sd := codegen.ServiceDef{
		Name:      string(svc.Desc.Name()),
		Package:   string(file.Desc.Package()),
		GoPackage: string(file.GoImportPath),
		PkgName:   goPkgName(file),
		ProtoFile: file.Desc.Path(),
		Messages:  make(map[string][]codegen.MessageFieldDef),
	}

	// Read service-level options
	if svcOpts := svc.Desc.Options(); svcOpts != nil {
		ext := proto.GetExtension(svcOpts, forgev1.E_Service)
		if ext != nil {
			if so, ok := ext.(*forgev1.ServiceOptions); ok && so != nil {
				// Service options available for future use (auth defaults, etc.)
				_ = so
			}
		}
	}

	for _, m := range svc.Methods {
		// Default to fail-closed: an unannotated method requires auth.
		// Methods that explicitly set `auth_required = false` in their
		// (forge.v1.method) annotation can opt out — the proto field is
		// `optional bool` so we distinguish unset from explicit false.
		// InputProtoFile / OutputProtoFile record the file that
		// physically declares the message. For same-file RPCs this
		// equals file.Desc.Path(); for cross-file refs (e.g. an RPC in
		// services/users/v1/users.proto returning a shared/v1/types.proto
		// Page) it differs, and the frontend hooks generator uses this
		// to import the symbol from the correct _pb.ts file rather than
		// the service's own _pb.ts.
		method := codegen.Method{
			Name:            string(m.Desc.Name()),
			InputType:       string(m.Input.Desc.Name()),
			OutputType:      string(m.Output.Desc.Name()),
			InputTypeFQ:     string(m.Input.Desc.FullName()),
			OutputTypeFQ:    string(m.Output.Desc.FullName()),
			ClientStreaming: m.Desc.IsStreamingClient(),
			ServerStreaming: m.Desc.IsStreamingServer(),
			AuthRequired:    true,
			InputProtoFile:  m.Input.Desc.ParentFile().Path(),
			OutputProtoFile: m.Output.Desc.ParentFile().Path(),
		}

		// Read method-level options
		if methodOpts := m.Desc.Options(); methodOpts != nil {
			ext := proto.GetExtension(methodOpts, forgev1.E_Method)
			if ext != nil {
				if mo, ok := ext.(*forgev1.MethodOptions); ok && mo != nil {
					applyMethodOptions(&method, mo)
				}
			}
		}

		sd.Methods = append(sd.Methods, method)

		// Extract message field definitions for input and output types
		extractMessageFields(sd.Messages, m.Input)
		extractMessageFields(sd.Messages, m.Output)

		// Extract the deep type graph (transitively reachable messages
		// + enums, keyed by fully-qualified name) for full JSON-Schema
		// emission in the MCP manifest.
		extractMessageSchema(&sd, m.Input)
		extractMessageSchema(&sd, m.Output)
	}

	return sd
}

// extractMessageSchema records msg's fields into sd.Schemas (keyed by
// fully-qualified name) and recurses into every message/enum the fields
// reference, so sd.Schemas ends up holding the complete type graph
// reachable from the service's RPC inputs/outputs.
//
// Recursion safety: the fully-qualified name is inserted into
// sd.Schemas BEFORE walking the fields, so self-referential messages
// (Tree → children []Tree) and mutually-recursive pairs terminate — the
// second visit hits the early-return.
//
// Well-known types (google.protobuf.*) are deliberately NOT recorded:
// their protojson encodings are fixed (Timestamp → RFC 3339 string,
// Struct → arbitrary object, ...) and the MCP schema emitter maps them
// statically. Recording e.g. Struct's internal fields would describe
// the proto representation, not the JSON wire shape.
func extractMessageSchema(sd *codegen.ServiceDef, msg *protogen.Message) {
	fq := string(msg.Desc.FullName())
	if strings.HasPrefix(fq, "google.protobuf.") {
		return
	}
	if sd.Schemas == nil {
		sd.Schemas = make(map[string][]codegen.SchemaFieldDef)
	}
	if sd.SchemaFiles == nil {
		sd.SchemaFiles = make(map[string]string)
	}
	if _, done := sd.Schemas[fq]; done {
		return
	}
	// Record the file that physically declares this message so downstream
	// codegen can import its generated `*Schema` from the right _pb module
	// when the message lives in a different proto file than the service.
	sd.SchemaFiles[fq] = msg.Desc.ParentFile().Path()
	sd.Schemas[fq] = nil // recursion guard — overwritten with the real fields below

	fields := make([]codegen.SchemaFieldDef, 0, len(msg.Fields))
	for _, f := range msg.Fields {
		fd := codegen.SchemaFieldDef{
			Name:     string(f.Desc.Name()),
			Optional: f.Desc.HasOptionalKeyword(),
		}
		// Real oneof membership only — proto3 `optional` is implemented
		// as a synthetic single-member oneof and must not surface as an
		// exclusivity constraint.
		if f.Oneof != nil && !f.Oneof.Desc.IsSynthetic() {
			fd.Oneof = string(f.Oneof.Desc.Name())
		}
		switch {
		case f.Desc.IsMap():
			fd.Kind = "map"
			fd.MapKeyKind = protoKindToString(f.Desc.MapKey().Kind())
			val := f.Desc.MapValue()
			fd.MapValueKind = protoKindToString(val.Kind())
			// For message/enum values, f.Message is the synthetic map
			// entry whose Fields[1] is the value — recurse through it so
			// the value type lands in the graph too.
			switch val.Kind() {
			case protoreflect.MessageKind:
				fd.MapValueTypeName = string(val.Message().FullName())
				if f.Message != nil && len(f.Message.Fields) == 2 && f.Message.Fields[1].Message != nil {
					extractMessageSchema(sd, f.Message.Fields[1].Message)
				}
			case protoreflect.EnumKind:
				fd.MapValueTypeName = string(val.Enum().FullName())
				if f.Message != nil && len(f.Message.Fields) == 2 && f.Message.Fields[1].Enum != nil {
					recordEnum(sd, f.Message.Fields[1].Enum)
				}
			}
		case f.Desc.Kind() == protoreflect.MessageKind, f.Desc.Kind() == protoreflect.GroupKind:
			fd.Kind = "message"
			fd.Repeated = f.Desc.IsList()
			fd.TypeName = string(f.Desc.Message().FullName())
			if f.Message != nil {
				extractMessageSchema(sd, f.Message)
			}
		case f.Desc.Kind() == protoreflect.EnumKind:
			fd.Kind = "enum"
			fd.Repeated = f.Desc.IsList()
			fd.TypeName = string(f.Desc.Enum().FullName())
			if f.Enum != nil {
				recordEnum(sd, f.Enum)
			}
		default:
			fd.Kind = protoKindToString(f.Desc.Kind())
			fd.Repeated = f.Desc.IsList()
		}
		fields = append(fields, fd)
	}
	sd.Schemas[fq] = fields
}

// recordEnum stores an enum's declared value names (declaration order)
// under its fully-qualified name. protojson encodes enum values as
// their names, so this list is verbatim the JSON Schema "enum" array.
func recordEnum(sd *codegen.ServiceDef, en *protogen.Enum) {
	fq := string(en.Desc.FullName())
	if strings.HasPrefix(fq, "google.protobuf.") {
		return
	}
	if sd.Enums == nil {
		sd.Enums = make(map[string][]string)
	}
	if _, done := sd.Enums[fq]; done {
		return
	}
	vals := make([]string, 0, len(en.Values))
	for _, v := range en.Values {
		vals = append(vals, string(v.Desc.Name()))
	}
	sd.Enums[fq] = vals
}

// extractMessageFields populates the Messages map with field definitions for a message.
func extractMessageFields(messages map[string][]codegen.MessageFieldDef, msg *protogen.Message) {
	name := string(msg.Desc.Name())
	if _, exists := messages[name]; exists {
		return
	}

	var fields []codegen.MessageFieldDef
	for _, f := range msg.Fields {
		// Repeated fields are encoded as "[]<elementKind>" so downstream
		// codegen (the MCP JSON Schema mapper, frontend hooks, etc.)
		// can distinguish scalar fields from arrays without inspecting
		// cardinality separately. FRICTION (cp-forge, 2026-06-09):
		// the MCP Inspector strict-validated structuredContent against
		// the declared outputSchema and rejected a List response
		// because the schema said `items: object` (singular message
		// type) while the wire data was `items: [array of objects]`.
		// Root cause was right here — the descriptor dropped
		// cardinality before it reached any downstream consumer.
		protoType := protoKindToString(f.Desc.Kind())
		if f.Desc.Cardinality() == protoreflect.Repeated && !f.Desc.IsMap() {
			protoType = "[]" + protoType
		}
		fd := codegen.MessageFieldDef{
			Name:       string(f.Desc.Name()),
			ProtoType:  protoType,
			IsOptional: f.Desc.HasOptionalKeyword(),
		}
		// Carry the referenced type's name for message fields. ProtoType
		// collapses these to the literal "message", which is unmatchable —
		// the CRUD shape matcher needs to know that UpdateItemRequest.item
		// is an Item, not just "a message" (F2 root cause).
		if f.Desc.Kind() == protoreflect.MessageKind && !f.Desc.IsMap() {
			fd.MessageType = string(f.Desc.Message().FullName())
		}
		fields = append(fields, fd)
	}
	messages[name] = fields
}

// noticeLegacyEntityAnnotation prints a one-line migration notice for
// messages still carrying the retired (forge.v1.entity) annotation.
// The annotation is ignored: entities are projections of the applied
// db/migrations schema now, and these projects' migrations were already
// the de-facto truth. The option DEFINITIONS remain in forge/v1/forge.proto
// (deprecated) so legacy protos keep compiling for one release.
func noticeLegacyEntityAnnotation(file *protogen.File, msg *protogen.Message) {
	opts, ok := msg.Desc.Options().(*descriptorpb.MessageOptions)
	if !ok || opts == nil {
		return
	}
	if !proto.HasExtension(opts, forgev1.E_Entity) {
		return
	}
	fmt.Fprintf(os.Stderr,
		"ℹ️  %s: message %s carries the retired (forge.v1.entity) annotation — it is now ignored.\n"+
			"   SQL is the schema: db/migrations drive the ORM/entity projections.\n"+
			"   Your migrations are already the truth; delete the annotation (and any proto/db entity files).\n",
		file.Desc.Path(), msg.Desc.Name())
}

// configFieldRoleString maps the ConfigFieldRole enum to the bare enum
// spelling stored on codegen.ConfigField.Role ("" for UNSPECIFIED so old
// descriptors and unannotated fields serialize identically). Config codegen
// keys semantic behavior on this string, never on the field name.
func configFieldRoleString(r forgev1.ConfigFieldRole) string {
	if r == forgev1.ConfigFieldRole_CONFIG_FIELD_ROLE_UNSPECIFIED {
		return ""
	}
	return r.String()
}

// appendConfigMessages extracts msg (and, recursively, its nested
// message declarations) into out. Nested declarations matter for
// component config blocks: `message AppConfig { message TraderConfig
// {...} TraderConfig trader = 21; }` must surface TraderConfig as its
// own ConfigMessage even though it isn't a top-level declaration.
func appendConfigMessages(out *[]codegen.ConfigMessage, msg *protogen.Message) {
	if cm, ok := extractConfigMessage(msg); ok {
		*out = append(*out, cm)
	}
	for _, nested := range msg.Messages {
		appendConfigMessages(out, nested)
	}
}

// extractConfigMessage checks if a message has any fields with config_field options
// and extracts them into a codegen.ConfigMessage.
//
// Two field shapes participate:
//
//   - scalar fields carrying the (forge.v1.config) extension — the classic
//     env_var/flag/default leaves;
//   - message-typed fields whose target message itself has config-annotated
//     fields — component config-block references (e.g. `TraderConfig trader
//     = 21;` on AppConfig). These need NO annotation of their own; the env
//     binding lives entirely on the referenced block's leaf fields. They are
//     recorded with ProtoType "message" + MessageType naming the block so
//     config_gen can emit a nested struct and wire_gen can resolve Deps
//     fields of the block type to `cfg.<Field>`.
func extractConfigMessage(msg *protogen.Message) (codegen.ConfigMessage, bool) {
	var configFields []codegen.ConfigField

	for _, f := range msg.Fields {
		// Component config-block reference: a message-typed field whose
		// target message has config-annotated fields. Repeated/map fields
		// are excluded — a config block composes exactly once per field.
		if f.Desc.Kind() == protoreflect.MessageKind && !f.Desc.IsList() && !f.Desc.IsMap() &&
			f.Message != nil && f.Message != msg && messageHasConfigFields(f.Message) {
			configFields = append(configFields, codegen.ConfigField{
				Name:        string(f.Desc.Name()),
				GoName:      f.GoName,
				ProtoType:   "message",
				MessageType: string(f.Message.Desc.Name()),
			})
			continue
		}

		opts := f.Desc.Options()
		if opts == nil {
			continue
		}

		ext := proto.GetExtension(opts, forgev1.E_Config)
		if ext == nil {
			continue
		}

		cf, ok := ext.(*forgev1.ConfigFieldOptions)
		if !ok || cf == nil {
			continue
		}

		configFields = append(configFields, codegen.ConfigField{
			Name:         string(f.Desc.Name()),
			GoName:       f.GoName,
			GoType:       codegen.ProtoTypeToGoType(protoKindToString(f.Desc.Kind())),
			ProtoType:    protoKindToString(f.Desc.Kind()),
			EnvVar:       cf.GetEnvVar(),
			Flag:         cf.GetFlag(),
			DefaultValue: cf.GetDefaultValue(),
			Required:     cf.GetRequired(),
			Description:  cf.GetDescription(),
			Sensitive:    cf.GetSensitive(),
			Category:     cf.GetCategory(),
			Role:         configFieldRoleString(cf.GetRole()),
		})
	}

	if len(configFields) == 0 {
		return codegen.ConfigMessage{}, false
	}

	return codegen.ConfigMessage{
		Name:   string(msg.Desc.Name()),
		Fields: configFields,
	}, true
}

// messageHasConfigFields reports whether any DIRECT field of msg
// carries the (forge.v1.config) extension — the test for "is this
// message a config block?". Deliberately non-recursive: one level of
// block nesting is the supported shape (root Config → block leaves).
func messageHasConfigFields(msg *protogen.Message) bool {
	for _, f := range msg.Fields {
		opts := f.Desc.Options()
		if opts == nil {
			continue
		}
		if proto.HasExtension(opts, forgev1.E_Config) {
			return true
		}
	}
	return false
}

// protoKindToString returns the proto type name for a protoreflect.Kind.
func protoKindToString(k protoreflect.Kind) string {
	switch k {
	case protoreflect.BoolKind:
		return "bool"
	case protoreflect.Int32Kind:
		return "int32"
	case protoreflect.Sint32Kind:
		return "sint32"
	case protoreflect.Sfixed32Kind:
		return "sfixed32"
	case protoreflect.Uint32Kind:
		return "uint32"
	case protoreflect.Fixed32Kind:
		return "fixed32"
	case protoreflect.Int64Kind:
		return "int64"
	case protoreflect.Sint64Kind:
		return "sint64"
	case protoreflect.Sfixed64Kind:
		return "sfixed64"
	case protoreflect.Uint64Kind:
		return "uint64"
	case protoreflect.Fixed64Kind:
		return "fixed64"
	case protoreflect.FloatKind:
		return "float"
	case protoreflect.DoubleKind:
		return "double"
	case protoreflect.StringKind:
		return "string"
	case protoreflect.BytesKind:
		return "bytes"
	case protoreflect.MessageKind:
		return "message"
	case protoreflect.EnumKind:
		return "enum"
	default:
		return "string"
	}
}
