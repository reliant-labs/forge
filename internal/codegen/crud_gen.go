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
	CRUDMethods  []CRUDMethodTemplateData
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
	HasOrderBy        bool              // true if list method has order_by field
	HasTenant         bool              // true when the entity has a tenant key field
	TenantGoName      string            // e.g., "OrgId", "TenantId" (PascalCase Go field name on entity)
	TenantColumnName  string            // e.g., "org_id", "tenant_id"
	UpdateEntityField string            // e.g., "Project" — Go field name in the update request that holds the entity
	CreateFields      []CreateFieldData // fields from the create request message
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

	relDir := filepath.Join("handlers", filepath.FromSlash(res.ImportLeaf))
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

	// Tier-1 projection: the per-entity wiring ops. Only emitted when at
	// least one method passed shape validation (mismatched methods get an
	// explanatory stub in the user-owned shim instead).
	if data.NeedsCRUDLib {
		content, rerr := templates.ServiceTemplates().Render("handlers_crud_ops_gen.go.tmpl", data)
		if rerr != nil {
			return fmt.Errorf("render handlers_crud_ops_gen.go.tmpl: %w", rerr)
		}
		if _, werr := checksums.WriteGeneratedFile(projectDir, opsRel, content, cs, true); werr != nil {
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

// removeRetiredForgeFile deletes a generated file forge no longer emits,
// plus its manifest entry — but never a file the user owns (Tier-2 /
// disowned / legacy-forked entries, or an untracked file without the
// forge banner).
func removeRetiredForgeFile(projectDir, relPath string, cs *checksums.FileChecksums) {
	fullPath := filepath.Join(projectDir, relPath)
	if cs != nil {
		if entry, ok := cs.Files[relPath]; ok {
			if entry.Tier == 2 || entry.Disowned || entry.Forked {
				return // user-owned now — not forge's to delete
			}
			_ = os.Remove(fullPath)
			delete(cs.Files, relPath)
			return
		}
	}
	// Untracked: only sweep what self-identifies as forge output.
	body, err := os.ReadFile(fullPath)
	if err != nil {
		return
	}
	if bytes.Contains(body, []byte("Code generated by forge")) {
		_ = os.Remove(fullPath)
	}
}

// crudShimImports computes the import block for the user-owned
// handlers_crud.go shim from the methods it will contain.
func crudShimImports(data CRUDTemplateData) []string {
	imports := []string{"context", "connectrpc.com/connect"}
	hasMismatch := false
	hasReal := false
	for _, m := range data.CRUDMethods {
		if m.ShapeMismatch {
			hasMismatch = true
		} else {
			hasReal = true
		}
	}
	if hasMismatch {
		imports = append(imports, "fmt")
	}
	if hasReal {
		imports = append(imports, "github.com/reliant-labs/forge/pkg/crud")
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
		if err := os.WriteFile(fullPath, content, 0o644); err != nil {
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
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("append CRUD shims to %s: %w", shimRel, err)
	}
	recordTier2(cs, shimRel, []byte(content))
	fmt.Printf("  ✅ Appended %d new CRUD shim(s) to %s (user-owned)\n", len(missing), shimRel)
	return nil
}

// warnCustomReadShapeStubs prints one loud line per RPC whose shim is
// being written as a custom-read-shape stub (CodeUnimplemented body).
// A stub is the system WORKING — the request/response shape is a
// legitimate domain decision and the body is the user's to implement —
// but it must never land silently: until the body exists, production
// traffic to the RPC 501s. When the RPC previously had a real generated
// implementation (the retired handlers_crud_gen.go carried a non-stub
// body for it), say so explicitly — that's a behavior regression on a
// live procedure, and a downstream agent filed a near-miss with live
// traffic on exactly this transition.
func warnCustomReadShapeStubs(methods []CRUDMethodTemplateData, shimRel string, previouslyImplemented map[string]bool) {
	for _, m := range methods {
		if !m.ShapeMismatch {
			continue
		}
		if previouslyImplemented[m.MethodName] {
			fmt.Printf("  ⚠️  %s: REPLACING a previously generated implementation with an Unimplemented stub in %s (custom read shape: %s) — implement the body before serving traffic\n",
				m.MethodName, shimRel, m.MismatchReason)
			continue
		}
		fmt.Printf("  ⚠️  %s: custom read shape (%s) — scaffolded an Unimplemented stub in %s; the body is yours to implement\n",
			m.MethodName, m.MismatchReason, shimRel)
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

// recordTier2 tracks a user-owned scaffold in the manifest at Tier-2 so
// the cleanup sweep never treats it as a stale Tier-1 artifact.
func recordTier2(cs *checksums.FileChecksums, relPath string, content []byte) {
	if cs == nil {
		return
	}
	cs.RecordFile(relPath, content)
	entry := cs.Files[relPath]
	entry.Tier = 2
	cs.Files[relPath] = entry
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
		// AIP-158-style template emits req.PageSize / req.PageToken
		// when the input type follows List*Request naming. If we
		// have field data and either is missing, the generated body
		// fails to compile against the real proto (kalshi-trader's
		// ListMarketsRequest carries `Limit` instead, for instance).
		if inputKnown && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			if _, hasSize := inputByName["page_size"]; !hasSize {
				return false, fmt.Sprintf("request %s lacks page_size (AIP-158 pagination assumed by template; observed fields: %s)", cm.Method.InputType, describeFields(inputFields))
			}
			if _, hasTok := inputByName["page_token"]; !hasTok {
				return false, fmt.Sprintf("request %s lacks page_token (AIP-158 pagination assumed by template; observed fields: %s)", cm.Method.InputType, describeFields(inputFields))
			}
		}
		// List response template emits `<EntityPlural>: items` and
		// optionally `NextPageToken: nextPageToken`. Validate the
		// response carries a repeated entity field by that name.
		if outputKnown {
			pluralLower := strings.ToLower(inflection.Plural(cm.Entity.Name))
			if _, has := outputByName[pluralLower]; !has {
				return false, fmt.Sprintf("response %s lacks repeated %s field %s (observed fields: %s)", cm.Method.OutputType, cm.Entity.Name, pluralLower, describeFields(outputFields))
			}
		}
	case "create":
		// Create response template emits `<EntityName>: entity`.
		// Validate the response carries a single field of that type
		// (named after the entity in snake_case).
		if outputKnown {
			lower := strings.ToLower(cm.Entity.Name)
			if _, has := outputByName[lower]; !has {
				return false, fmt.Sprintf("response %s lacks %s field %s (observed fields: %s)", cm.Method.OutputType, cm.Entity.Name, lower, describeFields(outputFields))
			}
		}
	}
	return true, ""
}

func buildCRUDTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath string) (CRUDTemplateData, error) {
	// Synthesized Package is a placeholder only: GenerateCRUDHandlers
	// overrides it with the disk-resolved package clause before rendering
	// (the file lands inside an EXISTING handler dir).
	pkg := naming.ServicePackage(svc.Name)

	// Build ProtoPackage path (same logic as mapServiceDefToTemplateData)
	protoPackage := ""
	if svc.ModulePath != "" && svc.GoPackage != "" {
		prefix := svc.ModulePath + "/gen/"
		if strings.HasPrefix(svc.GoPackage, prefix) {
			protoPackage = strings.TrimPrefix(svc.GoPackage, prefix)
			if idx := strings.LastIndex(protoPackage, "/v"); idx >= 0 {
				protoPackage = protoPackage[:idx]
			}
		}
	}

	var methods []CRUDMethodTemplateData
	for _, cm := range crudMethods {
		authAction := operationToAuthAction(cm.Operation)

		// Validate the request/response shape up front. When the
		// observed proto messages don't match the AIP-158-style body
		// the template emits, we still emit a method (so the proto's
		// RPC interface is satisfied) but route it to a tagged
		// CodeUnimplemented stub instead of the body. This keeps
		// handlers_crud_gen.go compiling against bespoke shapes
		// (Limit/enum filters, string Ticker keys, repeated-message
		// responses).
		shapeOK, shapeReason := validateCRUDShape(svc, cm)

		// Detect pagination for list operations: check if the input type
		// follows AIP-158 naming (List<Entity>Request implies page_size).
		// Skip when the shape didn't match — the stub branch doesn't
		// dereference PageSize/PageToken so suppressing keeps the
		// generated file from importing the crud lib unnecessarily.
		hasPagination := false
		paginationStyle := ""
		if shapeOK && cm.Operation == "list" && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			hasPagination = true
			paginationStyle = "cursor"
		}

		// Detect filters and ordering from request message fields.
		// Skip when the shape didn't match: classifyFilterField would
		// otherwise happily turn a bespoke field like `ticker` (a
		// string PK) or a `kalshi_status` enum into a synthetic
		// `WhereEq("ticker", req.Ticker)` clause that fails to compile
		// against the real request type.
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
					ff, ferr := classifyEntityFilterField(mf, cm.Entity)
					if ferr != nil {
						// LOUD by design: a filter the generator cannot map
						// to a declared column must fail the generate, not
						// ship a phantom-column query that silently returns
						// nothing (or leaks SQL errors) at runtime.
						return CRUDTemplateData{}, fmt.Errorf("%s.%s: %w", svc.Name, cm.Method.Name, ferr)
					}
					filterFields = append(filterFields, ff)
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

		// Collect fields from the create request message for entity construction.
		// Skip on shape mismatch — the stub doesn't reference these.
		var createFields []CreateFieldData
		if shapeOK && cm.Operation == "create" && svc.Messages != nil {
			if fields, ok := svc.Messages[cm.Method.InputType]; ok {
				for _, f := range fields {
					goType := ProtoTypeToGoType(f.ProtoType)
					// Try to get richer GoType from the entity definition
					for _, ef := range cm.Entity.Fields {
						if ef.Name == f.Name {
							goType = ef.GoType
							break
						}
					}
					kind := DetermineFieldKind(f.ProtoType, goType)
					var enumGoType string
					if kind == FieldKindEnum {
						enumGoType = goType
					}
					createFields = append(createFields, CreateFieldData{
						ProtoGoName:  naming.ToProtoPascalCase(f.Name),
						EntityGoName: naming.ToProtoPascalCase(f.Name),
						Kind:         kind,
						GoType:       goType,
						EnumGoType:   enumGoType,
					})
				}
			}
		}

		methods = append(methods, CRUDMethodTemplateData{
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
			CreateFields:      createFields,
			ShapeMismatch:     !shapeOK,
			MismatchReason:    shapeReason,
		})
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

	return CRUDTemplateData{
		Package:       pkg,
		Module:        modulePath,
		ProtoPackage:  protoPackage,
		DBPackagePath: modulePath + "/internal/db",
		HasPagination: hasPagination,
		HasFilters:    hasFilters,
		HasOrderBy:    hasOrderBy,
		NeedsORM:      needsORM,
		HasTenant:     hasTenant,
		NeedsCRUDLib:  needsCRUDLib,
		CRUDMethods:   methods,
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
	// TestHelperName mirrors ServiceTemplateData.TestHelperName: the suffix
	// the bootstrap testing generator emits on `app.NewTest<X>` /
	// `app.NewTest<X>Server`. CRUD test scaffolds use this rather than
	// pascal-casing Package so the call site matches the actual factory
	// when an internal package shares the service's leaf name.
	TestHelperName string
}

// CRUDTestEntityData groups CRUD operations by entity for lifecycle tests.
type CRUDTestEntityData struct {
	EntityName        string // "Patient"
	EntityLower       string // "patient"
	PkField           string // "Id"
	PkGoType          string // "int64"
	HasCreate         bool
	HasGet            bool
	HasList           bool
	HasUpdate         bool
	HasDelete         bool
	HasAllCRUD        bool   // true if all 5 operations exist
	HasTenant         bool   // true when the entity has a tenant key field
	TenantGoName      string // e.g., "OrgId"
	TenantColumnName  string // e.g., "org_id"
	HasTimestamps     bool   // entity annotation timestamps:true — created_at is asserted set
	// MutableStringField is the Go name of the first non-PK string field
	// (e.g. "Name") — the field the lifecycle test mutates to prove
	// update actually writes. Empty when the entity has none.
	MutableStringField string
	CreateMethod       CRUDMethodTemplateData
	GetMethod         CRUDMethodTemplateData
	ListMethod        CRUDMethodTemplateData
	UpdateMethod      CRUDMethodTemplateData
	DeleteMethod      CRUDMethodTemplateData
	Fields            []CRUDTestFieldData // entity proto message fields (minus PK, minus deleted_at)
	CreateFields      []CRUDTestFieldData // fields from the CreateRequest message
	UpdateEntityField string              // Go field name holding entity in UpdateRequest, e.g. "Project"
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
	relDir := filepath.Join("handlers", filepath.FromSlash(res.ImportLeaf))

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
	if err := os.WriteFile(lifecyclePath, content, 0o644); err != nil {
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
	if cs != nil {
		delete(cs.Files, relPath)
	}
}

func buildCRUDTestTemplateData(svc ServiceDef, crudMethods []CRUDMethod, modulePath, projectDir string) CRUDTestTemplateData {
	// Synthesized Package/TestHelperName are placeholders only:
	// GenerateCRUDTests overrides both with disk-resolved values before
	// rendering — see the call site for the rationale.
	pkg := naming.ServicePackage(svc.Name)

	// Build ProtoPackage path (same logic as buildCRUDTemplateData)
	protoPackage := ""
	if svc.ModulePath != "" && svc.GoPackage != "" {
		prefix := svc.ModulePath + "/gen/"
		if strings.HasPrefix(svc.GoPackage, prefix) {
			protoPackage = strings.TrimPrefix(svc.GoPackage, prefix)
			if idx := strings.LastIndex(protoPackage, "/v"); idx >= 0 {
				protoPackage = protoPackage[:idx]
			}
		}
	}

	// Group by entity
	entityMap := make(map[string]*CRUDTestEntityData)
	var entityOrder []string

	// Build all CRUDMethodTemplateData
	var allMethods []CRUDMethodTemplateData

	for _, cm := range crudMethods {
		authAction := operationToAuthAction(cm.Operation)

		// Validate the request/response shape up front. The CRUD-body
		// generator uses the same gate to decide whether to emit a real
		// handler or a tagged CodeUnimplemented stub; the test scaffold
		// has to mirror that decision or it emits per-RPC test rows that
		// dereference fields the request type doesn't have (e.g.
		// `Id: 1` on a GetMarketRequest keyed on `string ticker`).
		shapeOK, shapeReason := validateCRUDShape(svc, cm)

		hasPagination := false
		paginationStyle := ""
		if shapeOK && cm.Operation == "list" && strings.HasPrefix(cm.Method.InputType, "List") && strings.HasSuffix(cm.Method.InputType, "Request") {
			hasPagination = true
			paginationStyle = "cursor"
		}

		// Detect filters and ordering from request message fields.
		// Skip when the shape didn't match — the stub branch doesn't
		// dereference filter fields and classifyFilterField on a
		// bespoke request shape would otherwise leak filter rows into
		// per-RPC test setup that fails to compile.
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
					ff := classifyFilterField(mf)
					filterFields = append(filterFields, ff)
				}
			}
		}

		// Determine update entity field for this method's test data.
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

		mtd := CRUDMethodTemplateData{
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
			UpdateEntityField: updateEntityField,
			ShapeMismatch:     !shapeOK,
			MismatchReason:    shapeReason,
		}
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

			mutable := ""
			for _, f := range fields {
				if f.GoType == "string" {
					mutable = f.ProtoName
					break
				}
			}

			ent = &CRUDTestEntityData{
				EntityName:         cm.Entity.Name,
				EntityLower:        strings.ToLower(cm.Entity.Name),
				PkField:            naming.ToProtoPascalCase(cm.Entity.PkField),
				PkGoType:           cm.Entity.PkGoType,
				HasTenant:          cm.Entity.HasTenant,
				TenantGoName:       cm.Entity.TenantGoName,
				TenantColumnName:   cm.Entity.TenantColumnName,
				HasTimestamps:      cm.Entity.Timestamps,
				MutableStringField: mutable,
				Fields:             fields,
				UpdateEntityField:  updateEntityField,
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

	return CRUDTestTemplateData{
		Package:        pkg,
		Module:         modulePath,
		ProtoPackage:   protoPackage,
		HasTenant:      testHasTenant,
		Entities:       entities,
		CRUDMethods:    allMethods,
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
	for _, ef := range entity.Fields {
		if ef.Name == mf.Name {
			return ff, nil
		}
	}
	return ff, fmt.Errorf(
		"list filter field %q has no matching column on entity %s (declared columns: %s); rename the request field to a declared column, or implement the RPC by hand in a sibling file",
		mf.Name, entity.Name, entityColumnList(entity))
}

// entityStringColumns returns the entity's non-PK string column names —
// the span of a "search" filter.
func entityStringColumns(entity EntityDef) []string {
	var cols []string
	for _, ef := range entity.Fields {
		if ef.Name == entity.PkField {
			continue
		}
		if ef.ProtoType == "string" {
			cols = append(cols, ef.Name)
		}
	}
	return cols
}

// entityColumnList renders the declared column names for diagnostics.
func entityColumnList(entity EntityDef) string {
	names := make([]string, 0, len(entity.Fields))
	for _, ef := range entity.Fields {
		names = append(names, ef.Name)
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

	// If the Deps struct doesn't have a DB field yet, inject it.
	if !(strings.Contains(content, "DB ") && (strings.Contains(content, "orm.Context") || strings.Contains(content, "orm.Client"))) {
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
	return os.WriteFile(servicePath, []byte(content), 0644)
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
