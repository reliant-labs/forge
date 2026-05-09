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
		// LLM-port additions: these acronyms surfaced as scaffolder bugs
		// where service.go referenced "LlmGatewayService" but proto-emitted
		// Go correctly produced "LLMGatewayService".
		{"llm-gateway", "LLMGateway"},
		{"jwt-auth", "JWTAuth"},
		{"io-bound", "IOBound"},
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

func TestToKebabCase(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		// Plain PascalCase
		{"UserService", "user-service"},
		{"TaskItem", "task-item"},
		{"Tasks", "tasks"},
		{"A", "a"},
		{"", ""},
		// camelCase input
		{"taskItem", "task-item"},
		// snake_case input → kebab
		{"user_service", "user-service"},
		// already-kebab input is preserved
		{"user-service", "user-service"},
		// LLM/JWT/HTTP/URL/JSON acronyms must stay glued
		{"LLMGateway", "llm-gateway"},
		{"LLMGatewayService", "llm-gateway-service"},
		{"JWTAuth", "jwt-auth"},
		{"HTTPClient", "http-client"},
		{"URLBuilder", "url-builder"},
		{"JSONParser", "json-parser"},
		{"XMLParser", "xml-parser"},
		// Trailing acronym
		{"UserID", "user-id"},
		{"TaskUUID", "task-uuid"},
		// Lone acronym
		{"LLM", "llm"},
		{"API", "api"},
		// HTTPS prefix-vs-HTTP — longer wins
		{"HTTPSConnection", "https-connection"},
		// Doubled separators normalise
		{"foo--bar", "foo-bar"},
		{"foo__bar", "foo-bar"},
	}
	for _, tt := range tests {
		got := ToKebabCase(tt.input)
		if got != tt.want {
			t.Errorf("ToKebabCase(%q) = %q, want %q", tt.input, got, tt.want)
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