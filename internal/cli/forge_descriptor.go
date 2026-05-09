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

	forgev1 "github.com/reliant-labs/forge/gen/forge/v1"
	"github.com/reliant-labs/forge/internal/codegen"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
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
	Entities []codegen.EntityDef     `json:"entities"`
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

		// Extract entities from messages with explicit entity annotations.
		// Forge has no auto-detection: a message becomes an entity only when
		// it carries (forge.v1.entity) AND has a (forge.v1.field) = { pk: true }
		// marker. extractEntityDef returns an error for malformed entities;
		// surface it via p.Error so `forge generate` halts with a clear
		// remediation message instead of silently producing broken code.
		for _, msg := range f.Messages {
			ed, ok, err := extractEntityDef(f, msg)
			if err != nil {
				p.Error(err)
				return nil
			}
			if ok {
				desc.Entities = append(desc.Entities, ed)
			}
		}

		// Extract config messages
		for _, msg := range f.Messages {
			if cm, ok := extractConfigMessage(msg); ok {
				desc.Configs = append(desc.Configs, cm)
			}
		}
	}

	// Skip writing an empty fragment — saves a no-op file per plugin
	// invocation and keeps the merge step's directory listing clean.
	if len(desc.Services) == 0 && len(desc.Entities) == 0 && len(desc.Configs) == 0 {
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
		merged.Entities = append(merged.Entities, frag.Entities...)
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
		method := codegen.Method{
			Name:           string(m.Desc.Name()),
			InputType:      string(m.Input.Desc.Name()),
			OutputType:     string(m.Output.Desc.Name()),
			ClientStreaming: m.Desc.IsStreamingClient(),
			ServerStreaming: m.Desc.IsStreamingServer(),
			AuthRequired:   true,
		}

		// Read method-level options
		if methodOpts := m.Desc.Options(); methodOpts != nil {
			ext := proto.GetExtension(methodOpts, forgev1.E_Method)
			if ext != nil {
				if mo, ok := ext.(*forgev1.MethodOptions); ok && mo != nil {
					if mo.AuthRequired != nil {
						method.AuthRequired = *mo.AuthRequired
					}
				}
			}
		}

		sd.Methods = append(sd.Methods, method)

		// Extract message field definitions for input and output types
		extractMessageFields(sd.Messages, m.Input)
		extractMessageFields(sd.Messages, m.Output)
	}

	return sd
}

// extractMessageFields populates the Messages map with field definitions for a message.
func extractMessageFields(messages map[string][]codegen.MessageFieldDef, msg *protogen.Message) {
	name := string(msg.Desc.Name())
	if _, exists := messages[name]; exists {
		return
	}

	var fields []codegen.MessageFieldDef
	for _, f := range msg.Fields {
		fd := codegen.MessageFieldDef{
			Name:       string(f.Desc.Name()),
			ProtoType:  protoKindToString(f.Desc.Kind()),
			IsOptional: f.Desc.HasOptionalKeyword(),
		}
		fields = append(fields, fd)
	}
	messages[name] = fields
}

// extractEntityDef converts a proto message with entity annotations to a
// codegen.EntityDef. Entities are annotation-only — a message becomes an
// entity solely by carrying `option (forge.v1.entity) = { ... }` plus a
// field marked `[(forge.v1.field) = { pk: true }]`. parseEntity returns a
// non-nil error for messages that ARE annotated as entities but lack a PK
// marker; that error is propagated via p.Error in the descriptor plugin.
func extractEntityDef(file *protogen.File, msg *protogen.Message) (codegen.EntityDef, bool, error) {
	ent, isEntity, err := parseEntity(msg)
	if err != nil {
		return codegen.EntityDef{}, false, err
	}
	if !isEntity {
		return codegen.EntityDef{}, false, nil
	}

	ed := codegen.EntityDef{
		Name:      string(msg.Desc.Name()),
		TableName: ent.tableName,
		ProtoFile: file.Desc.Path(),
	}

	for _, fi := range ent.fields {
		ef := codegen.EntityField{
			Name:      string(fi.field.Desc.Name()),
			GoName:    fi.field.GoName,
			ProtoType: protoKindToString(fi.field.Desc.Kind()),
			GoType:    goTypeForField(fi.field),
		}

		if fi.isPK {
			ed.PkField = ef.Name
			ed.PkGoType = ef.GoType
		}

		if fi.references != "" {
			ef.IsFK = true
			// Extract table name from "table.column" format
			parts := splitRef(fi.references)
			if len(parts) == 2 {
				ef.FKTable = parts[0]
			}
		}

		if fi.isTenantKey {
			ed.HasTenant = true
			ed.TenantFieldName = ef.Name
			ed.TenantGoName = ef.GoName
			ed.TenantColumnName = fi.columnName
		}

		ed.Fields = append(ed.Fields, ef)
	}

	// parseEntity already enforces that ed.PkField is non-empty for any
	// entity that gets here — no fallback needed.
	return ed, true, nil
}

// extractConfigMessage checks if a message has any fields with config_field options
// and extracts them into a codegen.ConfigMessage.
func extractConfigMessage(msg *protogen.Message) (codegen.ConfigMessage, bool) {
	var configFields []codegen.ConfigField

	for _, f := range msg.Fields {
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

// splitRef splits a "table.column" reference string.
func splitRef(ref string) []string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '.' {
			return []string{ref[:i], ref[i+1:]}
		}
	}
	return []string{ref}
}