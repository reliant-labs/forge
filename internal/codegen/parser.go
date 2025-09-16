package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// ServiceDef represents a parsed Connect RPC service definition
type ServiceDef struct {
	Name       string   // "EchoService"
	Package    string   // "echo.v1"
	GoPackage  string   // "github.com/.../gen/proto/echo/v1"
	PkgName    string   // "echov1"
	Methods    []Method
	ProtoFile  string
	ModulePath string // e.g., "github.com/demo-project"
}

// Method represents a single RPC method
type Method struct {
	Name           string
	InputType      string
	OutputType     string
	ClientStreaming bool
	ServerStreaming bool
}

// IsInputEmpty returns true if the input type is google.protobuf.Empty.
func (m Method) IsInputEmpty() bool {
	return m.InputType == "google.protobuf.Empty"
}

// IsOutputEmpty returns true if the output type is google.protobuf.Empty.
func (m Method) IsOutputEmpty() bool {
	return m.OutputType == "google.protobuf.Empty"
}

// GoInputType returns the Go type reference for the input (handles Empty).
func (m Method) GoInputType() string {
	if m.IsInputEmpty() {
		return "emptypb.Empty"
	}
	return "pb." + m.InputType
}

// GoOutputType returns the Go type reference for the output (handles Empty).
func (m Method) GoOutputType() string {
	if m.IsOutputEmpty() {
		return "emptypb.Empty"
	}
	return "pb." + m.OutputType
}

// ParseServicesFromProtos scans the given proto services directory and extracts
// service definitions. projectDir is the project root that contains go.mod.
func ParseServicesFromProtos(dir string, projectDir string) ([]ServiceDef, error) {
	var services []ServiceDef

	// Get module path from go.mod in the project root
	modulePath, err := GetModulePath(projectDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read module path: %w", err)
	}

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		svcDefs, err := parseProtoFile(path, modulePath)
		if err != nil {
			return fmt.Errorf("failed to parse %s: %w", path, err)
		}

		services = append(services, svcDefs...)
		return nil
	})

	return services, err
}

func parseProtoFile(path string, modulePath string) ([]ServiceDef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	handler := reporter.NewHandler(reporter.NewReporter(
		func(err reporter.ErrorWithPos) error { return err },
		nil,
	))

	fileNode, err := parser.Parse(path, strings.NewReader(string(data)), handler)
	if err != nil {
		return nil, fmt.Errorf("proto parse error: %w", err)
	}

	// Extract file-level metadata: package and go_package option.
	var pkg string
	var goPackage string
	var pkgName string

	for _, decl := range fileNode.Decls {
		switch n := decl.(type) {
		case *ast.PackageNode:
			pkg = string(n.Name.AsIdentifier())

		case *ast.OptionNode:
			optName := optionNodeName(n)
			if optName == "go_package" {
				if sv, ok := n.Val.(ast.StringValueNode); ok {
					raw := sv.AsString()
					goPackage, pkgName = parseGoPackageValue(raw)
				}
			}
		}
	}

	// Walk file declarations again for services.
	var services []ServiceDef
	for _, decl := range fileNode.Decls {
		svcNode, ok := decl.(*ast.ServiceNode)
		if !ok {
			continue
		}

		svc := ServiceDef{
			Name:       string(svcNode.Name.AsIdentifier()),
			Package:    pkg,
			GoPackage:  goPackage,
			PkgName:    pkgName,
			ProtoFile:  path,
			ModulePath: modulePath,
		}

		for _, elem := range svcNode.Decls {
			rpcNode, ok := elem.(*ast.RPCNode)
			if !ok {
				continue
			}

			method := Method{
				Name:           string(rpcNode.Name.AsIdentifier()),
				InputType:      string(rpcNode.Input.MessageType.AsIdentifier()),
				OutputType:     string(rpcNode.Output.MessageType.AsIdentifier()),
				ClientStreaming: rpcNode.Input.Stream != nil,
				ServerStreaming: rpcNode.Output.Stream != nil,
			}
			svc.Methods = append(svc.Methods, method)
		}

		services = append(services, svc)
	}

	// Validate go_package was found if there are services
	if len(services) > 0 && goPackage == "" {
		return nil, fmt.Errorf("%s: go_package option not found but file defines services", path)
	}

	return services, nil
}

// optionNodeName returns the simple string name of an option (e.g. "go_package").
func optionNodeName(n *ast.OptionNode) string {
	if n.Name == nil || len(n.Name.Parts) == 0 {
		return ""
	}
	// For simple options like go_package, there is one non-extension part.
	if len(n.Name.Parts) == 1 && !n.Name.Parts[0].IsExtension() {
		return n.Name.Parts[0].Value()
	}
	return ""
}

// parseGoPackageValue parses a go_package option value.
// Supports both "path/to/pkg;alias" and "path/to/pkg" forms.
func parseGoPackageValue(raw string) (goPackage, pkgName string) {
	if idx := strings.Index(raw, ";"); idx >= 0 {
		goPackage = raw[:idx]
		pkgName = raw[idx+1:]
	} else {
		goPackage = raw
		parts := strings.Split(goPackage, "/")
		pkgName = parts[len(parts)-1]
		pkgName = strings.ReplaceAll(pkgName, ".", "")
	}
	return
}

// GetModulePath reads the module path from go.mod in the given directory.
func GetModulePath(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "module ") {
			return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "module ")), nil
		}
	}

	return "", fmt.Errorf("module directive not found in go.mod")
}
