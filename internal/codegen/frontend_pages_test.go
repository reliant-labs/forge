package codegen

import "testing"

func TestExtractCRUDEntities_FullCRUD(t *testing.T) {
	svc := ServiceDef{
		Name:      "TaskService",
		ProtoFile: "proto/services/tasks/v1/tasks.proto",
		Methods: []Method{
			{Name: "CreateTask", InputType: "CreateTaskRequest", OutputType: "CreateTaskResponse"},
			{Name: "GetTask", InputType: "GetTaskRequest", OutputType: "GetTaskResponse"},
			{Name: "ListTasks", InputType: "ListTasksRequest", OutputType: "ListTasksResponse"},
			{Name: "UpdateTask", InputType: "UpdateTaskRequest", OutputType: "UpdateTaskResponse"},
			{Name: "DeleteTask", InputType: "DeleteTaskRequest", OutputType: "DeleteTaskResponse"},
		},
		Messages: map[string][]MessageFieldDef{
			"CreateTaskRequest": {
				{Name: "title", ProtoType: "string"},
				{Name: "description", ProtoType: "string", IsOptional: true},
				{Name: "priority", ProtoType: "int32"},
				{Name: "is_active", ProtoType: "bool"},
			},
		},
	}

	entities := ExtractCRUDEntities(svc)
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}

	e := entities[0]
	if e.EntityName != "Task" {
		t.Errorf("EntityName = %q, want %q", e.EntityName, "Task")
	}
	if e.EntityNamePlural != "Tasks" {
		t.Errorf("EntityNamePlural = %q, want %q", e.EntityNamePlural, "Tasks")
	}
	if e.EntitySlug != "tasks" {
		t.Errorf("EntitySlug = %q, want %q", e.EntitySlug, "tasks")
	}
	if e.HooksImportPath != "@/hooks/task-service-hooks" {
		t.Errorf("HooksImportPath = %q, want %q", e.HooksImportPath, "@/hooks/task-service-hooks")
	}
	if e.TypesImportPath != "@/gen/services/tasks/v1/tasks_pb" {
		t.Errorf("TypesImportPath = %q, want %q", e.TypesImportPath, "@/gen/services/tasks/v1/tasks_pb")
	}
	if !e.HasList || !e.HasGet || !e.HasCreate || !e.HasUpdate || !e.HasDelete {
		t.Errorf("expected all CRUD operations, got list=%v get=%v create=%v update=%v delete=%v",
			e.HasList, e.HasGet, e.HasCreate, e.HasUpdate, e.HasDelete)
	}
	if e.ListRPC != "ListTasks" {
		t.Errorf("ListRPC = %q, want %q", e.ListRPC, "ListTasks")
	}
	if e.GetRPC != "GetTask" {
		t.Errorf("GetRPC = %q, want %q", e.GetRPC, "GetTask")
	}
	if e.CreateRPC != "CreateTask" {
		t.Errorf("CreateRPC = %q, want %q", e.CreateRPC, "CreateTask")
	}

	// Verify form fields
	if len(e.CreateFields) != 4 {
		t.Fatalf("expected 4 create fields, got %d", len(e.CreateFields))
	}

	// title
	if e.CreateFields[0].Name != "title" || e.CreateFields[0].Type != "text" || !e.CreateFields[0].Required {
		t.Errorf("field[0] = %+v, want title/text/required", e.CreateFields[0])
	}
	// description (optional)
	if e.CreateFields[1].Name != "description" || e.CreateFields[1].Required {
		t.Errorf("field[1] = %+v, want description/optional", e.CreateFields[1])
	}
	// priority (int)
	if e.CreateFields[2].Name != "priority" || e.CreateFields[2].Type != "number" {
		t.Errorf("field[2] = %+v, want priority/number", e.CreateFields[2])
	}
	// is_active (bool)
	if e.CreateFields[3].Name != "isActive" || e.CreateFields[3].Type != "checkbox" {
		t.Errorf("field[3] = %+v, want isActive/checkbox", e.CreateFields[3])
	}
}

func TestExtractCRUDEntities_ListOnly(t *testing.T) {
	svc := ServiceDef{
		Name:      "UserService",
		ProtoFile: "proto/services/users/v1/users.proto",
		Methods: []Method{
			{Name: "ListUsers", InputType: "ListUsersRequest", OutputType: "ListUsersResponse"},
		},
	}

	entities := ExtractCRUDEntities(svc)
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}

	e := entities[0]
	if e.EntityName != "User" {
		t.Errorf("EntityName = %q, want %q", e.EntityName, "User")
	}
	if !e.HasList {
		t.Error("expected HasList=true")
	}
	if e.HasGet || e.HasCreate || e.HasUpdate || e.HasDelete {
		t.Error("expected only List operation")
	}
}

