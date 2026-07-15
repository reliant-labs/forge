package codegen

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// CRUDMethod holds the correlation between an RPC method and a database entity.
type CRUDMethod struct {
	Method    MethodTemplateData // The RPC
	Entity    EntityDef          // The matched entity
	Operation string             // "create", "get", "list", "update", "delete"
}

// CRUDTemplateData holds all data needed to render the CRUD handlers template.
type CRUDTemplateData struct {
	Package       string // Go package name, e.g. "patients"
	Module        string // Go module path, e.g. "github.com/demo-project"
	ProtoPackage  string // e.g. "proto/services/patients"
	DBPackagePath string // e.g. "github.com/demo-project/gen/db/v1"
	HasPagination bool   // true if any list method uses pagination
	HasFilters    bool   // true if any list method has filter fields
	HasOrderBy    bool   // true if any list method has order_by
	NeedsORM      bool   // true if pagination, filters, or ordering requires orm import
	HasTenant     bool   // true if any CRUD method operates on a tenant-scoped entity
	// NeedsCRUDLib is true when at least one method emits a real CRUD
	// body (i.e. uses pkg/crud, internal/db, middleware). When every
	// method's request/response shape failed validation and we emit
	// only TODO stubs, the template skips those imports to keep the
	// file compiling.
	NeedsCRUDLib bool
	// NeedsOpsFile is true when the Tier-1 ops file must be written:
	// either NeedsCRUDLib (a real op constructor) OR there is at least
	// one entity conversion pair a wired custom-read-shape body depends
	// on. An all-custom service still needs the conversion helpers, so
	// it gets an ops file carrying only the <entity>ToProto/FromProto
	// pairs (no crud/middleware imports — see the template gating).
	NeedsOpsFile bool
	CRUDMethods  []CRUDMethodTemplateData
	// Entities carries the per-entity proto<->struct conversion pairs
	// (<entity>ToProto / <entity>FromProto) emitted alongside the ops.
	Entities []EntityConvTemplateData
	// NeedsTimestamppb gates the timestamppb import (set when any
	// conversion touches a timestamp column).
	NeedsTimestamppb bool
}

// CRUDMethodTemplateData holds per-method template data.
type CRUDMethodTemplateData struct {
	MethodName        string // "CreatePatient"
	InputType         string // "CreatePatientRequest"
	OutputType        string // "CreatePatientResponse"
	EntityName        string // "Patient"
	EntityLower       string // "patient"
	Operation         string // "create", "get", "list", "update", "delete"
	AuthRequired      bool
	AuthAction        string // "create", "read", "list", "update", "delete" (middleware constant)
	PkField           string // "Id" (proto PascalCase Go field name)
	PkColumnName      string // "id" (raw DB column name)
	PkGoType          string // "int64"
	HasPkInInput      bool   // true if the request message likely has an ID field
	ResponseField     string // "Patient" — the proto field name in the response that holds the entity
	HasPagination     bool   // true when List method's InputType follows AIP-158 convention
	PaginationStyle   string // "cursor" (default for now)
	HasFilters        bool   // true if list method has filter fields
	FilterFields      []FilterFieldData
	HasOrderBy        bool   // true if list method has order_by field
	HasTenant         bool   // true when the entity has a tenant key field
	TenantGoName      string // e.g., "OrgId", "TenantId" (PascalCase Go field name on entity)
	TenantColumnName  string // e.g., "org_id", "tenant_id"
	UpdateEntityField string // e.g., "Project" — Go field name in the update request that holds the entity
	// UpdateMaskField is the Go field name of the update request's
	// google.protobuf.FieldMask field (e.g. "UpdateMask"). Empty when the
	// request carries no mask — the generated UpdateOp then omits
	// Mask/PersistMasked and pkg/crud keeps the legacy full-replace path.
	// When set, the op wires both hooks so HandleUpdate honors AIP-134:
	// concrete mask paths write only the named columns via
	// db.Update<Entity>Masked.
	UpdateMaskField string
	CreateFields    []CreateFieldData // fields from the create request message
	// ShapeMismatch is true when the request/response message shapes
	// observed in svc.Messages don't line up with what the CRUD body
	// templates assume (AIP-158 page_size/page_token for list, an `id`
	// scalar key for get/update/delete, an entity-typed response field,
	// etc.). That's a legitimate domain decision, not an error — the
	// template emits a tagged stub returning CodeUnimplemented rather
	// than CRUD-body code that wouldn't compile against the real proto,
	// and the user implements the custom shape in the owned shim. See
	// validateCRUDShape for the rules. The stub carries a
	// `forge:custom-read-shape` marker plus MismatchReason so the user
	// (and `forge audit`) can spot it. (Markers emitted before this
	// release spelled it FORGE_CRUD_SHAPE_MISMATCH; audit still
	// recognizes that string for one release.)
	ShapeMismatch  bool
	MismatchReason string
	// CustomFilters are the request fields that DID map to a declared
	// column on the entity (best-effort), used to seed the wired
	// custom-read-shape body's []orm.QueryOption skeleton. Unlike the
	// strict FilterFields (which fail the generate on an unmappable
	// field), these are advisory: fields that don't map to a column are
	// silently skipped so the scaffold always compiles. Only populated
	// when ShapeMismatch is true.
	CustomFilters []FilterFieldData
	// CreateAssigns are the precomputed `e.X = req.X` statements that
	// map create-request fields onto the entity struct (with timestamp/
	// wrapper/array/width conversions baked in).
	CreateAssigns []string
}

// CreateFieldData holds a field mapping from a create request to the ORM entity.
type CreateFieldData struct {
	ProtoGoName  string    // Go field name on the proto request message, e.g. "Name"
	EntityGoName string    // Go field name on the ORM entity, e.g. "Name"
	Kind         FieldKind // scalar, enum, message, wrapper, timestamp, etc.
	GoType       string    // Go type: "string", "int32", "*timestamppb.Timestamp", etc.
	EnumGoType   string    // For enum fields: the pb.EnumType name
}

// FilterFieldData describes a filter field extracted from a List request message.
type FilterFieldData struct {
	ProtoName  string // e.g., "active", "search", "status"
	GoName     string // PascalCase: "Active", "Search", "Status"
	ColumnName string // DB column: "active", "status"
	FieldType  string // "bool", "string", "int32", "int64"
	FilterType string // "exact", "search"
	IsOptional bool   // proto optional keyword
	// SearchColumns is the entity's declared string columns (minus the
	// PK) that a "search" filter spans via orm.WhereILikeAny. A search
	// field never maps to a column of its own — the historical
	// WhereILike("search", ...) hit a phantom column and either errored
	// or (SQLite double-quote fallback) silently matched nothing.
	SearchColumns []string
}

// MatchCRUDMethods correlates a service's RPC methods with entity definitions
// and returns the matched CRUD methods. Only unary RPCs are matched.
func MatchCRUDMethods(svc ServiceDef, entities []EntityDef) []CRUDMethod {
	entityMap := make(map[string]EntityDef)
	for _, e := range entities {
		entityMap[strings.ToLower(e.Name)] = e
	}

	var matches []CRUDMethod
	for _, m := range svc.Methods {
		// Skip streaming methods — CRUD is unary only
		if m.ClientStreaming || m.ServerStreaming {
			continue
		}

		op, entityName := parseCRUDOperation(m.Name)
		if op == "" {
			continue
		}

		// Try to find the entity (case-insensitive)
		entity, ok := entityMap[strings.ToLower(entityName)]
		if !ok {
			// For "list", the method name uses plural — try singular
			if op == "list" {
				singular := inflection.Singular(entityName)
				entity, ok = entityMap[strings.ToLower(singular)]
			}
			if !ok {
				continue
			}
		}

		mtd := MethodTemplateData{
			Name:         m.Name,
			InputType:    m.InputType,
			OutputType:   m.OutputType,
			AuthRequired: m.AuthRequired,
		}

		matches = append(matches, CRUDMethod{
			Method:    mtd,
			Entity:    entity,
			Operation: op,
		})
	}
	return matches
}

// ParseCRUDOperation extracts the CRUD operation and entity name from a
// method name. Returns ("", "") if the method doesn't match a CRUD
// pattern. Exported so the CLI's webhook-only detection can ask the
// same question MatchCRUDMethods does without re-implementing the
// prefix list. Internal callers should keep using parseCRUDOperation;
// they're identical.
func ParseCRUDOperation(methodName string) (operation, entityName string) {
	return parseCRUDOperation(methodName)
}

// parseCRUDOperation extracts the CRUD operation and entity name from a method name.
// Returns ("", "") if the method doesn't match a CRUD pattern.
func parseCRUDOperation(methodName string) (operation, entityName string) {
	prefixes := []struct {
		prefix string
		op     string
	}{
		{"Create", "create"},
		{"Get", "get"},
		{"List", "list"},
		{"Update", "update"},
		{"Delete", "delete"},
	}

	for _, p := range prefixes {
		if strings.HasPrefix(methodName, p.prefix) {
			name := strings.TrimPrefix(methodName, p.prefix)
			if name != "" {
				return p.op, name
			}
		}
	}
	return "", ""
}

