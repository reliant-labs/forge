package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	forgev1 "github.com/reliant-labs/forge/gen/forge/v1"
	"github.com/reliant-labs/forge/internal/codegen"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ForgeDescriptor is the top-level JSON structure written by mode=descriptor.
// It aggregates all data the generate.go pipeline needs from proto descriptors.
type ForgeDescriptor struct {
	Services []codegen.ServiceDef    `json:"services"`
	Entities []codegen.EntityDef     `json:"entities"`
	Configs  []codegen.ConfigMessage `json:"configs"`
}

// generateDescriptor extracts services, entities, and configs from all proto
// files and writes them as a single forge_descriptor.json.
//
// The output directory is passed via the "descriptor_out" plugin option
// (set by runDescriptorGenerate in generate_orm.go). The file is written
// directly to disk instead of through protogen's generated-file mechanism
// to avoid deduplication issues when buf invokes the plugin once per proto
// package — protogen would keep only the last written copy, losing data
// from earlier packages.
func generateDescriptor(p *protogen.Plugin, descriptorOut string) error {
	if err := os.MkdirAll(descriptorOut, 0o755); err != nil {
		return fmt.Errorf("create descriptor output directory: %w", err)
	}

	outPath := filepath.Join(descriptorOut, "forge_descriptor.json")

	// Use a lock file to prevent concurrent buf plugin invocations from
	// racing on the read-modify-write of forge_descriptor.json.
	lockPath := outPath + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open lock file: %w", err)
	}
	defer func() {
		_ = lockFile.Close()
		_ = os.Remove(lockPath)
	}()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire descriptor lock: %w", err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	// Read existing descriptor to merge with (buf invokes the plugin once per
	// proto package, so we accumulate results across invocations).
	var desc ForgeDescriptor
	if existing, err := os.ReadFile(outPath); err == nil {
		_ = json.Unmarshal(existing, &desc)
	}

	for _, f := range p.Files {
		if !f.Generate {
			continue
		}

		// Extract services
		for _, svc := range f.Services {
			sd := extractService(f, svc)
			desc.Services = append(desc.Services, sd)
		}

		// Extract entities from messages with entity annotations or entity conventions
		for _, msg := range f.Messages {
			if ed, ok := extractEntityDef(f, msg); ok {
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

	descBytes, err := json.MarshalIndent(desc, "", "  ")
	if err != nil {
		return err
	}

	if err := os.WriteFile(outPath, descBytes, 0o644); err != nil {
		return fmt.Errorf("write forge_descriptor.json: %w", err)
	}

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
		method := codegen.Method{
			Name:           string(m.Desc.Name()),
			InputType:      string(m.Input.Desc.Name()),
			OutputType:     string(m.Output.Desc.Name()),
			ClientStreaming: m.Desc.IsStreamingClient(),
			ServerStreaming: m.Desc.IsStreamingServer(),
		}

		// Read method-level options
		if methodOpts := m.Desc.Options(); methodOpts != nil {
			ext := proto.GetExtension(methodOpts, forgev1.E_Method)
			if ext != nil {
				if mo, ok := ext.(*forgev1.MethodOptions); ok && mo != nil {
					method.AuthRequired = mo.GetAuthRequired()
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

// extractEntityDef converts a proto message with entity annotations to a codegen.EntityDef.
func extractEntityDef(file *protogen.File, msg *protogen.Message) (codegen.EntityDef, bool) {
	// Try to parse as entityInfo first (reuses the ORM parsing logic)
	ent, ok := parseEntity(msg)
	if !ok {
		return codegen.EntityDef{}, false
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

	// Default PK
	if ed.PkField == "" {
		ed.PkField = "id"
		ed.PkGoType = "string"
	}

	return ed, true
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