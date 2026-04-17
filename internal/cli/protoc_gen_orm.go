package cli

import (
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const (
	entityOptionsFieldNum protoreflect.FieldNumber = 50200
	fieldOptionsFieldNum  protoreflect.FieldNumber = 50300
)

func newProtocGenOrmCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "protoc-gen-forge-orm",
		Short:  "Protoc plugin for ORM code generation (invoked by buf)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			protogen.Options{}.Run(func(p *protogen.Plugin) error {
				for _, f := range p.Files {
					if !f.Generate {
						continue
					}
					if err := generateOrmFile(p, f); err != nil {
						return err
					}
				}
				return nil
			})
			return nil
		},
	}
}

func generateOrmFile(p *protogen.Plugin, file *protogen.File) error {
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
	sharedFilename := file.GeneratedFilenamePrefix + "_orm_shared.pb.orm.go"
	shared := p.NewGeneratedFile(sharedFilename, file.GoImportPath)
	generateSharedHeader(shared, file, anyHasTimestamp)

	// Generate per-entity files.
	for _, ent := range entities {
		entHasTimestamp := false
		for _, f := range ent.fields {
			if f.isTimestamp {
				entHasTimestamp = true
				break
			}
		}

		filename := file.GeneratedFilenamePrefix + "_" + naming.ToSnakeCase(string(ent.msg.Desc.Name())) + ".pb.orm.go"
		g := p.NewGeneratedFile(filename, file.GoImportPath)

		generateEntityHeader(g, file, entHasTimestamp, ent.softDelete)
		generateEntityCode(g, ent, entHasTimestamp)
	}

	return nil
}