// GenerateCRUDHandlers generates handlers_crud_gen.go for a service with CRUD methods.
// It skips methods that already exist in user-owned handler files.
//
// cs is the project's checksum tracker. Passing it ensures the rendered
// handlers_crud_gen.go is recorded so it doesn't show up as an orphan in
// `forge audit`. A nil cs is tolerated.
func GenerateCRUDHandlers(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string, cs *checksums.FileChecksums) error {
	// Disk-first: handlers_crud_gen.go lands inside the EXISTING handler
	// dir and must declare that dir's real package clause. Re-synthesizing
	// the path from the proto name is how the historical
	// handlers/adminserver-vs-admin_server duplicate-dir bug was created.
	res, resErr := ResolveServiceComponent(projectDir, svc.Name)
	if resErr != nil {
		return resErr
	}
	pkg := res.PackageName
	targetDir := res.Dir

	// Scan existing user-owned methods to avoid generating duplicates
	existingMethods, err := scanExistingMethods(targetDir, false)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scan existing methods for %s: %w", pkg, err)
	}

	// Filter out methods that already exist
	var filteredMethods []CRUDMethod
	for _, cm := range crudMethods {
		if existingMethods[cm.Method.Name] {
			continue
		}
		filteredMethods = append(filteredMethods, cm)
	}

	relDir := filepath.Join("internal", "handlers", filepath.FromSlash(res.ImportLeaf))
	opsRel := filepath.Join(relDir, "handlers_crud_ops_gen.go")

	// The pre-split Tier-1 implementation file is dead: forge no longer
	// emits RPC method bodies as generated code. Sweep it (and its
	// manifest entry) unless the user took ownership of it. Snapshot the
	// methods it really implemented FIRST: if one of them is about to be
	// re-scaffolded as a custom-read-shape stub, that replaces a working
	// implementation with CodeUnimplemented and must be LOUD — a
	// downstream agent had a near-miss with live traffic on exactly this.
	previouslyImplemented := implementedMethodsIn(filepath.Join(projectDir, relDir, "handlers_crud_gen.go"))
	removeRetiredForgeFile(projectDir, filepath.Join(relDir, "handlers_crud_gen.go"), cs)

	if len(filteredMethods) == 0 {
		// No CRUD methods left for forge to wire — drop the ops file.
		removeRetiredForgeFile(projectDir, opsRel, cs)
		return nil
	}

	// Ensure the Deps struct in service.go has a DB field for CRUD operations.
	if err := ensureDepsDBField(targetDir); err != nil {
		return fmt.Errorf("ensure Deps DB field for %s: %w", pkg, err)
	}

	// Build template data. Package is overridden with the disk-resolved
	// clause so the emitted file always matches the directory it lands in
	// (buildCRUDTemplateData's synthesis only holds for fresh scaffolds).
	// A filter-mapping failure here is a hard error by design: a filter
	// field that maps to no declared column must fail the generate, not
	// ship a phantom-column query.
	data, err := buildCRUDTemplateData(svc, filteredMethods, modulePath)
	if err != nil {
		return err
	}
	data.Package = pkg

	// Tier-1 projection: the per-entity wiring ops + conversion helpers.
	// Emitted when at least one method passed shape validation OR a wired
	// custom-read-shape body needs the entity conversion helpers (an
	// all-custom service still gets the <entity>ToProto/FromProto pairs).
	if data.NeedsOpsFile {
		content, rerr := templates.ServiceTemplates().Render("handlers_crud_ops_gen.go.tmpl", data)
		if rerr != nil {
			return fmt.Errorf("render handlers_crud_ops_gen.go.tmpl: %w", rerr)
		}
		if werr := writeForgeOwned(projectDir, opsRel, content, cs); werr != nil {
			return fmt.Errorf("write handlers_crud_ops_gen.go: %w", werr)
		}
	} else {
		removeRetiredForgeFile(projectDir, opsRel, cs)
	}

	// User-owned implementation: scaffold handlers_crud.go once; on later
	// runs append shims for newly-added CRUD RPCs only (existing content
	// is never modified).
	return ensureCRUDShimFile(projectDir, relDir, data, cs, previouslyImplemented)
}

// removeRetiredForgeFile deletes a generated file forge no longer
// emits — but never a file the user owns. Ownership is read from the
// file itself: a VERIFYING forge:hash marker certifies the bytes as
// forge output (delete); a Modified marker means the user edited it
// (keep); no marker falls back to the legacy "Code generated by forge"
// banner check for pre-marker projects. Disowned paths are never
// touched.
func removeRetiredForgeFile(projectDir, relPath string, cs *checksums.FileChecksums) {
	if cs.IsDisowned(relPath) {
		return // user-owned by recorded intent
	}
	fullPath := filepath.Join(projectDir, relPath)
	body, err := os.ReadFile(fullPath)
	if err != nil {
		return
	}
	switch checksums.Verify(body) {
	case checksums.Pristine:
		_ = os.Remove(fullPath)
	case checksums.Modified:
		return // hand-edited — not forge's to delete
	case checksums.NoMarker:
		// Pre-marker forge output: only sweep what self-identifies.
		if bytes.Contains(body, []byte("Code generated by forge")) {
			_ = os.Remove(fullPath)
		}
	}
}

// crudShimImports computes the import block for the user-owned
// handlers_crud.go shim from the methods it will contain.
func crudShimImports(data CRUDTemplateData) []string {
	imports := []string{"context", "connectrpc.com/connect"}
	hasMismatch := false
	hasReal := false
	hasTenantMismatch := false
	for _, m := range data.CRUDMethods {
		if m.ShapeMismatch {
			hasMismatch = true
			if m.HasTenant {
				hasTenantMismatch = true
			}
		} else {
			hasReal = true
		}
	}
	if hasReal {
		imports = append(imports, "github.com/reliant-labs/forge/pkg/crud")
	}
	if hasMismatch {
		// The WIRED custom-read-shape body runs a real query: it builds
		// orm.QueryOption filters and calls db.List<Entity>, projecting
		// rows via the generated <entity>ToProto helper.
		imports = append(imports,
			"github.com/reliant-labs/forge/pkg/orm",
			"github.com/reliant-labs/forge/pkg/svcerr",
			data.Module+"/internal/db")
		if hasTenantMismatch {
			imports = append(imports, data.Module+"/pkg/middleware")
		}
	}
	imports = append(imports, "pb "+data.Module+"/gen/"+data.ProtoPackage+"/v1")
	return imports
}

// renderCRUDShimMethods renders the per-RPC shim blocks (delegation or
// explanatory Unimplemented stub) for the given methods.
func renderCRUDShimMethods(methods []CRUDMethodTemplateData) (string, error) {
	var b strings.Builder
	for _, m := range methods {
		block, err := templates.ServiceTemplates().Render("handlers_crud_shim_method.go.tmpl", m)
		if err != nil {
			return "", fmt.Errorf("render handlers_crud_shim_method.go.tmpl for %s: %w", m.MethodName, err)
		}
		b.WriteString("\n")
		b.Write(bytes.TrimSpace(block))
		b.WriteString("\n")
	}
	return b.String(), nil
}

