//go:build ignore

package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// GeneratePlanProtoFile writes a populated proto file for a service based on
// plan RPC definitions. It overwrites any existing proto stub at
// proto/services/<name>/v1/<name>.proto.
func GeneratePlanProtoFile(root, modulePath, serviceName string, rpcs []config.PlanRPC) error {
	protoDir := filepath.Join(root, "proto", "services", serviceName, "v1")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		return fmt.Errorf("create proto directory: %w", err)
	}

	handlerName := naming.ToPascalCase(serviceName)
	needsTimestamp := planRPCsNeedTimestamp(rpcs)

	var b strings.Builder

	// Header
	b.WriteString("syntax = \"proto3\";\n\n")
	fmt.Fprintf(&b, "package services.%s.v1;\n\n", serviceName)
	fmt.Fprintf(&b, "option go_package = \"%s/gen/services/%s/v1;%sv1\";\n", modulePath, serviceName, serviceName)

	// Imports
	if needsTimestamp {
		b.WriteString("\nimport \"google/protobuf/timestamp.proto\";\n")
	}

	// Service block
	fmt.Fprintf(&b, "\n// %sService defines the %s service RPCs.\n", handlerName, serviceName)
	fmt.Fprintf(&b, "service %sService {\n", handlerName)
	for _, rpc := range rpcs {
		if rpc.Description != "" {
			fmt.Fprintf(&b, "  // %s\n", rpc.Description)
		}
		fmt.Fprintf(&b, "  rpc %s(%sRequest) returns (%sResponse) {}\n", rpc.Name, rpc.Name, rpc.Name)
	}
	b.WriteString("}\n")

	// Messages
	for _, rpc := range rpcs {
		b.WriteString("\n")
		fmt.Fprintf(&b, "message %sRequest {\n", rpc.Name)
		for i, f := range rpc.Request {
			fmt.Fprintf(&b, "  %s %s = %d;\n", mapProtoType(f.Type), f.Name, i+1)
		}
		b.WriteString("}\n")

		b.WriteString("\n")
		fmt.Fprintf(&b, "message %sResponse {\n", rpc.Name)
		for i, f := range rpc.Response {
			fmt.Fprintf(&b, "  %s %s = %d;\n", mapProtoType(f.Type), f.Name, i+1)
		}
		b.WriteString("}\n")
	}

	protoPath := filepath.Join(protoDir, fmt.Sprintf("%s.proto", serviceName))
	return os.WriteFile(protoPath, []byte(b.String()), 0644)
}

// mapProtoType converts a plan field type to a proto3 type string.
func mapProtoType(t string) string {
	if t == "timestamp" {
		return "google.protobuf.Timestamp"
	}
	// Everything else (string, int32, bool, repeated string, message Foo, etc.)
	// is used literally — proto validation will catch errors.
	return t
}

// planRPCsNeedTimestamp reports whether any RPC field uses the timestamp type.
func planRPCsNeedTimestamp(rpcs []config.PlanRPC) bool {
	for _, rpc := range rpcs {
		for _, f := range rpc.Request {
			if f.Type == "timestamp" {
				return true
			}
		}
		for _, f := range rpc.Response {
			if f.Type == "timestamp" {
				return true
			}
		}
	}
	return false
}