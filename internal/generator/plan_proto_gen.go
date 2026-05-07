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
// plan RPC definitions and entity messages. It overwrites any existing proto
// stub at proto/services/<name>/v1/<name>.proto.
//
// When entities are provided, their message definitions are emitted inline
// in the service proto (before the RPC request/response messages) so that
// CRUD RPCs can reference them by bare name.
func GeneratePlanProtoFile(root, modulePath, serviceName string, rpcs []config.PlanRPC, entities []config.PlanEntity) error {
	protoDir := filepath.Join(root, "proto", "services", serviceName, "v1")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		return fmt.Errorf("create proto directory: %w", err)
	}

	handlerName := naming.ToPascalCase(serviceName)
	needsTimestamp := planRPCsNeedTimestamp(rpcs) || planEntitiesNeedTimestamp(entities)
	entityImports := planRPCsEntityImports(rpcs)

	var b strings.Builder

	// Header
	b.WriteString("syntax = \"proto3\";\n\n")
	fmt.Fprintf(&b, "package services.%s.v1;\n\n", serviceName)
	fmt.Fprintf(&b, "option go_package = \"%s/gen/services/%s/v1;%sv1\";\n", modulePath, serviceName, serviceName)

	// Imports
	if needsTimestamp || len(entityImports) > 0 {
		b.WriteString("\n")
	}
	if needsTimestamp {
		b.WriteString("import \"google/protobuf/timestamp.proto\";\n")
	}
	for _, imp := range entityImports {
		fmt.Fprintf(&b, "import \"%s\";\n", imp)
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

	// Entity messages (before RPC request/response messages)
	for _, ent := range entities {
		b.WriteString("\n")
		// Emit forge annotations as comments so the entity parser can
		// reconstruct metadata without proto options.
		fmt.Fprintf(&b, "// forge:entity\n")
		if tenantField := planEntityTenantField(ent); tenantField != "" {
			fmt.Fprintf(&b, "// forge:tenant_key=%s\n", tenantField)
		}
		fmt.Fprintf(&b, "message %s {\n", ent.Name)
		fieldNum := 1

		// Always emit id as the first field unless the user already defined it.
		hasID := false
		for _, f := range ent.Fields {
			if f.Name == "id" {
				hasID = true
				break
			}
		}
		if !hasID {
			fmt.Fprintf(&b, "  string id = %d;\n", fieldNum)
			fieldNum++
		}

		for _, f := range ent.Fields {
			fmt.Fprintf(&b, "  %s %s = %d;\n", mapProtoType(f.Type), f.Name, fieldNum)
			fieldNum++
		}
		if ent.Timestamps {
			fmt.Fprintf(&b, "  google.protobuf.Timestamp created_at = %d;\n", fieldNum)
			fieldNum++
			fmt.Fprintf(&b, "  google.protobuf.Timestamp updated_at = %d;\n", fieldNum)
			fieldNum++
		}
		if ent.SoftDelete {
			fmt.Fprintf(&b, "  google.protobuf.Timestamp deleted_at = %d;\n", fieldNum)
			fieldNum++
		}
		b.WriteString("}\n")
	}

	// RPC Messages
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

// planEntityTenantField returns the name of the field explicitly marked as
// tenant_key in the entity, or empty string if none.
func planEntityTenantField(ent config.PlanEntity) string {
	for _, f := range ent.Fields {
		if f.TenantKey {
			return f.Name
		}
	}
	return ""
}

// mapProtoType converts a plan field type to a proto3 type string.
func mapProtoType(t string) string {
	if t == "timestamp" {
		return "google.protobuf.Timestamp"
	}
	// Everything else (string, int32, bool, repeated string,
	// google.protobuf.Timestamp, message Foo, etc.)
	// is used literally — proto validation will catch errors.
	return t
}

// planRPCsEntityImports collects db entity proto imports needed by the RPCs.
// It detects types like "db.v1.Patient" or "repeated db.v1.Patient" and returns
// the corresponding import paths (e.g. "db/v1/patient.proto").
func planRPCsEntityImports(rpcs []config.PlanRPC) []string {
	seen := map[string]bool{}
	var imports []string

	collect := func(t string) {
		// Strip "repeated " prefix if present.
		t = strings.TrimPrefix(t, "repeated ")
		if !strings.HasPrefix(t, "db.v1.") {
			return
		}
		// e.g. "db.v1.Patient" -> messageName = "Patient"
		msgName := strings.TrimPrefix(t, "db.v1.")
		snakeName := naming.ToSnakeCase(msgName)
		importPath := "db/v1/" + snakeName + ".proto"
		if !seen[importPath] {
			seen[importPath] = true
			imports = append(imports, importPath)
		}
	}

	for _, rpc := range rpcs {
		for _, f := range rpc.Request {
			collect(f.Type)
		}
		for _, f := range rpc.Response {
			collect(f.Type)
		}
	}
	return imports
}

// planRPCsNeedTimestamp reports whether any RPC field uses the timestamp type.
func planRPCsNeedTimestamp(rpcs []config.PlanRPC) bool {
	for _, rpc := range rpcs {
		for _, f := range rpc.Request {
			if f.Type == "timestamp" || f.Type == "google.protobuf.Timestamp" {
				return true
			}
		}
		for _, f := range rpc.Response {
			if f.Type == "timestamp" || f.Type == "google.protobuf.Timestamp" {
				return true
			}
		}
	}
	return false
}

// planEntitiesNeedTimestamp reports whether any entity uses timestamps or soft-delete
// (which require google.protobuf.Timestamp).
func planEntitiesNeedTimestamp(entities []config.PlanEntity) bool {
	for _, ent := range entities {
		if ent.Timestamps || ent.SoftDelete {
			return true
		}
		for _, f := range ent.Fields {
			if f.Type == "timestamp" || f.Type == "google.protobuf.Timestamp" {
				return true
			}
		}
	}
	return false
}