// ensureCRUDShimFile maintains the user-owned handlers_crud.go:
//
//   - absent  → scaffold the whole file (the ONLY full write, ever);
//   - present → append shims for CRUD RPCs that have no method in the
//     file yet (a newly-annotated entity, a new RPC). Existing content
//     is never rewritten; needed imports are inserted into the existing
//     import block if missing.
//
// Methods the user implemented in sibling files were already filtered
// out by the caller, so an append never collides with a hand-written
// handler.
func ensureCRUDShimFile(projectDir, relDir string, data CRUDTemplateData, cs *checksums.FileChecksums, previouslyImplemented map[string]bool) error {
	shimRel := filepath.Join(relDir, "handlers_crud.go")
	fullPath := filepath.Join(projectDir, shimRel)

	existing, readErr := os.ReadFile(fullPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read %s: %w", shimRel, readErr)
	}

	if os.IsNotExist(readErr) {
		blocks, err := renderCRUDShimMethods(data.CRUDMethods)
		if err != nil {
			return err
		}
		warnCustomReadShapeStubs(data.CRUDMethods, shimRel, previouslyImplemented)
		var b strings.Builder
		b.WriteString("// yours: scaffolded once, never touched again — forge will not overwrite this file\n")
		b.WriteString("//\n")
		b.WriteString("// Each method is a thin delegation: the CRUD lifecycle\n")
		b.WriteString("// (auth, tenant scoping, pagination, error mapping) lives in\n")
		b.WriteString("// github.com/reliant-labs/forge/pkg/crud, and the per-entity wiring is\n")
		b.WriteString("// generated in handlers_crud_ops_gen.go (Tier-1, regenerated every run).\n")
		b.WriteString("// Because these bodies never name an entity field, proto/schema changes\n")
		b.WriteString("// flow through the regenerated ops file without touching this one.\n")
		b.WriteString("//\n")
		b.WriteString("// To customize an RPC, replace its delegation with a real implementation\n")
		b.WriteString("// right here. CRUD RPCs added later are appended to this file by\n")
		b.WriteString("// `forge generate`; your existing content is never modified.\n")
		b.WriteString("package " + data.Package + "\n\n")
		b.WriteString("import (\n")
		for _, imp := range crudShimImports(data) {
			if alias, path, ok := strings.Cut(imp, " "); ok {
				b.WriteString("\t" + alias + " \"" + path + "\"\n")
			} else {
				b.WriteString("\t\"" + imp + "\"\n")
			}
		}
		b.WriteString(")\n")
		b.WriteString(blocks)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			return err
		}
		content := []byte(b.String())
		if err := writeUserScaffold(fullPath, content); err != nil {
			return fmt.Errorf("write %s: %w", shimRel, err)
		}
		recordTier2(cs, shimRel, content)
		return nil
	}

	// Append shims for methods missing from the file.
	var missing []CRUDMethodTemplateData
	for _, m := range data.CRUDMethods {
		if !bytes.Contains(existing, []byte("func (s *Service) "+m.MethodName+"(")) {
			missing = append(missing, m)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	blocks, err := renderCRUDShimMethods(missing)
	if err != nil {
		return err
	}
	warnCustomReadShapeStubs(missing, shimRel, previouslyImplemented)
	content := string(existing)
	for _, imp := range crudShimImports(CRUDTemplateData{
		Module:       data.Module,
		ProtoPackage: data.ProtoPackage,
		CRUDMethods:  missing,
	}) {
		content = ensureImportLine(content, imp)
	}
	content = strings.TrimRight(content, "\n") + "\n" + blocks
	if err := writeUserScaffold(fullPath, []byte(content)); err != nil {
		return fmt.Errorf("append CRUD shims to %s: %w", shimRel, err)
	}
	recordTier2(cs, shimRel, []byte(content))
	fmt.Printf("  ✅ Appended %d new CRUD shim(s) to %s (user-owned)\n", len(missing), shimRel)
	return nil
}

// warnCustomReadShapeStubs prints one loud line per RPC whose shim is
// being written as a WIRED custom-read-shape scaffold. The scaffold is the
// system WORKING — the request/response shape is a legitimate domain
// decision and the body is the user's to refine — but it must never land
// silently: the wired body runs a naive query and returns an EMPTY
// response (a `// TODO: refine` marker on the response packing) until the
// user finishes it, so a deploy ships a procedure that 200s with no data.
// When the RPC previously had a real generated implementation (the retired
// handlers_crud_gen.go carried a non-stub body for it), say so
// explicitly — that's a behavior change on a live procedure, and a
// downstream agent filed a near-miss with live traffic on exactly this
// transition.
func warnCustomReadShapeStubs(methods []CRUDMethodTemplateData, shimRel string, previouslyImplemented map[string]bool) {
	for _, m := range methods {
		if !m.ShapeMismatch {
			continue
		}
		if previouslyImplemented[m.MethodName] {
			fmt.Printf("  ⚠️  %s: REPLACING a previously generated implementation with a WIRED custom-read-shape scaffold in %s (custom read shape: %s) — finish wiring the response before serving traffic\n",
				m.MethodName, shimRel, m.MismatchReason)
			continue
		}
		fmt.Printf("  ⚠️  %s: custom read shape (%s) — scaffolded a WIRED starting point in %s (runs a real query; finish wiring the response onto pb.%s)\n",
			m.MethodName, m.MismatchReason, shimRel, m.OutputType)
	}
}

// implementedMethodsIn scans a retired generated handler file for
// *Service methods that had REAL bodies — i.e. methods NOT tagged with
// a shape-mismatch marker (`forge:custom-read-shape`, or the legacy
// `FORGE_CRUD_SHAPE_MISMATCH` spelling) in the comment block above the
// declaration. Returns nil when the file doesn't exist (the common,
// post-migration case). Line-based on purpose: the file is forge's own
// past output with a fixed shape, and this runs once per generate.
func implementedMethodsIn(path string) map[string]bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	implemented := map[string]bool{}
	pendingMarker := false
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "// forge:custom-read-shape:") ||
			strings.HasPrefix(trimmed, "// FORGE_CRUD_SHAPE_MISMATCH:") {
			pendingMarker = true
			continue
		}
		if !strings.HasPrefix(trimmed, "func (s *Service) ") {
			// The marker stays pending until the next func declaration —
			// the template emits it inside the doc comment directly above
			// the func, so being lenient about intervening comment lines
			// is safe and being strict would miss reformatted output.
			continue
		}
		rest := strings.TrimPrefix(trimmed, "func (s *Service) ")
		if end := strings.IndexAny(rest, "(\t "); end > 0 {
			if !pendingMarker {
				implemented[rest[:end]] = true
			}
		}
		pendingMarker = false
	}
	return implemented
}

// recordTier2 marks a user-owned scaffold as touched this run. Tier-2
// files carry no certification marker (user-owned from birth), so the
// stale sweep can never target them; the written-this-run mark is kept
// for parity with the chokepoint writers.
func recordTier2(cs *checksums.FileChecksums, relPath string, content []byte) {
	_ = cs
	_ = content
	checksums.MarkWrittenThisRun(relPath)
}

// ensureImportLine inserts an import (optionally "alias path") into the
// file's first import block when no line for that path exists yet. Same
// minimal-mutation approach as ensureDepsDBField: the file is user-owned,
// so we touch only what the appended code needs to compile.
func ensureImportLine(content, imp string) string {
	alias, path, hasAlias := strings.Cut(imp, " ")
	if !hasAlias {
		path = imp
		alias = ""
	}
	if strings.Contains(content, "\""+path+"\"") {
		return content
	}
	idx := strings.Index(content, "import (")
	if idx < 0 {
		return content
	}
	closing := strings.Index(content[idx:], ")")
	if closing < 0 {
		return content
	}
	line := "\t\"" + path + "\"\n"
	if alias != "" {
		line = "\t" + alias + " \"" + path + "\"\n"
	}
	insertPos := idx + closing
	return content[:insertPos] + line + content[insertPos:]
}

