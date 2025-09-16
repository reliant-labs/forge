package naming

import "testing"

func TestToPascalCase(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"api-gateway", "APIGateway"},
		{"db-service", "DBService"},
		{"http_client", "HTTPClient"},
		{"database_url", "DatabaseURL"},
		{"simple", "Simple"},
		{"", ""},
		{"a", "A"},
		{"id", "ID"},
		{"user_id", "UserID"},
		{"uuid", "UUID"},
		{"mcp-server", "MCPServer"},
		{"sql_db", "SQLDB"},
	}
	for _, tt := range tests {
		got := ToPascalCase(tt.input)
		if got != tt.want {
			t.Errorf("ToPascalCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestToExportedFieldName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"api", "API"},
		{"db", "DB"},
		{"mcp", "MCP"},
		{"orders", "Orders"},
		{"notifications", "Notifications"},
		{"", ""},
		{"id", "ID"},
		{"uuid", "UUID"},
	}
	for _, tt := range tests {
		got := ToExportedFieldName(tt.input)
		if got != tt.want {
			t.Errorf("ToExportedFieldName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"firstName", "first_name"},
		{"LastName", "last_name"},
		{"already_snake", "already_snake"},
		{"HTTPStatus", "http_status"},
		{"userID", "user_id"},
		{"UPPER_CASE", "upper_case"},
		{"simple", "simple"},
		{"", ""},
		{"a", "a"},
		{"A", "a"},
		{"createdAt", "created_at"},
		{"XMLParser", "xml_parser"},
		// Multi-byte runes must not confuse prev/next lookups (byte vs rune index bug).
		{"café", "café"},
		{"CaféName", "café_name"},
		{"αBeta", "α_beta"},
	}
	for _, tt := range tests {
		got := ToSnakeCase(tt.input)
		if got != tt.want {
			t.Errorf("ToSnakeCase(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestApplyGoInitialisms_LongerFirst(t *testing.T) {
	// "Uuid" should become "UUID", not "UuID" (which would happen if "Id" was processed first)
	got := applyGoInitialisms("Uuid")
	if got != "UUID" {
		t.Errorf("applyGoInitialisms(%q) = %q, want %q", "Uuid", got, "UUID")
	}

	// "Xsrf" should become "XSRF", not get partially mangled
	got = applyGoInitialisms("Xsrf")
	if got != "XSRF" {
		t.Errorf("applyGoInitialisms(%q) = %q, want %q", "Xsrf", got, "XSRF")
	}
}