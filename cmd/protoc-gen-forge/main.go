package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/pluginpb"
)

// forgeMetadata contains extracted forge options
type forgeMetadata struct {
	Services map[string]*ServiceMetadata `json:"services"`
	Messages map[string]*MessageMetadata `json:"messages"`
}

type ServiceMetadata struct {
	Name         string                    `json:"name"`
	Package      string                    `json:"package"`
	Methods      []*MethodMetadata         `json:"methods"`
	Middleware   map[string]interface{}    `json:"middleware,omitempty"`
	Dependencies map[string][]string       `json:"dependencies,omitempty"`
	Config       map[string]interface{}    `json:"config,omitempty"`
}

type MethodMetadata struct {
	Name         string                 `json:"name"`
	InputType    string                 `json:"input_type"`
	OutputType   string                 `json:"output_type"`
	StreamInput  bool                   `json:"stream_input"`
	StreamOutput bool                   `json:"stream_output"`
	Options      map[string]interface{} `json:"options,omitempty"`
}

type MessageMetadata struct {
	Name   string                    `json:"name"`
	Fields []*FieldMetadata          `json:"fields"`
	Entity *EntityMetadata           `json:"entity,omitempty"`
}

type FieldMetadata struct {
	Name       string                 `json:"name"`
	Type       string                 `json:"type"`
	Number     int32                  `json:"number"`
	Repeated   bool                   `json:"repeated"`
	Optional   bool                   `json:"optional"`
	Database   map[string]interface{} `json:"database,omitempty"`
	Validation map[string]interface{} `json:"validation,omitempty"`
}

type EntityMetadata struct {
	Table      string   `json:"table"`
	Indexes    []string `json:"indexes"`
	SoftDelete bool     `json:"soft_delete"`
	Timestamps bool     `json:"timestamps"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--help" {
		fmt.Println("Protoc plugin for extracting forge metadata from proto files")
		fmt.Println("Usage: protoc --forge_out=. your_file.proto")
		return
	}

	// Read input from protoc
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input: %v\n", err)
		os.Exit(1)
	}

	// Parse the request
	var request pluginpb.CodeGeneratorRequest
	if err := proto.Unmarshal(input, &request); err != nil {
		fmt.Fprintf(os.Stderr, "Error parsing request: %v\n", err)
		os.Exit(1)
	}

	// Process with protogen
	options := protogen.Options{}
	plugin, err := options.New(&request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating plugin: %v\n", err)
		os.Exit(1)
	}

	// Extract metadata
	metadata := &forgeMetadata{
		Services: make(map[string]*ServiceMetadata),
		Messages: make(map[string]*MessageMetadata),
	}

	for _, file := range plugin.Files {
		if !file.Generate {
			continue
		}

		// Process services
		for _, service := range file.Services {
			svcMeta := extractServiceMetadata(service)
			metadata.Services[string(service.Desc.Name())] = svcMeta
		}

		// Process messages
		for _, message := range file.Messages {
			msgMeta := extractMessageMetadata(message)
			metadata.Messages[string(message.Desc.Name())] = msgMeta
		}
	}

	// Generate output file with metadata
	for _, file := range plugin.Files {
		if !file.Generate {
			continue
		}

		// Create metadata file
		metadataFile := plugin.NewGeneratedFile(
			file.GeneratedFilenamePrefix+".forge.json",
			file.GoImportPath,
		)

		// Write metadata as JSON
		jsonData, err := json.MarshalIndent(metadata, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error marshaling metadata: %v\n", err)
			os.Exit(1)
		}

		if _, err := metadataFile.Write(jsonData); err != nil {
			fmt.Fprintf(os.Stderr, "Error writing metadata: %v\n", err)
			os.Exit(1)
		}
	}

	// Generate response
	response := plugin.Response()
	output, err := proto.Marshal(response)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error marshaling response: %v\n", err)
		os.Exit(1)
	}

	if _, err := os.Stdout.Write(output); err != nil {
		fmt.Fprintf(os.Stderr, "Error writing output: %v\n", err)
		os.Exit(1)
	}
}

func extractServiceMetadata(service *protogen.Service) *ServiceMetadata {
	meta := &ServiceMetadata{
		Name:    string(service.Desc.Name()),
		Package: string(service.Desc.ParentFile().Package()),
		Methods: make([]*MethodMetadata, 0),
	}

	// Extract service options (if we had custom options defined)
	// For now, we'll use comments as a workaround
	if service.Comments.Leading != "" {
		// Parse comments for @middleware, @dependencies, etc.
		meta.Middleware = extractOptionsFromComments(service.Comments.Leading.String(), "@middleware")
		meta.Dependencies = extractDependenciesFromComments(service.Comments.Leading.String())
	}

	// Process methods
	for _, method := range service.Methods {
		methodMeta := &MethodMetadata{
			Name:         string(method.Desc.Name()),
			InputType:    string(method.Input.Desc.Name()),
			OutputType:   string(method.Output.Desc.Name()),
			StreamInput:  method.Desc.IsStreamingClient(),
			StreamOutput: method.Desc.IsStreamingServer(),
		}

		// Extract method options from comments
		if method.Comments.Leading != "" {
			methodMeta.Options = extractOptionsFromComments(method.Comments.Leading.String(), "@")
		}

		meta.Methods = append(meta.Methods, methodMeta)
	}

	return meta
}

func extractMessageMetadata(message *protogen.Message) *MessageMetadata {
	meta := &MessageMetadata{
		Name:   string(message.Desc.Name()),
		Fields: make([]*FieldMetadata, 0),
	}

	// Check for entity annotation in comments
	if message.Comments.Leading != "" {
		if entity := extractEntityFromComments(message.Comments.Leading.String()); entity != nil {
			meta.Entity = entity
		}
	}

	// Process fields
	for _, field := range message.Fields {
		fieldMeta := &FieldMetadata{
			Name:     string(field.Desc.Name()),
			Type:     field.Desc.Kind().String(),
			Number:   int32(field.Desc.Number()),
			Repeated: field.Desc.Cardinality() == protoreflect.Repeated,
			Optional: field.Desc.HasOptionalKeyword(),
		}

		// Extract field options from comments
		if field.Comments.Leading != "" {
			fieldMeta.Database = extractOptionsFromComments(field.Comments.Leading.String(), "@db")
			fieldMeta.Validation = extractOptionsFromComments(field.Comments.Leading.String(), "@validate")
		}

		meta.Fields = append(meta.Fields, fieldMeta)
	}

	return meta
}

// TODO: Implement option extraction from compiled proto descriptors.
// These functions currently return empty results. The plugin still outputs
// structural metadata (service/method/message/field names and types) which
// is useful, but custom annotations (@middleware, @db, @validate, @entity)
// are not yet parsed. When implemented, these should read from proto
// extension options rather than comment parsing.

func extractOptionsFromComments(_ string, _ string) map[string]interface{} {
	return nil
}

func extractDependenciesFromComments(_ string) map[string][]string {
	return nil
}

func extractEntityFromComments(_ string) *EntityMetadata {
	return nil
}