// validateCRUDShape decides whether the request/response messages observed
// in svc.Messages match the AIP-158-style shape the CRUD body template
// emits. It returns ok=true when:
//
//   - svc.Messages is empty (legacy/no-descriptor path — preserve old
//     behaviour and let the existing template fire), OR
//   - the input/output messages for this RPC are absent from
//     svc.Messages (we can't validate, so be lenient), OR
//   - every shape rule for the operation holds.
//
// When ok=false the returned reason describes the first failing rule.
// Callers should mark the method ShapeMismatch and skip emitting CRUD-body
// fields that would dereference unavailable proto fields (PageSize,
// PageToken, the entity-typed PK accessor, the repeated-entity response
// field). The shim template renders a `forge:custom-read-shape`-tagged
// CodeUnimplemented stub in that branch so the user-owned file still
// compiles against bespoke proto shapes (Limit/enum filters, string Ticker
// keys, repeated-message responses) — the custom shape is the user's to
// implement, by design.
//
// The rules deliberately stay conservative — they only fail when we can
// see message fields and prove they don't fit. Anything ambiguous (no
// Messages map at all, or this particular RPC's messages missing from it)
// is treated as ok so existing projects whose protos do match AIP-158
// keep generating the same code they did before this check landed.
func validateCRUDShape(svc ServiceDef, cm CRUDMethod) (ok bool, reason string) {
	if len(svc.Messages) == 0 {
		return true, ""
	}

	inputFields, inputKnown := svc.Messages[cm.Method.InputType]
	outputFields, outputKnown := svc.Messages[cm.Method.OutputType]

	inputByName := make(map[string]MessageFieldDef, len(inputFields))
	for _, f := range inputFields {
		inputByName[f.Name] = f
	}
	outputByName := make(map[string]MessageFieldDef, len(outputFields))
	for _, f := range outputFields {
		outputByName[f.Name] = f
	}

	switch cm.Operation {
	case "get", "delete":
		if !inputKnown {
			return true, ""
		}
		// PkField on the entity is the snake_case proto name (e.g.
		// "id", "ticker"). The CRUD template emits `req.<PascalPk>`
		// and downstream code calls Get/DeleteByID with that value.
		// If the request has no field with that name at all, the
		// generated body won't compile.
		if _, has := inputByName[cm.Entity.PkField]; !has {
			return false, fmt.Sprintf("request %s has no %s field matching entity PK (observed fields: %s)", cm.Method.InputType, cm.Entity.PkField, describeFields(inputFields))
		}
	case "update":
		if !inputKnown {
			return true, ""
		}
		// Update body dereferences `req.<EntityField>` and expects
		// it to be a *db.<Entity>. Validate the request actually
		// carries an entity-typed field.
		found := false
		for _, f := range inputFields {
			if fieldMatchesEntity(f, cm.Entity.Name) {
				found = true
				break
			}
		}
		if !found {
			return false, fmt.Sprintf("request %s carries no %s message field (observed fields: %s)", cm.Method.InputType, cm.Entity.Name, describeFields(inputFields))
		}
	case "list":
		// Two valid AIP-132 list shapes are Tier-1:
		//
		//   - cursor-paginated: BOTH page_size and page_token present
		//     (the template emits req.PageSize / req.PageToken and packs
		//     NextPageToken);
		//   - non-paginated: NEITHER present — a plain filtered/ordered
		//     list that returns all matches (no cursor dereferences).
		//
		// Only an INCONSISTENT pair (exactly one of the two) falls to the
		// custom path: the cursor template needs both, and a lone
		// page_size (kalshi-trader's `Limit`/offset style, say) is a
		// bespoke pagination contract the user must wire by hand.
		if inputKnown && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			_, hasSize := inputByName["page_size"]
			_, hasTok := inputByName["page_token"]
			if hasSize != hasTok {
				missing := "page_token"
				if hasTok {
					missing = "page_size"
				}
				return false, fmt.Sprintf("request %s has one half of AIP-158 cursor pagination but not the other (missing %s); add both for a cursor-paginated list, or neither for a plain filtered list (observed fields: %s)", cm.Method.InputType, missing, describeFields(inputFields))
			}
		}
		// List response template emits `<EntityPlural>: items` and
		// optionally `NextPageToken: nextPageToken`. Validate the
		// response carries a repeated entity field by that name.
		//
		// The field name is derived through naming.EntityListFieldName —
		// the SAME helper the entity scaffolder emits (`forge add entity`)
		// and the ops emitter's Go field derives from — so the comparison
		// is against the actual snake_case proto field ("module_configs"),
		// not a concatenated-lowercase form ("moduleconfigs") that matched
		// no proto and broke every multi-word entity's CRUD.
		if outputKnown {
			listField := naming.EntityListFieldName(cm.Entity.Name)
			if _, has := outputByName[listField]; !has {
				return false, fmt.Sprintf("response %s lacks repeated %s field %s (observed fields: %s)", cm.Method.OutputType, cm.Entity.Name, listField, describeFields(outputFields))
			}
		}
	case "create":
		// Create response template emits `<EntityName>: entity`.
		// Validate the response carries a single field of that type
		// (named after the entity in snake_case). Same helper as the
		// scaffolder/emitter (see the list case) so multi-word entities
		// (module_config, order_item, …) match instead of falling to the
		// custom-read-shape stub on the concatenated-lowercase mismatch.
		if outputKnown {
			entityField := naming.EntityFieldName(cm.Entity.Name)
			if _, has := outputByName[entityField]; !has {
				return false, fmt.Sprintf("response %s lacks %s field %s (observed fields: %s)", cm.Method.OutputType, cm.Entity.Name, entityField, describeFields(outputFields))
			}
		}
	}
	return true, ""
}

// detectListPagination reports whether a list RPC gets cursor pagination
// wired in. Cursor pagination is opt-in: the request must follow
// List*Request naming AND actually carry both page_size and page_token.
// A List*Request without them is a valid non-paginated AIP-132 list
// (filter/order only) that validateCRUDShape admits to Tier-1 — its body
// must NOT dereference req.PageSize/req.PageToken (they don't exist on
// that proto). When there is no field descriptor at all (legacy path),
// the historical assume-paginated behavior is preserved. Returns
// ("", false-equivalents) for non-list / shape-mismatched methods.
func detectListPagination(svc ServiceDef, cm CRUDMethod, shapeOK bool) (bool, string) {
	if !shapeOK || cm.Operation != "list" {
		return false, ""
	}
	if !strings.HasPrefix(cm.Method.InputType, "List") || !strings.HasSuffix(cm.Method.InputType, "Request") {
		return false, ""
	}
	fields, ok := svc.Messages[cm.Method.InputType]
	if !ok {
		// No descriptor — preserve the historical assume-cursor default.
		return true, "cursor"
	}
	hasSize, hasTok := false, false
	for _, f := range fields {
		switch f.Name {
		case "page_size":
			hasSize = true
		case "page_token":
			hasTok = true
		}
	}
	if hasSize && hasTok {
		return true, "cursor"
	}
	return false, ""
}

// crudMethodFacts derives the per-method CRUDMethodTemplateData facts shared
// by BOTH the handler builder (buildCRUDTemplateData) and the test builder
// (buildCRUDTestTemplateData). Both previously re-derived this same set
// independently with explicit keep-in-sync comments; computing it once here is
// the single source of truth. The handler builder consumes the result
// directly; the test builder calls this then layers entity-grouping on top.
//
// strictFilters is the ONE axis on which the two callers differ. The handler
// path (strictFilters=true) classifies list filters via
// classifyEntityFilterField, which fails the generate LOUDLY when a filter
// field names no declared column — shipping a phantom-column query is the
// review-confirmed silence bug. The test path (strictFilters=false) classifies
// via classifyFilterField, which is advisory (no SearchColumns, never fatal):
// the test template never dereferences filter facts, so it must not abort the
// generate on a bespoke field. Errors are only ever returned when
// strictFilters is true.
func crudMethodFacts(svc ServiceDef, cm CRUDMethod, strictFilters bool) (CRUDMethodTemplateData, error) {
	authAction := operationToAuthAction(cm.Operation)

	// Validate the request/response shape up front. When the observed proto
	// messages don't match the AIP-158-style body the template emits, we
	// still emit a method (so the proto's RPC interface is satisfied) but
	// route it to a tagged CodeUnimplemented stub instead of the body. This
	// keeps handlers_crud_gen.go compiling against bespoke shapes
	// (Limit/enum filters, string Ticker keys, repeated-message responses).
	// The test scaffold mirrors the same gate so it doesn't emit per-RPC
	// rows that dereference fields the request type doesn't have.
	shapeOK, shapeReason := validateCRUDShape(svc, cm)

	// Cursor pagination is opt-in (both page_size + page_token present); a
	// List*Request without them is a plain non-paginated AIP-132
	// filtered/ordered list, still Tier-1.
	hasPagination, paginationStyle := detectListPagination(svc, cm, shapeOK)

	// Detect filters and ordering from request message fields. Skip when the
	// shape didn't match: classifyFilterField would otherwise happily turn a
	// bespoke field like `ticker` (a string PK) or a `kalshi_status` enum
	// into a synthetic `WhereEq("ticker", req.Ticker)` clause that fails to
	// compile against the real request type.
	var filterFields []FilterFieldData
	hasOrderBy := false
	if shapeOK && cm.Operation == "list" && svc.Messages != nil {
		if msgFields, ok := svc.Messages[cm.Method.InputType]; ok {
			for _, mf := range msgFields {
				if classifySkipField(mf.Name) {
					continue
				}
				if mf.Name == "order_by" {
					hasOrderBy = true
					continue
				}
				if strictFilters {
					ff, ferr := classifyEntityFilterField(mf, cm.Entity)
					if ferr != nil {
						// LOUD by design: a filter the generator cannot map
						// to a declared column must fail the generate, not
						// ship a phantom-column query that silently returns
						// nothing (or leaks SQL errors) at runtime.
						return CRUDMethodTemplateData{}, fmt.Errorf("%s.%s: %w", svc.Name, cm.Method.Name, ferr)
					}
					filterFields = append(filterFields, ff)
				} else {
					filterFields = append(filterFields, classifyFilterField(mf))
				}
			}
		}
	}

	// Determine the entity field name in the update request message.
	// Proto generates a field named after the entity (e.g., "Project project = 1;"
	// becomes Go field "Project"). We look it up in the parsed message fields;
	// if not found, we fall back to the entity name.
	updateEntityField := cm.Entity.Name
	if cm.Operation == "update" && svc.Messages != nil {
		if fields, ok := svc.Messages[cm.Method.InputType]; ok {
			for _, f := range fields {
				if fieldMatchesEntity(f, cm.Entity.Name) {
					updateEntityField = naming.ToProtoPascalCase(f.Name)
					break
				}
			}
		}
	}

	// AIP-134: a FieldMask field on the update request wires the
	// masked-persistence hooks. Skip on shape mismatch — the stub
	// branch never dereferences it.
	updateMaskField := ""
	if shapeOK && cm.Operation == "update" {
		updateMaskField = updateMaskGoField(svc, cm.Method.InputType)
	}

	// Best-effort filter mapping for the WIRED custom-read-shape body.
	// Only when the shape didn't match (the stub branch): seed the
	// scaffold's []orm.QueryOption skeleton from request fields that
	// happen to name a declared column. Advisory, never fatal.
	var customFilters []FilterFieldData
	if !shapeOK {
		customFilters = buildCustomFilters(svc, cm)
	}

	// Precompute the create-request -> entity-struct assignments.
	// Skip on shape mismatch — the stub doesn't reference these.
	var createAssigns []string
	if shapeOK && cm.Operation == "create" {
		m := Method{
			Name:        cm.Method.Name,
			InputType:   cm.Method.InputType,
			OutputType:  cm.Method.OutputType,
			InputTypeFQ: svc.Package + "." + cm.Method.InputType,
		}
		createAssigns = buildCreateAssigns(svc, m, cm.Entity)
	}

	return CRUDMethodTemplateData{
		MethodName:        cm.Method.Name,
		InputType:         cm.Method.InputType,
		OutputType:        cm.Method.OutputType,
		EntityName:        cm.Entity.Name,
		EntityLower:       strings.ToLower(cm.Entity.Name),
		Operation:         cm.Operation,
		AuthRequired:      cm.Method.AuthRequired,
		AuthAction:        authAction,
		PkField:           naming.ToProtoPascalCase(cm.Entity.PkField),
		PkColumnName:      cm.Entity.PkField,
		PkGoType:          cm.Entity.PkGoType,
		HasPkInInput:      cm.Operation == "get" || cm.Operation == "update" || cm.Operation == "delete",
		ResponseField:     cm.Entity.Name,
		HasPagination:     hasPagination,
		PaginationStyle:   paginationStyle,
		HasFilters:        len(filterFields) > 0,
		FilterFields:      filterFields,
		HasOrderBy:        hasOrderBy,
		HasTenant:         cm.Entity.HasTenant,
		TenantGoName:      cm.Entity.TenantGoName,
		TenantColumnName:  cm.Entity.TenantColumnName,
		UpdateEntityField: updateEntityField,
		UpdateMaskField:   updateMaskField,
		CreateAssigns:     createAssigns,
		ShapeMismatch:     !shapeOK,
		MismatchReason:    shapeReason,
		CustomFilters:     customFilters,
	}, nil
}

func buildCRUDTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath string) (CRUDTemplateData, error) {
	// Synthesized Package is a placeholder only: GenerateCRUDHandlers
	// overrides it with the disk-resolved package clause before rendering
	// (the file lands inside an EXISTING handler dir).
	pkg := naming.ServicePackage(svc.Name)

	// Build ProtoPackage path (same logic as mapServiceDefToTemplateData)
	protoPackage := protoImportPath(svc)

	var methods []CRUDMethodTemplateData
	for _, cm := range crudMethods {
		// Strict filter classification: an unmappable list filter fails the
		// generate LOUDLY (phantom-column queries silently match nothing).
		mtd, err := crudMethodFacts(svc, cm, true)
		if err != nil {
			return CRUDTemplateData{}, err
		}
		methods = append(methods, mtd)
	}

	// Check if any method has pagination, filters, ordering, or a real
	// (non-stub) body. Mismatched stubs contribute nothing to the file's
	// import needs so we don't pull in pkg/crud or pkg/orm for a file
	// that only emits TODO stubs.
	hasPagination := false
	hasFilters := false
	hasOrderBy := false
	needsCRUDLib := false
	for _, m := range methods {
		if m.HasPagination {
			hasPagination = true
		}
		if m.HasFilters {
			hasFilters = true
		}
		if m.HasOrderBy {
			hasOrderBy = true
		}
		if !m.ShapeMismatch {
			needsCRUDLib = true
		}
	}
	hasTenant := false
	for _, m := range methods {
		if m.HasTenant && !m.ShapeMismatch {
			hasTenant = true
			break
		}
	}
	needsORM := hasPagination || hasFilters || hasOrderBy || hasTenant

	// One conversion pair per entity referenced by ANY CRUD body —
	// real (non-stub) OR a wired custom-read-shape stub. The wired
	// custom body calls <entity>ToProto to project the rows it fetches
	// onto the wire, so the projection pair must exist even when every
	// method on the service is custom (no real op constructor at all).
	var convs []EntityConvTemplateData
	seenConv := map[string]bool{}
	for i, m := range methods {
		if seenConv[m.EntityName] {
			continue
		}
		seenConv[m.EntityName] = true
		convs = append(convs, BuildEntityConv(svc, crudMethods[i].Entity))
	}

	// The ops file is emitted when there's at least one real op to wire
	// OR at least one conversion pair a wired custom body depends on.
	needsOpsFile := needsCRUDLib || len(convs) > 0

	return CRUDTemplateData{
		Package:          pkg,
		Module:           modulePath,
		ProtoPackage:     protoPackage,
		DBPackagePath:    modulePath + "/internal/db",
		HasPagination:    hasPagination,
		HasFilters:       hasFilters,
		HasOrderBy:       hasOrderBy,
		NeedsORM:         needsORM,
		HasTenant:        hasTenant,
		NeedsCRUDLib:     needsCRUDLib,
		NeedsOpsFile:     needsOpsFile,
		CRUDMethods:      methods,
		Entities:         convs,
		NeedsTimestamppb: ConvNeedsTimestamppb(convs),
	}, nil
}

// CRUDTestTemplateData holds all data needed to render the CRUD test template.
type CRUDTestTemplateData struct {
	Package      string                   // Go package name, e.g. "patients"
	Module       string                   // Go module path, e.g. "github.com/demo-project"
	ProtoPackage string                   // e.g. "proto/services/patients"
	HasTenant    bool                     // true if any entity has tenant isolation
	Entities     []CRUDTestEntityData     // Grouped per-entity test data
	CRUDMethods  []CRUDMethodTemplateData // All CRUD methods (for individual error tests)
	// NeedsFieldMask gates the fieldmaskpb import: true when at least one
	// entity's lifecycle test emits the AIP-134 masked-update block.
	NeedsFieldMask bool
	// TestHelperName mirrors ServiceTemplateData.TestHelperName: the suffix
	// the bootstrap testing generator emits on `app.NewTest<X>` /
	// `app.NewTest<X>Server`. CRUD test scaffolds use this rather than
	// pascal-casing Package so the call site matches the actual factory
	// when an internal package shares the service's leaf name.
	TestHelperName string
}

// CRUDTestEntityData groups CRUD operations by entity for lifecycle tests.
type CRUDTestEntityData struct {
	EntityName       string // "Patient"
	EntityLower      string // "patient"
	PkField          string // "Id"
	PkGoType         string // "int64"
	HasCreate        bool
	HasGet           bool
	HasList          bool
	HasUpdate        bool
	HasDelete        bool
	HasAllCRUD       bool   // true if all 5 operations exist
	HasTenant        bool   // true when the entity has a tenant key field
	TenantGoName     string // e.g., "OrgId"
	TenantColumnName string // e.g., "org_id"
	HasTimestamps    bool   // entity annotation timestamps:true — created_at is asserted set
	// MutableStringField is the Go name of the first non-PK string field
	// (e.g. "Name") — the field the lifecycle test mutates to prove
	// update actually writes. Empty when the entity has none.
	MutableStringField string
	// MutableStringFieldPath is MutableStringField's proto field name
	// (snake_case) — the AIP-134 update_mask path for it.
	MutableStringFieldPath string
	// SecondStringField/-Path name a SECOND mutable string field when the
	// entity has one (skipping the tenant key). The masked-update test
	// loads it with a clobber value the mask does NOT name, then asserts
	// it survived — proving the mask restricts the write. Empty when the
	// entity has only one string field (the non-clobber assertion is then
	// skipped; the masked write itself is still exercised).
	SecondStringField     string
	SecondStringFieldPath string
	CreateMethod          CRUDMethodTemplateData
	GetMethod             CRUDMethodTemplateData
	ListMethod            CRUDMethodTemplateData
	UpdateMethod          CRUDMethodTemplateData
	DeleteMethod          CRUDMethodTemplateData
	Fields                []CRUDTestFieldData // entity proto message fields (minus PK, minus deleted_at)
	CreateFields          []CRUDTestFieldData // fields from the CreateRequest message
	UpdateEntityField     string              // Go field name holding entity in UpdateRequest, e.g. "Project"
}

// CRUDTestFieldData holds per-field data for generating test values.
type CRUDTestFieldData struct {
	ProtoName string    // "Name"
	GoType    string    // "string"
	Kind      FieldKind // scalar, enum, message, wrapper, timestamp, etc.
	TestValue string    // `"test-value"` or `1` or `true`
}

