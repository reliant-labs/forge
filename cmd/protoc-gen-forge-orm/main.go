// protoc-gen-forge-orm is a protoc plugin that generates type-safe ORM
// code for proto messages annotated with EntityOptions and FieldOptions.
//
// For each annotated message it produces a <name>.pb.orm.go file containing:
//   - Model and Scanner interface implementations
//   - CRUD functions: Create, GetByID, List, Update, Delete
//
// Generated code uses github.com/reliant-labs/forge/pkg/orm.
package main

import (
	"github.com/reliant-labs/forge/internal/naming"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Extension field numbers from proto/forge/options/v1/entity.proto and field.proto.
const (
	entityOptionsFieldNum protoreflect.FieldNumber = 50200
	fieldOptionsFieldNum  protoreflect.FieldNumber = 50300
)

func main() {
	protogen.Options{}.Run(func(p *protogen.Plugin) error {
		for _, f := range p.Files {
			if !f.Generate {
				continue
			}
			if err := generateFile(p, f); err != nil {
				return err
			}
		}
		return nil
	})
}

func generateFile(p *protogen.Plugin, file *protogen.File) error {
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
