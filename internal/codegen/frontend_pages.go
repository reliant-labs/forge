package codegen

import (
	"strings"
	"unicode"

	"github.com/jinzhu/inflection"

	"github.com/reliant-labs/forge/internal/naming"
)

// PageTemplateData holds data for rendering a single entity's CRUD pages.
type PageTemplateData struct {
	EntityName       string // "Task" (PascalCase)
	EntityNamePlural string // "Tasks"
	EntitySlug       string // "tasks" (kebab-case for URL)
	ServiceName      string // "TaskService"
	ServiceNameCamel string // "taskService"
	HooksImportPath  string // "@/hooks/task-service-hooks"
	TypesImportPath  string // "@/gen/services/tasks/v1/tasks_pb"
	ListRPC          string // "ListTasks" (PascalCase, matching hook name)
	GetRPC           string // "GetTask"
	CreateRPC        string // "CreateTask"
	UpdateRPC        string // "UpdateTask"
	DeleteRPC        string // "DeleteTask"
	HasList          bool
	HasGet           bool
	HasCreate        bool
	HasUpdate        bool
	HasDelete        bool
	// ItemsField is the camelCase (protojson) accessor for the list
	// response's repeated field — the array the list hook's `data` holds.
	// It is the ACTUAL repeated proto field name on the ListXxxResponse
	// message (e.g. `keys` for `ListLLMKeysResponse { repeated LLMKey keys
	// = 1; }`), NOT the camelCased entity plural. They usually coincide
	// ("tasks" for ListTasksResponse.tasks) but diverge whenever the proto
	// names the field differently — and a wrong accessor silently yields
	// `undefined`, breaking the list page, the dashboard count tile, and
	// the mock transport's response shape all at once. Falls back to the
	// camelCased plural for older descriptors that don't carry the field.
	ItemsField   string
	CreateFields []PageField // Fields for the create form
	UpdateFields []PageField // Fields for the edit form
	// UpdateEntityFieldCamel is the camelCase request field wrapping the
	// entity when the update request follows AIP-134 ("task" for
	// `Task task = 1;`). The edit page then nests the form values under
	// it (with the PK inside) instead of spreading them at the top level.
	// Empty for flat update requests (legacy id+fields shape).
	UpdateEntityFieldCamel string
	// UpdateMaskFieldCamel is the camelCase google.protobuf.FieldMask
	// field on the update request ("updateMask"); the edit page sends a
	// mask naming exactly the form's fields so the server's masked write
	// can't clobber columns the form never edits. Empty when the request
	// has no mask.
	UpdateMaskFieldCamel string
	// Response type names for imports
	ListResponseType   string // "ListTasksResponse"
	GetResponseType    string // "GetTaskResponse"
	CreateRequestType  string // "CreateTaskRequest"
	CreateResponseType string // "CreateTaskResponse"
	UpdateRequestType  string // "UpdateTaskRequest"
	GetRequestType     string // "GetTaskRequest"
	DeleteRequestType  string // "DeleteTaskRequest"

	// ── Entity-derived metadata (AttachEntityMeta) ──────────────────
	// The generator KNOWS the entity's fields from the proto descriptor,
	// so page templates emit typed column/field declarations instead of
	// casting proto messages to Record<string, unknown> and reflecting
	// over Object.keys at runtime.

	// EntityTypeImportPath is the TS module declaring the entity type
	// ("@/gen/db/v1/tasks_pb"). May differ from TypesImportPath when the
	// entity message lives in its own proto file.
	EntityTypeImportPath string
	// Columns drives the list page's typed column array and the detail
	// page's field rows. Only renderable kinds are included (scalars,
	// enums, timestamps, repeated scalars) — nested messages and maps
	// don't belong in a table cell.
	Columns []EntityPageField
	// SearchFields are the camelCase string-typed fields client-side
	// search filters over. Empty → the list page omits the search box.
	SearchFields []string
	// DisplayField is the camelCase string field used as the human title
	// ("name", then "title"); empty when the entity has neither.
	DisplayField string
	// PkFieldCamel is the camelCase primary-key field ("id").
	PkFieldCamel string
	// HasBadgeColumns reports whether any column renders as a Badge —
	// gates the Badge / enumBadgeVariant imports in page templates.
	HasBadgeColumns bool
	// HasDateCreateFields / HasDateUpdateFields gate the timestamp
	// conversion imports (timestampFromDate / toDatetimeLocal) in the
	// create and edit form templates.
	HasDateCreateFields bool
	HasDateUpdateFields bool
}