// GenerateCRUDTests generates handlers_crud_gen_test.go (unit-test frames,
// no build tag — runs in the default `go test ./...`) and
// handlers_crud_integration_test.go (lifecycle / tenant / pagination /
// filter / NotFound suites guarded by `//go:build integration`) for a
// service with CRUD methods.
//
// cs is the project's checksum tracker. Both scaffold files are recorded
// when actually written; once the user clears every FORGE_SCAFFOLD marker
// the file becomes user-owned and forge stops re-rendering it (and stops
// updating the checksum). A nil cs is tolerated.
func GenerateCRUDTests(svc ServiceDef, crudMethods []CRUDMethod, modulePath string, projectDir string, cs *checksums.FileChecksums) error {
	// Disk-first: same handler-dir + package-clause resolution as
	// GenerateCRUDHandlers (the two MUST land in the same directory and
	// declare the same package).
	res, resErr := ResolveServiceComponent(projectDir, svc.Name)
	if resErr != nil {
		return resErr
	}
	pkg := res.PackageName
	targetDir := res.Dir
	relDir := filepath.Join("internal", "handlers", filepath.FromSlash(res.ImportLeaf))

	// Retire the marker-scaffold test pair this generator used to emit
	// (per-RPC AnyOutcome frames + a build-tag-gated integration suite).
	// Their replacement is ONE user-owned lifecycle test with real
	// assertions; files where the user already cleared every
	// FORGE_SCAFFOLD marker are theirs and are left alone.
	removeRetiredScaffoldTest(projectDir, filepath.Join(relDir, "handlers_crud_gen_test.go"), cs)
	removeRetiredScaffoldTest(projectDir, filepath.Join(relDir, "handlers_crud_integration_test.go"), cs)

	// Mirror the dedup GenerateCRUDHandlers applies: methods the user
	// implemented by hand keep their own tests.
	existingMethods, err := scanExistingMethods(targetDir, false)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scan existing methods for %s tests: %w", pkg, err)
	}
	var filteredMethods []CRUDMethod
	for _, cm := range crudMethods {
		if existingMethods[cm.Method.Name] {
			continue
		}
		filteredMethods = append(filteredMethods, cm)
	}
	if len(filteredMethods) == 0 {
		return nil
	}

	// Package + TestHelperName are overridden with the disk-resolved
	// package clause so the emitted test file always matches the directory
	// it lands in AND calls the `app.NewTest<X>` factory the bootstrap
	// testing generator (which uses the same resolver) actually emitted.
	data := buildCRUDTestTemplateData(svc, filteredMethods, modulePath, projectDir)
	data.Package = pkg
	data.TestHelperName = ComputeTestHelperName(pkg, projectDir)

	// The lifecycle test covers string-PK entities with create+get (the
	// forge CRUD convention). Nothing qualifying → nothing to scaffold.
	qualifying := false
	for _, e := range data.Entities {
		if e.HasCreate && e.HasGet && e.PkGoType == "string" {
			qualifying = true
			break
		}
	}
	if !qualifying {
		return nil
	}

	// Scaffold-once: handlers_crud_test.go is user-owned from line one.
	lifecycleRel := filepath.Join(relDir, "handlers_crud_test.go")
	lifecyclePath := filepath.Join(projectDir, lifecycleRel)
	if _, statErr := os.Stat(lifecyclePath); statErr == nil {
		return nil
	}

	content, err := templates.ServiceTemplates().Render("handlers_crud_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render handlers_crud_test.go.tmpl: %w", err)
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}
	if err := writeUserScaffold(lifecyclePath, content); err != nil {
		return fmt.Errorf("write handlers_crud_test.go: %w", err)
	}
	recordTier2(cs, lifecycleRel, content)
	return nil
}

// removeRetiredScaffoldTest deletes a retired marker-scaffold test file —
// but ONLY while it still carries FORGE_SCAFFOLD markers (i.e. forge
// still owned it). A file the user customized (markers cleared) is
// user-owned and stays, manifest entry and all.
func removeRetiredScaffoldTest(projectDir, relPath string, cs *checksums.FileChecksums) {
	fullPath := filepath.Join(projectDir, relPath)
	body, err := os.ReadFile(fullPath)
	if err != nil {
		return
	}
	if !bytes.Contains(body, []byte("FORGE_SCAFFOLD:")) {
		return
	}
	_ = os.Remove(fullPath)
	_ = cs
}

func buildCRUDTestTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath, projectDir string) CRUDTestTemplateData {
	// Synthesized Package/TestHelperName are placeholders only:
	// GenerateCRUDTests overrides both with disk-resolved values before
	// rendering — see the call site for the rationale.
	pkg := naming.ServicePackage(svc.Name)

	// Build ProtoPackage path (same logic as buildCRUDTemplateData)
	protoPackage := protoImportPath(svc)

	// Group by entity
	entityMap := make(map[string]*CRUDTestEntityData)
	var entityOrder []string

	// Build all CRUDMethodTemplateData
	var allMethods []CRUDMethodTemplateData

	for _, cm := range crudMethods {
		// Same per-method facts the handler builder derives, with the ONE
		// difference: advisory (non-strict) filter classification. The test
		// scaffold never dereferences filter facts and must not abort the
		// generate on a bespoke list field, so classifyEntityFilterField's
		// fatal phantom-column check is off here — crudMethodFacts therefore
		// never returns an error in this path. The shape gate, pagination,
		// update-entity-field, and AIP-134 mask facts all stay in lockstep
		// with the handler because they come from the same function.
		mtd, _ := crudMethodFacts(svc, cm, false)
		allMethods = append(allMethods, mtd)

		ent, ok := entityMap[cm.Entity.Name]
		if !ok {
			// Build proto service message field set for this entity
			protoFieldSet := make(map[string]bool)
			if svc.Messages != nil {
				if msgFields, ok := svc.Messages[cm.Entity.Name]; ok {
					for _, mf := range msgFields {
						protoFieldSet[mf.Name] = true
					}
				}
			}

			// Build entity fields (for update test): fields in both DB entity AND proto service message, minus PK
			var fields []CRUDTestFieldData
			protoNameByGoName := make(map[string]string)
			for _, f := range cm.Entity.Fields {
				if f.Name == cm.Entity.PkField {
					continue
				}
				if len(protoFieldSet) > 0 && !protoFieldSet[f.Name] {
					continue
				}
				kind := DetermineFieldKind(f.ProtoType, f.GoType)
				fields = append(fields, CRUDTestFieldData{
					ProtoName: f.GoName,
					GoType:    f.GoType,
					Kind:      kind,
					TestValue: testValueForType(f.GoType),
				})
				protoNameByGoName[f.GoName] = f.Name
			}

			// Determine update entity field name from UpdateRequest message
			updateEntityField := cm.Entity.Name
			if svc.Messages != nil {
				updateReqName := "Update" + cm.Entity.Name + "Request"
				if msgFields, ok := svc.Messages[updateReqName]; ok {
					for _, f := range msgFields {
						if fieldMatchesEntity(f, cm.Entity.Name) {
							updateEntityField = naming.ToProtoPascalCase(f.Name)
							break
						}
					}
				}
			}

			mutable, second := "", ""
			for _, f := range fields {
				if f.GoType != "string" {
					continue
				}
				// The tenant key is no good as a mutation target: the ORM
				// excludes it from the updatable set, so neither the
				// masked write nor the clobber assertion would observe it.
				if cm.Entity.HasTenant && f.ProtoName == cm.Entity.TenantGoName {
					continue
				}
				if mutable == "" {
					mutable = f.ProtoName
					continue
				}
				second = f.ProtoName
				break
			}

			ent = &CRUDTestEntityData{
				EntityName:             cm.Entity.Name,
				EntityLower:            strings.ToLower(cm.Entity.Name),
				PkField:                naming.ToProtoPascalCase(cm.Entity.PkField),
				PkGoType:               cm.Entity.PkGoType,
				HasTenant:              cm.Entity.HasTenant,
				TenantGoName:           cm.Entity.TenantGoName,
				TenantColumnName:       cm.Entity.TenantColumnName,
				HasTimestamps:          cm.Entity.Timestamps,
				MutableStringField:     mutable,
				MutableStringFieldPath: protoNameByGoName[mutable],
				SecondStringField:      second,
				SecondStringFieldPath:  protoNameByGoName[second],
				Fields:                 fields,
				UpdateEntityField:      updateEntityField,
			}
			entityMap[cm.Entity.Name] = ent
			entityOrder = append(entityOrder, cm.Entity.Name)
		}

		// Build CreateFields from the actual create request message
		if cm.Operation == "create" && svc.Messages != nil {
			if msgFields, ok := svc.Messages[cm.Method.InputType]; ok {
				var createFields []CRUDTestFieldData
				for _, f := range msgFields {
					goType := ProtoTypeToGoType(f.ProtoType)
					// Try to get richer GoType from entity definition
					for _, ef := range cm.Entity.Fields {
						if ef.Name == f.Name {
							goType = ef.GoType
							break
						}
					}
					kind := DetermineFieldKind(f.ProtoType, goType)
					createFields = append(createFields, CRUDTestFieldData{
						ProtoName: naming.ToProtoPascalCase(f.Name),
						GoType:    goType,
						Kind:      kind,
						TestValue: testValueForType(goType),
					})
				}
				ent.CreateFields = createFields
			}
		}

		switch cm.Operation {
		case "create":
			ent.HasCreate = true
			ent.CreateMethod = mtd
		case "get":
			ent.HasGet = true
			ent.GetMethod = mtd
		case "list":
			ent.HasList = true
			ent.ListMethod = mtd
		case "update":
			ent.HasUpdate = true
			ent.UpdateMethod = mtd
		case "delete":
			ent.HasDelete = true
			ent.DeleteMethod = mtd
		}
	}

	// Compute HasAllCRUD and build ordered slice
	var entities []CRUDTestEntityData
	for _, name := range entityOrder {
		ent := entityMap[name]
		ent.HasAllCRUD = ent.HasCreate && ent.HasGet && ent.HasList && ent.HasUpdate && ent.HasDelete
		entities = append(entities, *ent)
	}

	testHasTenant := false
	for _, e := range entities {
		if e.HasTenant {
			testHasTenant = true
			break
		}
	}

	// fieldmaskpb is imported only when some lifecycle test will actually
	// emit a masked-update block — mirror the template's emission gates
	// exactly or the generated file has an unused import.
	needsFieldMask := false
	for _, e := range entities {
		if e.HasCreate && e.HasGet && e.PkGoType == "string" &&
			e.HasUpdate && e.MutableStringField != "" &&
			e.UpdateMethod.UpdateMaskField != "" {
			needsFieldMask = true
			break
		}
	}

	return CRUDTestTemplateData{
		Package:        pkg,
		Module:         modulePath,
		ProtoPackage:   protoPackage,
		HasTenant:      testHasTenant,
		Entities:       entities,
		CRUDMethods:    allMethods,
		NeedsFieldMask: needsFieldMask,
		TestHelperName: ComputeTestHelperName(pkg, projectDir),
	}
}

