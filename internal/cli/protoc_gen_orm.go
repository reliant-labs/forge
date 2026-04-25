package cli

import (
	"fmt"
	"os"

	forgev1 "github.com/reliant-labs/forge/gen/forge/v1"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/pluginpb"
)

// forgePluginMode controls what the protoc-gen-forge plugin generates.
type forgePluginMode string

const (
	forgePluginModeORM        forgePluginMode = "orm"
	forgePluginModeDescriptor forgePluginMode = "descriptor"
	// forgePluginModeScaffold is reserved for future use.
	forgePluginModeScaffold forgePluginMode = "scaffold"
)

func newProtocGenForgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "protoc-gen-forge",
		Short:  "Protoc plugin for code generation (invoked by buf)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// protogen.Options{}.Run() reads os.Args directly and rejects
			// any arg that doesn't start with "--". When invoked as
			// `forge protoc-gen-forge` by buf, os.Args[1] is the
			// subcommand name. Strip it so protogen sees only the binary.
			os.Args = os.Args[:1]

			mode := forgePluginModeORM // default
			descriptorOut := "gen"      // default output directory for descriptor mode

			opts := protogen.Options{
				ParamFunc: func(name, value string) error {
					switch name {
					case "mode":
						switch forgePluginMode(value) {
						case forgePluginModeORM, forgePluginModeDescriptor, forgePluginModeScaffold:
							mode = forgePluginMode(value)
						default:
							return fmt.Errorf("unknown mode: %s (valid: orm, descriptor, scaffold)", value)
						}
					case "descriptor_out":
						descriptorOut = value
					}
					return nil
				},
			}

			opts.Run(func(p *protogen.Plugin) error {
				p.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)

				switch mode {
				case forgePluginModeDescriptor:
					return generateDescriptor(p, descriptorOut)
				case forgePluginModeScaffold:
					p.Error(fmt.Errorf("scaffold mode is not yet implemented"))
					return nil
				default:
					return generateOrmPlugin(p)
				}
			})
			return nil
		},
	}
}

// generateOrmPlugin runs the ORM code generation mode (original behavior).
func generateOrmPlugin(p *protogen.Plugin) error {
	// Track which packages already got a shared header to avoid
	// redeclaring package-level vars (ormTracer etc.) when
	// multiple .proto files live in the same Go package.
	sharedGenerated := make(map[protogen.GoImportPath]bool)

	for _, f := range p.Files {
		if !f.Generate {
			continue
		}
		if err := generateOrmFile(p, f, sharedGenerated); err != nil {
			return err
		}
	}
	return nil
}

func generateOrmFile(p *protogen.Plugin, file *protogen.File, sharedGenerated map[protogen.GoImportPath]bool) error {
	var entities []entityInfo

	for _, msg := range file.Messages {
		ent, ok := parseEntity(msg)
		if !ok {
			continue
		}
		entities = append(entities, ent)
	}

	if len(entities) == 0 {
		return nil
	}

	// Check if any entity has timestamps for the shared file.
	anyHasTimestamp := false
	for _, ent := range entities {
		for _, f := range ent.fields {
			if f.isTimestamp {
				anyHasTimestamp = true
				break
			}
		}
		if anyHasTimestamp {
			break
		}
	}

	// Generate a shared file with package-level declarations (ormTracer, etc.)
	// Only generate once per Go package to avoid redeclaration errors.
	if !sharedGenerated[file.GoImportPath] {
		sharedFilename := file.GeneratedFilenamePrefix + "_orm_shared.pb.orm.go"
		shared := p.NewGeneratedFile(sharedFilename, file.GoImportPath)
		generateSharedHeader(shared, file, anyHasTimestamp)
		sharedGenerated[file.GoImportPath] = true
	}

	// Generate per-entity files.
	for _, ent := range entities {
		entHasTimestamp := false
		for _, f := range ent.fields {
			if f.isTimestamp {
				entHasTimestamp = true
				break
			}
		}

		filename := file.GeneratedFilenamePrefix + "_" + toSnake(string(ent.msg.Desc.Name())) + ".pb.orm.go"
		g := p.NewGeneratedFile(filename, file.GoImportPath)

		generateEntityHeader(g, file, entHasTimestamp, ent.softDelete, ent.tenantField != nil)
		generateEntityCode(g, ent, entHasTimestamp)
	}

	return nil
}

// hasEntityAnnotation checks if a message has the forge.v1.entity extension.
func hasEntityAnnotation(msg *protogen.Message) bool {
	opts := msg.Desc.Options()
	if opts == nil {
		return false
	}
	ext := proto.GetExtension(opts, forgev1.E_Entity)
	if ext == nil {
		return false
	}
	eo, ok := ext.(*forgev1.EntityOptions)
	return ok && eo != nil
}