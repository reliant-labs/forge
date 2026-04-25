package codegen

import (
	"strings"
	"unicode"
)

// FrontendHookTemplateData holds data for rendering a single service's
// TypeScript React Query hooks file.
type FrontendHookTemplateData struct {
	ServiceName      string               // e.g., "UserService"
	ServiceNameCamel string               // e.g., "userService"
	ImportPath       string               // e.g., "services/users/v1/users_pb"
	Methods          []FrontendHookMethod
}

// FrontendHookMethod represents a single unary RPC method for hook generation.
type FrontendHookMethod struct {
	Name       string // PascalCase: "GetUser"
	NameCamel  string // camelCase: "getUser"
	InputType  string // "GetUserRequest"
	OutputType string // "GetUserResponse"
	IsQuery    bool   // true for Get/List/Search, false for mutations
}

// queryPrefixes are RPC name prefixes that indicate a read-only query.
var queryPrefixes = []string{
	"Get", "List", "Search", "Find",
	"Check", "Has", "Is", "Count", "Exists",
}

// isQueryMethod returns true if the method name starts with a read-only prefix.
func isQueryMethod(name string) bool {
	for _, prefix := range queryPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// toCamelCaseFromPascal converts PascalCase to camelCase by lowering the first
// run of uppercase letters. "GetUser" → "getUser", "RPCMethod" → "rpcMethod".
func toCamelCaseFromPascal(s string) string {
	if s == "" {
		return s
	}
	runes := []rune(s)
	// Find the end of the initial uppercase run.
	i := 0
	for i < len(runes) && unicode.IsUpper(runes[i]) {
		i++
	}
	if i == 0 {
		return s
	}
	// If the entire string is uppercase, lower it all.
	if i == len(runes) {
		return strings.ToLower(s)
	}
	// If multiple uppercase letters precede a lowercase, keep the last
	// uppercase as part of the next word: "RPCMethod" → "rpcMethod".
	if i > 1 {
		i--
	}
	return strings.ToLower(string(runes[:i])) + string(runes[i:])
}

// ProtoFileToTSImportPath converts a proto file path to the TypeScript import
// path used by the buf ES plugin. For example:
//
//	"proto/services/users/v1/users.proto" → "services/users/v1/users_pb"
//
// The buf ES plugin strips the leading proto/ directory and replaces the .proto
// extension with _pb.
func ProtoFileToTSImportPath(protoFile string) string {
	// Strip leading "proto/" if present.
	p := strings.TrimPrefix(protoFile, "proto/")
	// Strip .proto extension and append _pb.
	p = strings.TrimSuffix(p, ".proto") + "_pb"
	return p
}

// ServiceDefToHookData converts a ServiceDef to FrontendHookTemplateData,
// skipping streaming RPCs.
func ServiceDefToHookData(svc ServiceDef) FrontendHookTemplateData {
	data := FrontendHookTemplateData{
		ServiceName:      svc.Name,
		ServiceNameCamel: toCamelCaseFromPascal(svc.Name),
		ImportPath:       ProtoFileToTSImportPath(svc.ProtoFile),
	}

	for _, m := range svc.Methods {
		// Skip streaming RPCs — only generate hooks for unary.
		if m.ClientStreaming || m.ServerStreaming {
			continue
		}

		data.Methods = append(data.Methods, FrontendHookMethod{
			Name:       m.Name,
			NameCamel:  toCamelCaseFromPascal(m.Name),
			InputType:  m.InputType,
			OutputType: m.OutputType,
			IsQuery:    isQueryMethod(m.Name),
		})
	}

	return data
}