func TestExtractCRUDEntities_SkipsNonCRUD(t *testing.T) {
	svc := ServiceDef{
		Name:      "EchoService",
		ProtoFile: "proto/services/echo/v1/echo.proto",
		Methods: []Method{
			{Name: "Echo", InputType: "EchoRequest", OutputType: "EchoResponse"},
			{Name: "Ping", InputType: "PingRequest", OutputType: "PingResponse"},
		},
	}

	entities := ExtractCRUDEntities(svc)
	if len(entities) != 0 {
		t.Errorf("expected 0 entities for non-CRUD service, got %d", len(entities))
	}
}

func TestExtractCRUDEntities_SkipsStreaming(t *testing.T) {
	svc := ServiceDef{
		Name:      "TaskService",
		ProtoFile: "proto/services/tasks/v1/tasks.proto",
		Methods: []Method{
			{Name: "ListTasks", InputType: "ListTasksRequest", OutputType: "ListTasksResponse", ServerStreaming: true},
		},
	}

	entities := ExtractCRUDEntities(svc)
	if len(entities) != 0 {
		t.Errorf("expected 0 entities (streaming skipped), got %d", len(entities))
	}
}

func TestExtractCRUDEntities_SkipsAutoFields(t *testing.T) {
	svc := ServiceDef{
		Name:      "ItemService",
		ProtoFile: "proto/services/items/v1/items.proto",
		Methods: []Method{
			{Name: "CreateItem", InputType: "CreateItemRequest", OutputType: "CreateItemResponse"},
			{Name: "GetItem", InputType: "GetItemRequest", OutputType: "GetItemResponse"},
		},
		Messages: map[string][]MessageFieldDef{
			"CreateItemRequest": {
				{Name: "id", ProtoType: "string"},
				{Name: "name", ProtoType: "string"},
				{Name: "created_at", ProtoType: "Timestamp"},
				{Name: "updated_at", ProtoType: "Timestamp"},
			},
		},
	}

	entities := ExtractCRUDEntities(svc)
	if len(entities) != 1 {
		t.Fatalf("expected 1 entity, got %d", len(entities))
	}

	// Should only have "name" — id, created_at, updated_at are skipped
	if len(entities[0].CreateFields) != 1 {
		t.Fatalf("expected 1 create field, got %d: %+v", len(entities[0].CreateFields), entities[0].CreateFields)
	}
	if entities[0].CreateFields[0].Name != "name" {
		t.Errorf("expected field 'name', got %q", entities[0].CreateFields[0].Name)
	}
}

func TestExtractCRUDEntities_MultipleEntities(t *testing.T) {
	svc := ServiceDef{
		Name:      "ProjectService",
		ProtoFile: "proto/services/projects/v1/projects.proto",
		Methods: []Method{
			{Name: "ListProjects", InputType: "ListProjectsRequest", OutputType: "ListProjectsResponse"},
			{Name: "GetProject", InputType: "GetProjectRequest", OutputType: "GetProjectResponse"},
			{Name: "ListMembers", InputType: "ListMembersRequest", OutputType: "ListMembersResponse"},
			{Name: "GetMember", InputType: "GetMemberRequest", OutputType: "GetMemberResponse"},
		},
	}

	entities := ExtractCRUDEntities(svc)
	if len(entities) != 2 {
		t.Fatalf("expected 2 entities, got %d", len(entities))
	}

	if entities[0].EntityName != "Project" {
		t.Errorf("first entity = %q, want %q", entities[0].EntityName, "Project")
	}
	if entities[1].EntityName != "Member" {
		t.Errorf("second entity = %q, want %q", entities[1].EntityName, "Member")
	}
}

func TestProtoTypeToFormField(t *testing.T) {
	tests := []struct {
		proto string
		want  string
	}{
		{"string", "text"},
		{"bool", "checkbox"},
		{"int32", "number"},
		{"int64", "number"},
		{"uint32", "number"},
		{"float", "number"},
		{"double", "number"},
		{"Timestamp", "date"},
		{"google.protobuf.Timestamp", "date"},
		{"bytes", "textarea"},
		{"SomeMessage", "text"},
	}

	for _, tt := range tests {
		got := protoTypeToFormField(tt.proto)
		if got != tt.want {
			t.Errorf("protoTypeToFormField(%q) = %q, want %q", tt.proto, got, tt.want)
		}
	}
}

func TestFieldNameToLabel(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"title", "Title"},
		{"first_name", "First Name"},
		{"is_active", "Is Active"},
		{"email", "Email"},
		{"firstName", "First Name"},
	}

	for _, tt := range tests {
		got := fieldNameToLabel(tt.input)
		if got != tt.want {
			t.Errorf("fieldNameToLabel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFieldNameToCamel(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"title", "title"},
		{"first_name", "firstName"},
		{"is_active", "isActive"},
		{"already_camel", "alreadyCamel"},
	}

	for _, tt := range tests {
		got := fieldNameToCamel(tt.input)
		if got != tt.want {
			t.Errorf("fieldNameToCamel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPascalToKebab(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"Tasks", "tasks"},
		{"UserService", "user-service"},
		{"TaskItem", "task-item"},
		{"A", "a"},
	}

	for _, tt := range tests {
		got := PascalToKebab(tt.input)
		if got != tt.want {
			t.Errorf("PascalToKebab(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}