// EntityPageField is one renderable entity field for list columns /
// detail rows.
type EntityPageField struct {
	Name    string // camelCase TS field name: "createdAt"
	Label   string // display label: "Created At"
	IsBadge bool   // render as a status Badge (enum kind or enum-like string name)
}

// PageField represents a form field derived from a proto message field.
type PageField struct {
	Name  string // "title" (camelCase)
	Label string // "Title" (display name)
	Type  string // "text", "number", "checkbox", "date", "textarea"
	// ProtoName is the original snake_case proto field name ("created_at")
	// — the AIP-134 update_mask path for this field.
	ProtoName string
	Required  bool
	ProtoType string // original proto type for reference
	// IsBigInt marks 64-bit integer fields — protobuf-es types them as
	// bigint, so form submissions convert the zod number before mutate().
	IsBigInt bool
	// IsRepeated marks repeated scalar fields (descriptor ProtoType
	// "[]string" etc.). The form renders a comma-separated text input and
	// the submit handler splits it back into the array the RPC expects —
	// without the split, the generated page assigned a string to a
	// string[] request field and failed the TypeScript build.
	IsRepeated bool
	// RepeatedNumeric marks repeated numeric fields whose elements need
	// Number() conversion after the comma split.
	RepeatedNumeric bool
}

// isRepeatedScalarProtoType reports whether a descriptor message-field
// proto type is a repeated scalar ("[]string", "[]int32", ...).
// Repeated message fields carry "[]message" and are not form-mappable.
func isRepeatedScalarProtoType(protoType string) bool {
	base, ok := strings.CutPrefix(protoType, "[]")
	if !ok {
		return false
	}
	return base != "message" && base != "enum"
}

// isRepeatedNumericProtoType reports whether the repeated element type is
// numeric (form values need Number() conversion after the comma split).
func isRepeatedNumericProtoType(protoType string) bool {
	switch strings.TrimPrefix(protoType, "[]") {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64", "float", "double":
		return true
	}
	return false
}

// isFormFieldRequired determines whether a form field should be marked as required.
// Fields with the proto optional keyword, booleans, timestamps, enums, message
// types, and repeated fields are never required in forms.
func isFormFieldRequired(f MessageFieldDef) bool {
	if f.IsOptional {
		return false
	}
	if strings.HasPrefix(f.ProtoType, "[]") {
		return false
	}
	switch f.ProtoType {
	case "bool", "google.protobuf.Timestamp", "Timestamp", "enum", "message":
		return false
	}
	return true
}

// protoTypeToFormField maps proto field types to HTML form input types.
func protoTypeToFormField(protoType string) string {
	// Repeated scalars render as a comma-separated text input; the page's
	// submit handler splits the value back into the array.
	if strings.HasPrefix(protoType, "[]") {
		return "text"
	}
	switch protoType {
	case "bool":
		return "checkbox"
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64",
		"fixed32", "fixed64", "sfixed32", "sfixed64":
		return "number"
	case "float", "double":
		return "number"
	case "google.protobuf.Timestamp", "Timestamp":
		return "date"
	case "bytes":
		return "textarea"
	default:
		return "text"
	}
}

// fieldNameToLabel converts a snake_case or camelCase field name to a display label.
// "first_name" → "First Name", "email" → "Email"
func fieldNameToLabel(name string) string {
	// Handle snake_case
	if strings.Contains(name, "_") {
		parts := strings.Split(name, "_")
		for i, p := range parts {
			if len(p) > 0 {
				parts[i] = strings.ToUpper(p[:1]) + p[1:]
			}
		}
		return strings.Join(parts, " ")
	}
	// Handle camelCase — insert spaces before uppercase letters
	var result strings.Builder
	for i, r := range name {
		if i > 0 && unicode.IsUpper(r) {
			result.WriteRune(' ')
		}
		if i == 0 {
			result.WriteRune(unicode.ToUpper(r))
		} else {
			result.WriteRune(r)
		}
	}
	return result.String()
}

