package codegen

import (
	"sort"
	"strings"
	"unicode"

	"github.com/jinzhu/inflection"
)

// FrontendHookTemplateData holds data for rendering a single service's
// TypeScript React Query hooks file.
type FrontendHookTemplateData struct {
	ServiceName      string // e.g., "UserService"
	ServiceNameCamel string // e.g., "userService"
	ImportPath       string // e.g., "services/users/v1/users_pb" — the service's own proto file
	Methods          []FrontendHookMethod
	// HasQueries / HasMutations let the template conditionally import
	// only the hooks it actually uses. Without these flags the emitted
	// file pulls in useMutation/useQueryClient/UseMutationOptions for
	// query-only services, tripping no-unused-vars in eslint configs.
	HasQueries   bool
	HasMutations bool
	// SchemaImports groups input-message `Schema` value imports by their
	// declaring proto file's TS import path. The template emits one
	// `import { ...Schema }` statement per entry. Same-file schemas land
	// under the service's ImportPath; cross-file schemas land under their
	// own proto file's derived path. Sorted for deterministic output.
	SchemaImports []HookImportGroup
	// TypeImports groups output-message `type` imports the same way.
	// Kept separate from SchemaImports because the template emits these
	// as `import type { ... }` — value vs type-only is required so a
	// `--isolatedModules` build still tree-shakes the type-only side.
	TypeImports []HookImportGroup
	// Workspaces is true when the project opted into the pnpm-workspace
	// layout (frontend.workspaces: true). When true, the rendered hook
	// file lives under packages/hooks/src/generated/ and imports
	// connectClient from "../transport" + proto types from the
	// project's @<scope>/api workspace. When false (the default), the
	// file lives under frontends/<name>/src/hooks/ and imports from the
	// frontend-local @/lib/connect + @/gen paths — byte-identical to
	// projects that predate the workspaces flag.
	Workspaces bool
	// ApiPackage is the workspace package name for the shared API
	// (e.g. "@myapp/api"). Empty when Workspaces is false.
	ApiPackage string
	// EntityScopes is the sorted, deduplicated set of camelCase CRUD
	// entity names ("task", "user") derived from the service's RPC
	// names. The template emits one entity-scope key per entry in the
	// generated query-key factory so mutations can invalidate ONLY the
	// queries for the entity they touched (entity-scoped invalidation)
	// instead of nuking every query on the service.
	EntityScopes []string
}

// HookImportGroup is one TS import statement: a list of symbols (sorted,
// deduplicated) drawn from a single source proto file. The template emits
// one statement per group so cross-proto-file refs resolve to the
// declaring _pb.ts file.
type HookImportGroup struct {
	ImportPath string   // e.g., "services/users/v1/users_pb" or "shared/v1/types_pb"
	Symbols    []string // sorted, deduplicated identifiers
}

