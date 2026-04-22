package codegen

import "testing"

func TestToCamelCaseFromPascal(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"GetUser", "getUser"},
		{"CreateUser", "createUser"},
		{"RPCMethod", "rpcMethod"},
		{"ID", "id"},
		{"A", "a"},
		{"", ""},
		{"alreadyCamel", "alreadyCamel"},
		{"ListUsers", "listUsers"},
	}
	for _, tt := range tests {
		got := toCamelCaseFromPascal(tt.input)
		if got != tt.want {
			t.Errorf("toCamelCaseFromPascal(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsQueryMethod(t *testing.T) {
	queries := []string{
		"GetUser", "ListUsers", "SearchProducts", "FindItem",
		"CheckPermission", "HasAccess", "IsAdmin", "CountOrders", "ExistsUser",
	}
	mutations := []string{
		"CreateUser", "UpdateUser", "DeleteUser", "SetConfig",
		"AddItem", "RemoveItem", "SendNotification",
	}

	for _, name := range queries {
		if !isQueryMethod(name) {
			t.Errorf("isQueryMethod(%q) = false, want true", name)
		}
	}
	for _, name := range mutations {
		if isQueryMethod(name) {
			t.Errorf("isQueryMethod(%q) = true, want false", name)
		}
	}
}

func TestProtoFileToTSImportPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"proto/services/users/v1/users.proto", "services/users/v1/users_pb"},
		{"proto/services/echo/v1/echo.proto", "services/echo/v1/echo_pb"},
		{"services/billing/v1/billing.proto", "services/billing/v1/billing_pb"},
	}
	for _, tt := range tests {
		got := ProtoFileToTSImportPath(tt.input)
		if got != tt.want {
			t.Errorf("ProtoFileToTSImportPath(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestServiceDefToHookData(t *testing.T) {
	svc := ServiceDef{
		Name:      "UserService",
		ProtoFile: "proto/services/users/v1/users.proto",
		Methods: []Method{
			{Name: "GetUser", InputType: "GetUserRequest", OutputType: "GetUserResponse"},
			{Name: "CreateUser", InputType: "CreateUserRequest", OutputType: "CreateUserResponse"},
			{Name: "ListUsers", InputType: "ListUsersRequest", OutputType: "ListUsersResponse"},
			{Name: "StreamUpdates", InputType: "StreamRequest", OutputType: "StreamResponse", ServerStreaming: true},
		},
	}

	data := ServiceDefToHookData(svc)

	if data.ServiceName != "UserService" {
		t.Errorf("ServiceName = %q, want %q", data.ServiceName, "UserService")
	}
	if data.ServiceNameCamel != "userService" {
		t.Errorf("ServiceNameCamel = %q, want %q", data.ServiceNameCamel, "userService")
	}
	if data.ImportPath != "services/users/v1/users_pb" {
		t.Errorf("ImportPath = %q, want %q", data.ImportPath, "services/users/v1/users_pb")
	}

	// Should have 3 methods (streaming one skipped)
	if len(data.Methods) != 3 {
		t.Fatalf("got %d methods, want 3", len(data.Methods))
	}

	// GetUser should be a query
	if !data.Methods[0].IsQuery {
		t.Error("GetUser should be IsQuery=true")
	}
	if data.Methods[0].NameCamel != "getUser" {
		t.Errorf("GetUser.NameCamel = %q, want %q", data.Methods[0].NameCamel, "getUser")
	}

	// CreateUser should be a mutation
	if data.Methods[1].IsQuery {
		t.Error("CreateUser should be IsQuery=false")
	}

	// ListUsers should be a query
	if !data.Methods[2].IsQuery {
		t.Error("ListUsers should be IsQuery=true")
	}
}