// fieldNameToCamel converts a snake_case field name to camelCase.
// "first_name" → "firstName", "email" → "email"
func fieldNameToCamel(name string) string {
	if !strings.Contains(name, "_") {
		return name
	}
	parts := strings.Split(name, "_")
	for i := 1; i < len(parts); i++ {
		if len(parts[i]) > 0 {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// createFieldSkipList contains field names that should not appear in create forms.
var createFieldSkipList = map[string]bool{
	"id":          true,
	"created_at":  true,
	"updated_at":  true,
	"deleted_at":  true,
	"create_time": true,
	"update_time": true,
	"delete_time": true,
}

// ExtractCRUDEntities analyzes a service's methods and returns PageTemplateData
// for each entity that has CRUD-pattern RPCs.
func ExtractCRUDEntities(svc ServiceDef) []PageTemplateData {
	// Group methods by entity name
	type entityMethods struct {
		listRPC    string
		getRPC     string
		createRPC  string
		updateRPC  string
		deleteRPC  string
		listResp   string
		getResp    string
		createReq  string
		createResp string
		updateReq  string
		getReq     string
		deleteReq  string
	}

	entities := make(map[string]*entityMethods)
	entityOrder := []string{} // preserve discovery order

	for _, m := range svc.Methods {
		if m.ClientStreaming || m.ServerStreaming {
			continue
		}

		op, rawEntity := parseCRUDOperation(m.Name)
		if op == "" {
			continue
		}

		// Normalize: for "list", the method uses plural form — singularize
		entityName := rawEntity
		if op == "list" {
			entityName = inflection.Singular(rawEntity)
		}

		em, ok := entities[entityName]
		if !ok {
			em = &entityMethods{}
			entities[entityName] = em
			entityOrder = append(entityOrder, entityName)
		}

		switch op {
		case "list":
			em.listRPC = m.Name
			em.listResp = m.OutputType
		case "get":
			em.getRPC = m.Name
			em.getReq = m.InputType
			em.getResp = m.OutputType
		case "create":
			em.createRPC = m.Name
			em.createReq = m.InputType
			em.createResp = m.OutputType
		case "update":
			em.updateRPC = m.Name
			em.updateReq = m.InputType
		case "delete":
			em.deleteRPC = m.Name
			em.deleteReq = m.InputType
		}
	}

	hooksFile := strings.TrimSuffix(naming.ServiceHookFile(svc.Name), ".ts")
	importPath := ProtoFileToTSImportPath(svc.ProtoFile)

	var pages []PageTemplateData
	for _, entityName := range entityOrder {
		em := entities[entityName]

		// Only generate pages for real entities with at least a List RPC
		// or both Get and Create. A lone Get (e.g., GetStatus) is not
		// sufficient — it's likely a non-CRUD RPC.
		if em.listRPC == "" && (em.getRPC == "" || em.createRPC == "") {
			continue
		}

		plural := inflection.Plural(entityName)
		slug := PascalToKebab(plural)

		itemsField := listItemsField(svc, em.listResp, plural)

		data := PageTemplateData{
			EntityName:         entityName,
			EntityNamePlural:   plural,
			EntitySlug:         slug,
			ServiceName:        svc.Name,
			ServiceNameCamel:   toCamelCaseFromPascal(svc.Name),
			HooksImportPath:    "@/hooks/" + hooksFile,
			TypesImportPath:    "@/gen/" + importPath,
			ListRPC:            em.listRPC,
			GetRPC:             em.getRPC,
			CreateRPC:          em.createRPC,
			UpdateRPC:          em.updateRPC,
			DeleteRPC:          em.deleteRPC,
			HasList:            em.listRPC != "",
			HasGet:             em.getRPC != "",
			HasCreate:          em.createRPC != "",
			HasUpdate:          em.updateRPC != "",
			HasDelete:          em.deleteRPC != "",
			ItemsField:         itemsField,
			ListResponseType:   em.listResp,
			GetResponseType:    em.getResp,
			CreateRequestType:  em.createReq,
			CreateResponseType: em.createResp,
			GetRequestType:     em.getReq,
			UpdateRequestType:  em.updateReq,
			DeleteRequestType:  em.deleteReq,
		}

		// Extract form fields from the create request message
		if em.createReq != "" && svc.Messages != nil {
			if fields, ok := svc.Messages[em.createReq]; ok {
				for _, f := range fields {
					if createFieldSkipList[f.Name] {
						continue
					}
					pf := pageFieldFromMessageField(f)
					if pf.Type == "date" {
						data.HasDateCreateFields = true
					}
					data.CreateFields = append(data.CreateFields, pf)
				}
			}
		}

		// Extract form fields for the edit page. The canonical generated
		// update request follows AIP-134 — it WRAPS the entity
		// (`Task task = 1;`) and carries a `google.protobuf.FieldMask
		// update_mask` — so the form fields come from the ENTITY message,
		// the submit nests them under the wrapper field, and the mask
		// names exactly the fields the form edits (without it the
		// server's update clobbers every column the form doesn't carry).
		// A flat request (id + scalar fields) keeps the legacy top-level
		// spread with no mask.
		if em.updateReq != "" && svc.Messages != nil {
			if fields, ok := svc.Messages[em.updateReq]; ok {
				formFields := fields
				for _, f := range fields {
					if isFieldMaskField(f) {
						data.UpdateMaskFieldCamel = fieldNameToCamel(f.Name)
						continue
					}
					if fieldMatchesEntity(f, entityName) {
						data.UpdateEntityFieldCamel = fieldNameToCamel(f.Name)
						if entityFields, ok := svc.Messages[localMessageName(f.MessageType)]; ok {
							formFields = entityFields
						}
					}
				}
				for _, f := range formFields {
					if createFieldSkipList[f.Name] {
						continue
					}
					// Skip the id field — it's set from the URL param
					if f.Name == "id" {
						continue
					}
					// Never render the mask or the entity wrapper itself
					// as form inputs (pre-AIP-134 pages did, producing a
					// dead "Update Mask" text box).
					if isFieldMaskField(f) {
						continue
					}
					if data.UpdateEntityFieldCamel != "" && fieldMatchesEntity(f, entityName) {
						continue
					}
					// Nested messages (other than timestamps) have no form
					// input representation.
					if f.MessageType != "" && f.MessageType != "google.protobuf.Timestamp" {
						continue
					}
					pf := pageFieldFromMessageField(f)
					if pf.Type == "date" {
						data.HasDateUpdateFields = true
					}
					data.UpdateFields = append(data.UpdateFields, pf)
				}
			}
		}

		pages = append(pages, data)
	}

	return pages
}

// listItemsField returns the camelCase (protojson) accessor for the
// repeated field on a ListXxxResponse message — i.e. the array the list
// hook's `data` actually holds. It reads the response descriptor's first
// repeated field (descriptors encode repeated fields with a "[]" ProtoType
// prefix) rather than deriving the camelCased entity plural, because the
// proto is free to name the field differently (e.g.
// `ListLLMKeysResponse { repeated LLMKey keys = 1; }` → `keys`, not
// `llmKeys`). When the descriptor carries no repeated field (older
// descriptors, or a non-standard list response) it falls back to the
// camelCased plural, preserving prior behavior.
func listItemsField(svc ServiceDef, listResp, plural string) string {
	if listResp != "" && svc.Messages != nil {
		if fields, ok := svc.Messages[listResp]; ok {
			for _, f := range fields {
				if strings.HasPrefix(f.ProtoType, "[]") {
					return fieldNameToCamel(f.Name)
				}
			}
		}
	}
	return ToCamelCaseFromPascalExport(plural)
}

// pageFieldFromMessageField builds the form-field projection of one proto
// message field. Timestamp message fields map to a date input via their
// MessageType (descriptors collapse every message field's ProtoType to the
// literal "message", which the form-type switch can't match).
func pageFieldFromMessageField(f MessageFieldDef) PageField {
	effectiveType := f.ProtoType
	if f.MessageType == "google.protobuf.Timestamp" {
		effectiveType = f.MessageType
	}
	return PageField{
		Name:            fieldNameToCamel(f.Name),
		Label:           fieldNameToLabel(f.Name),
		Type:            protoTypeToFormField(effectiveType),
		ProtoName:       f.Name,
		Required:        isFormFieldRequired(f),
		ProtoType:       f.ProtoType,
		IsBigInt:        isBigIntProtoType(f.ProtoType),
		IsRepeated:      isRepeatedScalarProtoType(f.ProtoType),
		RepeatedNumeric: isRepeatedNumericProtoType(f.ProtoType),
	}
}

// isFieldMaskField reports whether a message field is an AIP-134
// google.protobuf.FieldMask (by referenced type, with a name fallback for
// older descriptors that don't carry MessageType).
func isFieldMaskField(f MessageFieldDef) bool {
	if f.MessageType == "google.protobuf.FieldMask" || strings.HasSuffix(f.MessageType, ".FieldMask") || f.MessageType == "FieldMask" {
		return true
	}
	return f.MessageType == "" && f.Name == "update_mask"
}

// localMessageName strips any package qualifier from a referenced message
// name ("services.tasks.v1.Task" → "Task") so it can key svc.Messages,
// which indexes by local name.
func localMessageName(messageType string) string {
	if idx := strings.LastIndex(messageType, "."); idx >= 0 {
		return messageType[idx+1:]
	}
	return messageType
}

// enumLikeNameFragments mirrors the isEnumLike heuristic in the emitted
// format-utils.ts: string fields whose names suggest a closed value set
// render as status badges.
var enumLikeNameFragments = []string{
	"status", "type", "kind", "role", "state", "category", "priority", "level",
}

func isEnumLikeFieldName(name string) bool {
	lower := strings.ToLower(name)
	for _, frag := range enumLikeNameFragments {
		if strings.Contains(lower, frag) {
			return true
		}
	}
	return false
}

// AttachEntityMeta enriches a PageTemplateData with typed field metadata
// from the matched proto entity definition. The page generator calls this
// after pairing a CRUD RPC group with its EntityDef — the same pairing
// that gates page emission — so templates can emit fully typed columns,
// search fields, and detail rows.
func AttachEntityMeta(page *PageTemplateData, entity EntityDef) {
	importSource := entity.ProtoFile
	if importSource == "" {
		// Entity declared in the service's proto file.
		page.EntityTypeImportPath = page.TypesImportPath
	} else {
		page.EntityTypeImportPath = "@/gen/" + ProtoFileToTSImportPath(importSource)
	}

	page.PkFieldCamel = fieldNameToCamel(entity.PkField)
	if page.PkFieldCamel == "" {
		page.PkFieldCamel = "id"
	}

	for _, f := range entity.Fields {
		switch f.Kind {
		case FieldKindMessage, FieldKindMap, FieldKindRepeatedMessage:
			// Nested structures don't render in a table cell / detail row.
			continue
		}
		// The soft-delete column is machinery, not data: generated reads
		// filter `deleted_at IS NULL`, so the cell is always empty — a
		// dead column that makes the UI look broken.
		if f.Name == "deleted_at" || f.Name == "delete_time" {
			continue
		}

		camel := fieldNameToCamel(f.Name)
		isBadge := f.Kind == FieldKindEnum || (f.ProtoType == "string" && isEnumLikeFieldName(f.Name))
		if isBadge {
			page.HasBadgeColumns = true
		}
		page.Columns = append(page.Columns, EntityPageField{
			Name:    camel,
			Label:   fieldNameToLabel(f.Name),
			IsBadge: isBadge,
		})

		if f.ProtoType == "string" {
			page.SearchFields = append(page.SearchFields, camel)
		}
		if page.DisplayField == "" && f.ProtoType == "string" && (f.Name == "name" || f.Name == "title") {
			page.DisplayField = camel
		}
	}

	// Prefer "name" over "title" when both exist.
	for _, f := range entity.Fields {
		if f.Name == "name" && f.ProtoType == "string" {
			page.DisplayField = fieldNameToCamel(f.Name)
			break
		}
	}
}

// PascalToKebab converts PascalCase to kebab-case, respecting Go
// initialisms (LLM, API, URL, JSON, …) so that "LLMGateway" produces
// "llm-gateway" rather than "l-l-m-gateway".
//
// Thin wrapper around naming.ToKebabCase — kept here for backwards
// compatibility with existing callers (frontend_pages, frontend_mocks,
// related tests). New code should call naming.ToKebabCase directly.
func PascalToKebab(s string) string {
	return naming.ToKebabCase(s)
}