// FrontendHookMethod represents a single unary RPC method for hook generation.
type FrontendHookMethod struct {
	Name       string // PascalCase: "GetUser"
	NameCamel  string // camelCase: "getUser"
	InputType  string // "GetUserRequest"
	OutputType string // "GetUserResponse"
	IsQuery    bool   // true for Get/List/Search, false for mutations
	// EntityScope is the camelCase singular CRUD entity this method
	// operates on ("task" for ListTasks/GetTask/CreateTask), derived
	// from the RPC-name CRUD pattern. Empty for non-CRUD methods.
	// Queries embed it in their query key ([service, entity, method,
	// req]); mutations invalidate the [service, entity] scope when set,
	// falling back to the whole-service scope when empty (a bespoke
	// mutation may touch anything, so over-invalidating is the safe
	// default there).
	EntityScope string
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

// ToCamelCaseFromPascalExport is the exported wrapper around the
// package-internal helper. Callers outside this package (the frontend
// hooks barrel generator deriving a namespace alias from a service name)
// use it so the camelCase rules stay in lockstep across packages.
func ToCamelCaseFromPascalExport(s string) string {
	return toCamelCaseFromPascal(s)
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

	// schemasByPath / typesByPath collect symbol -> tspath buckets. We use
	// sets keyed on symbol name to dedupe: the same Request type may be
	// referenced by multiple RPCs, and the same Response type may appear
	// in both queries and mutations.
	schemasByPath := map[string]map[string]struct{}{}
	typesByPath := map[string]map[string]struct{}{}

	addSym := func(buckets map[string]map[string]struct{}, path, sym string) {
		set, ok := buckets[path]
		if !ok {
			set = map[string]struct{}{}
			buckets[path] = set
		}
		set[sym] = struct{}{}
	}

	for _, m := range svc.Methods {
		// Skip streaming RPCs — only generate hooks for unary.
		if m.ClientStreaming || m.ServerStreaming {
			continue
		}

		isQuery := isQueryMethod(m.Name)
		if isQuery {
			data.HasQueries = true
		} else {
			data.HasMutations = true
		}

		// The Service value still lives in the service's own _pb.ts, so
		// ImportPath stays as svc.ProtoFile's derived path. But each
		// RPC's InputSchema (value import) and Output type (type-only
		// import) come from the file that physically declares them,
		// which may differ from svc.ProtoFile for cross-file refs.
		// Falling back to svc.ProtoFile when InputProtoFile/
		// OutputProtoFile are empty keeps legacy descriptor.json files
		// (written before the cross-file fix landed) producing valid
		// imports rather than `import "@/gen/_pb"`.
		inPath := m.InputProtoFile
		if inPath == "" {
			inPath = svc.ProtoFile
		}
		outPath := m.OutputProtoFile
		if outPath == "" {
			outPath = svc.ProtoFile
		}
		addSym(schemasByPath, ProtoFileToTSImportPath(inPath), m.InputType+"Schema")
		addSym(typesByPath, ProtoFileToTSImportPath(outPath), m.OutputType)

		data.Methods = append(data.Methods, FrontendHookMethod{
			Name:        m.Name,
			NameCamel:   toCamelCaseFromPascal(m.Name),
			InputType:   m.InputType,
			OutputType:  m.OutputType,
			IsQuery:     isQuery,
			EntityScope: methodEntityScope(m.Name),
		})
	}

	// The Service descriptor itself is a value import from the service's
	// own _pb.ts. Folding it into the schema buckets (instead of a
	// dedicated template line) guarantees ONE import statement per source
	// module — a separate statement for the service tripped
	// import/no-duplicates whenever a request schema lived in the same
	// file (the common case).
	if len(data.Methods) > 0 {
		addSym(schemasByPath, data.ImportPath, svc.Name)
	}

	data.SchemaImports = flattenImportGroups(schemasByPath)
	data.TypeImports = flattenImportGroups(typesByPath)

	scopeSet := map[string]struct{}{}
	for _, m := range data.Methods {
		if m.EntityScope != "" {
			scopeSet[m.EntityScope] = struct{}{}
		}
	}
	for s := range scopeSet {
		data.EntityScopes = append(data.EntityScopes, s)
	}
	sort.Strings(data.EntityScopes)

	return data
}

// methodEntityScope derives the camelCase singular entity name from a
// CRUD-pattern RPC name: "ListTasks" → "task", "CreateTask" → "task".
// Returns "" for non-CRUD method names — those key/invalidate at the
// service scope.
func methodEntityScope(methodName string) string {
	op, rawEntity := parseCRUDOperation(methodName)
	if op == "" || rawEntity == "" {
		return ""
	}
	entity := rawEntity
	if op == "list" {
		entity = inflection.Singular(rawEntity)
	}
	return toCamelCaseFromPascal(entity)
}

// flattenImportGroups converts a path -> symbol-set map into a sorted
// []HookImportGroup with sorted, deduplicated symbol slices. Sorting at
// both levels makes the rendered TS deterministic byte-for-byte across
// runs, which the snapshot-style codegen tests rely on.
func flattenImportGroups(buckets map[string]map[string]struct{}) []HookImportGroup {
	if len(buckets) == 0 {
		return nil
	}
	paths := make([]string, 0, len(buckets))
	for p := range buckets {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := make([]HookImportGroup, 0, len(paths))
	for _, p := range paths {
		set := buckets[p]
		syms := make([]string, 0, len(set))
		for s := range set {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		out = append(out, HookImportGroup{ImportPath: p, Symbols: syms})
	}
	return out
}