// testValueForType returns a Go literal suitable for test data based on the Go type.
func testValueForType(goType string) string {
	switch goType {
	case "string":
		return `"test-value"`
	case "int32":
		return "1"
	case "int64":
		return "1"
	case "uint32":
		return "1"
	case "uint64":
		return "1"
	case "float32":
		return "1.0"
	case "float64":
		return "1.0"
	case "bool":
		return "true"
	case "[]byte":
		return `[]byte("test")`
	case "timestamppb.Timestamp", "*timestamppb.Timestamp":
		return "timestamppb.Now()"
	// Wrapper types (google.protobuf.*Value)
	case "*string":
		return `wrapperspb.String("test-value")`
	case "*int32":
		return "wrapperspb.Int32(42)"
	case "*int64":
		return "wrapperspb.Int64(42)"
	case "*uint32":
		return "wrapperspb.UInt32(42)"
	case "*uint64":
		return "wrapperspb.UInt64(42)"
	case "*bool":
		return "wrapperspb.Bool(true)"
	case "*float32":
		return "wrapperspb.Float(1.0)"
	case "*float64":
		return "wrapperspb.Double(1.0)"
	default:
		// Enum types (single-word Go ident like "Status") → use zero value
		// Repeated/map/message types → nil
		if strings.HasPrefix(goType, "[]") || strings.HasPrefix(goType, "map[") || strings.HasPrefix(goType, "*") {
			return "nil"
		}
		// Likely an enum type — use 0
		return "0"
	}
}

// classifySkipField returns true if the field should be skipped during filter classification.
// Pagination fields, ordering companions, and order_by itself are not filters.
func classifySkipField(name string) bool {
	switch name {
	case "page_size", "page_token", "descending", "desc", "sort_order":
		return true
	}
	return false
}

// classifyEntityFilterField builds a FilterFieldData validated against the
// entity's DECLARED columns:
//
//   - search-style fields (search/query/q) span the entity's string
//     columns (minus the PK) via orm.WhereILikeAny — they never map to a
//     column of their own;
//   - every other filter field must name a declared column, or the
//     generate fails loudly. Shipping a filter on a phantom column is the
//     review-confirmed silence bug: the query either errors at runtime or
//     (SQLite double-quoted fallback) silently matches nothing.
func classifyEntityFilterField(mf MessageFieldDef, entity EntityDef) (FilterFieldData, error) {
	ff := classifyFilterField(mf)
	if ff.FilterType == "search" {
		cols := entityStringColumns(entity)
		if len(cols) == 0 {
			return ff, fmt.Errorf(
				"list filter %q: entity %s has no non-PK string columns to search (declared columns: %s); drop the filter or implement the RPC by hand",
				mf.Name, entity.Name, entityColumnList(entity))
		}
		ff.SearchColumns = cols
		return ff, nil
	}
	for _, c := range entity.Columns {
		if c.Name == mf.Name {
			return ff, nil
		}
	}
	return ff, fmt.Errorf(
		"list filter field %q has no matching column on entity %s in the applied schema (columns: %s); rename the request field to a real column (or add the column via a migration), or implement the RPC by hand in a sibling file",
		mf.Name, entity.Name, entityColumnList(entity))
}

// buildCustomFilters maps a custom-read-shape RPC's request fields onto
// entity columns BEST-EFFORT, for seeding the wired scaffold body's
// []orm.QueryOption skeleton. Unlike classifyEntityFilterField (which
// fails the generate on an unmappable field — the strict Tier-1 list
// path), this is advisory: a field that doesn't name a declared column
// (or isn't a search field) is silently skipped, so the scaffold always
// compiles regardless of how bespoke the request is. Pagination/cursor/
// ordering control fields are skipped via classifySkipField. The result
// is a starting point the user refines, not a contract.
func buildCustomFilters(svc ServiceDef, cm CRUDMethod) []FilterFieldData {
	if svc.Messages == nil {
		return nil
	}
	msgFields, ok := svc.Messages[cm.Method.InputType]
	if !ok {
		return nil
	}
	var out []FilterFieldData
	for _, mf := range msgFields {
		if classifySkipField(mf.Name) || mf.Name == "order_by" {
			continue
		}
		// Only scalar fields make sense as WhereEq/ILike predicates;
		// message/enum/repeated fields are the user's to interpret.
		switch mf.ProtoType {
		case "message", "enum", "":
			continue
		}
		ff := classifyFilterField(mf)
		if ff.FilterType == "search" {
			cols := entityStringColumns(cm.Entity)
			if len(cols) == 0 {
				continue // nothing to search; let the user wire it
			}
			ff.SearchColumns = cols
			out = append(out, ff)
			continue
		}
		// exact: keep only when the field names a real column.
		for _, c := range cm.Entity.Columns {
			if c.Name == mf.Name {
				out = append(out, ff)
				break
			}
		}
	}
	return out
}

// entityStringColumns returns the columns a "search" filter spans: the
// entity's text columns by convention (SearchColumns is populated from
// the introspected schema — see schemadef.DetectConventions).
func entityStringColumns(entity EntityDef) []string {
	return entity.SearchColumns
}

// entityColumnList renders the applied schema's column names for diagnostics.
func entityColumnList(entity EntityDef) string {
	names := make([]string, 0, len(entity.Columns))
	for _, c := range entity.Columns {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// classifyFilterField builds a FilterFieldData from a proto message field.
func classifyFilterField(mf MessageFieldDef) FilterFieldData {
	goType := ProtoTypeToGoType(mf.ProtoType)

	filterType := "exact"
	switch mf.Name {
	case "search", "query", "q":
		filterType = "search"
	}

	return FilterFieldData{
		ProtoName:  mf.Name,
		GoName:     naming.ToProtoPascalCase(mf.Name),
		ColumnName: mf.Name,
		FieldType:  goType,
		FilterType: filterType,
		IsOptional: mf.IsOptional,
	}
}

// ensureDepsDBField checks the service.go Deps struct for a DB field and adds
// one if missing. The CRUD handlers reference s.deps.DB, so we need it present.
//
// service.go is a Tier-3 user-owned file: forge scaffolds it once at `forge
// add service` time, then never re-renders it. Silently injecting fields on
// every regen broke that contract — a user who hand-wrote a service with
// `List*`-prefixed RPC methods (matched by parseCRUDOperation) but no
// intention of using forge's CRUD codegen would see their service.go grow
// a `DB orm.Context` field and an orm import on the next `forge generate`.
//
// The opt-out: if the user has written a `handlers.go` (the sibling Tier-2
// hand-written-handler file) in the service package, they're signaling that
// they own handler wiring and forge should not touch service.go. The CRUD
// dedup pass in GenerateCRUDHandlers already drops any CRUD method the user
// has implemented in handlers.go; the remaining stubs in handlers_crud_gen.go
// will fail to compile without a DB field, but that failure is loud (a
// `go build` error the user sees directly) rather than a silent mutation of
// their service.go.
//
// A fresh service (no handlers.go on disk) still gets the DB field injected
// automatically — that's the happy path the original code was written for.
func ensureDepsDBField(serviceDir string) error {
	// Opt-out signal: user has written a handlers.go file. They're managing
	// handler wiring (and Deps shape) themselves; don't mutate service.go.
	if _, err := os.Stat(filepath.Join(serviceDir, "handlers.go")); err == nil {
		return nil
	}

	servicePath := filepath.Join(serviceDir, "service.go")
	data, err := os.ReadFile(servicePath)
	if err != nil {
		return err
	}

	content := string(data)
	original := content

	// The generated CRUD REQUIRES the Deps.DB field to be typed exactly
	// `orm.Context`. This is load-bearing in two places that must never
	// silently diverge:
	//   - the ops call `db.Create<E>(ctx, s.deps.DB, ...)`, whose db-func
	//     parameter is the orm.Context interface; AND
	//   - the CRUD test harness — DetectDepsDBField keys the WithDB /
	//     NewMigratedTestDB emission on an `orm.Context` field, and the
	//     generated testing.go assigns `deps.DB = cfg.db` (an orm.Context).
	// A `DB *orm.Client` field compiles the ops but SILENTLY suppresses the
	// test helpers (`undefined: app.WithDB`) and breaks the deps.DB
	// assignment. Rather than accept orm.Client as an equivalent (the old
	// hidden switch), the requirement is made EXPLICIT: detect the canonical
	// orm.Context field via the SAME AST predicate the test-helper trigger
	// uses; if a DB field of another type is present, say so LOUDLY; otherwise
	// inject the canonical field.
	hasORMContext, _ := DetectDepsDBField(serviceDir)
	if !hasORMContext && depsDeclaresForeignDBField(content) {
		fmt.Printf("⚠️  %s declares a Deps.DB field that is not orm.Context — the generated CRUD handlers and their tests require `DB orm.Context` (the ORM client satisfies it). Retype the field to enable WithDB / NewMigratedTestDB.\n", servicePath)
	}

	// If the Deps struct doesn't have the canonical orm.Context DB field yet
	// (and no foreign DB field is blocking a clean inject), add it.
	if !hasORMContext && !depsDeclaresForeignDBField(content) {
		// Find the Deps struct and inject the DB field after the opening line.
		marker := "// Add your dependencies here."
		if !strings.Contains(content, marker) {
			// Try to find the Deps struct opening brace
			marker = "type Deps struct {"
			idx := strings.Index(content, marker)
			if idx < 0 {
				return nil // Can't find Deps struct, skip
			}
			// Insert after the opening brace line
			newlineIdx := strings.Index(content[idx:], "\n")
			if newlineIdx < 0 {
				return nil
			}
			insertPos := idx + newlineIdx + 1
			dbField := "\tDB         orm.Context\n"
			content = content[:insertPos] + dbField + content[insertPos:]
		} else {
			// Insert the DB field before the marker comment
			content = strings.Replace(content, marker, "DB         orm.Context\n\t"+marker, 1)
		}

		// Ensure the orm import is present
		if !strings.Contains(content, "\"github.com/reliant-labs/forge/pkg/orm\"") {
			// Find the import block and add the orm import
			importIdx := strings.Index(content, "import (")
			if importIdx >= 0 {
				// Find the closing paren of the import block
				closingIdx := strings.Index(content[importIdx:], ")")
				if closingIdx >= 0 {
					insertPos := importIdx + closingIdx
					content = content[:insertPos] + "\n\t\"github.com/reliant-labs/forge/pkg/orm\"\n" + content[insertPos:]
				}
			}
		}
	}

	// The DB field is REQUIRED for the generated CRUD handlers — gate it
	// in validateDeps so a missing database fails at boot with an
	// actionable error, not as a nil-interface panic on the first RPC.
	// (wire_gen passes a true nil interface via app.ORMContext(), so this
	// check actually fires.) Inject at the scaffold's extension marker;
	// services whose validateDeps was rewritten by hand are left alone —
	// the marker is the opt-in surface.
	content = injectValidateDepsDBCheck(content)

	if content == original {
		return nil
	}
	return writeUserScaffold(servicePath, []byte(content))
}

// depsDeclaresForeignDBField reports whether service.go already carries a
// database-ish Deps field of a type OTHER than orm.Context — the two shapes
// the generated CRUD cannot use (`*orm.Client`, `*sql.DB`/`sql.DB`). It exists
// so ensureDepsDBField can make the orm.Context requirement explicit: when
// this is true and the canonical orm.Context field is absent, forge warns
// rather than silently injecting a duplicate `DB` field (a compile error) or
// silently tolerating a type that disables the CRUD test helpers. Detection is
// text-based, matching ensureDepsDBField's own minimal-mutation style; the
// caller already used the AST predicate (DetectDepsDBField) for the positive
// orm.Context case.
func depsDeclaresForeignDBField(content string) bool {
	return strings.Contains(content, "orm.Client") ||
		strings.Contains(content, "*sql.DB") ||
		strings.Contains(content, "sql.DB")
}

// injectValidateDepsDBCheck inserts `if d.DB == nil { ... }` into the
// scaffolded validateDeps() ahead of the "Add checks" marker comment.
// No-ops when the check (any `d.DB == nil` mention) already exists or
// the marker is gone (user-rewritten validateDeps).
func injectValidateDepsDBCheck(content string) string {
	if strings.Contains(content, "d.DB == nil") {
		return content
	}
	const marker = "// Add checks for your required Deps fields here."
	idx := strings.Index(content, marker)
	if idx < 0 {
		return content
	}
	check := "if d.DB == nil {\n" +
		"\t\treturn fmt.Errorf(\"Deps.DB is required by the generated CRUD handlers — set DATABASE_URL (the generated bootstrap constructs the ORM client from it)\")\n" +
		"\t}\n\t"
	return content[:idx] + check + content[idx:]
}

// operationToAuthAction maps a CRUD operation to the middleware action constant.
func operationToAuthAction(op string) string {
	switch op {
	case "create":
		return "create"
	case "get":
		return "read"
	case "list":
		return "list"
	case "update":
		return "update"
	case "delete":
		return "delete"
	default:
		return "read"
	}
}

// protoTypeMatchesEntity checks if a proto field type references the given entity.
// Handles both bare types ("Patient") and qualified types ("db.v1.Patient").
func protoTypeMatchesEntity(protoType, entityName string) bool {
	return protoType == entityName || protoType == "db.v1."+entityName
}

// fieldMatchesEntity reports whether a message field references the given
// entity type. The modern descriptor carries the referenced message name
// in MessageType ("Item", or fully-qualified "services.api.v1.Item");
// ProtoType alone is the literal "message" for every message field, which
// is unmatchable — relying on it produced a false custom-read-shape stub
// (then spelled FORGE_CRUD_SHAPE_MISMATCH) on forge's own scaffold (the F2 bug).
func fieldMatchesEntity(f MessageFieldDef, entityName string) bool {
	if f.MessageType != "" {
		if f.MessageType == entityName || strings.HasSuffix(f.MessageType, "."+entityName) {
			return true
		}
	}
	return protoTypeMatchesEntity(f.ProtoType, entityName)
}

// updateMaskGoField returns the Go field name of the AIP-134
// google.protobuf.FieldMask field on the given request message ("" when
// the message declares none, or when no field data is available). The
// modern descriptor carries the referenced message name in MessageType;
// the suffix match also accepts a bare "FieldMask" from older descriptors.
func updateMaskGoField(svc ServiceDef, inputType string) string {
	if svc.Messages == nil {
		return ""
	}
	for _, f := range svc.Messages[inputType] {
		if f.MessageType == "google.protobuf.FieldMask" || strings.HasSuffix(f.MessageType, ".FieldMask") || f.MessageType == "FieldMask" {
			return naming.ToProtoPascalCase(f.Name)
		}
	}
	return ""
}

// describeFields renders an observed-field list ("name type, ...") for
// shape-mismatch diagnostics, so a wrong matcher self-incriminates: the
// reason shows exactly what the matcher saw, not just what it wanted.
func describeFields(fields []MessageFieldDef) string {
	if len(fields) == 0 {
		return "(no fields)"
	}
	parts := make([]string, 0, len(fields))
	for _, f := range fields {
		t := f.ProtoType
		if f.MessageType != "" {
			t = f.MessageType
		}
		parts = append(parts, f.Name+" "+t)
	}
	return strings.Join(parts, ", ")
}
