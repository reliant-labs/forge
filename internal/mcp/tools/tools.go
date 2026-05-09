package tools

import (
	"encoding/json"
	"fmt"
)

// Tool represents an MCP tool definition
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// toolRegistry maps tool names to their executor functions.
var toolRegistry = map[string]func(json.RawMessage) (string, error){
	// Taskfile
	"taskfile": executeTaskfile,
	// Database tools
	"query_db":          executeQueryDB,
	"migrate_db":        executeMigrateDB,
	"seed_db":           executeSeedDB,
	"introspect_schema": executeIntrospectSchema,
	// Context tools
	"list_protos":          executeListProtos,
	"list_services":        executeListServices,
	"get_service_contract": executeGetServiceContract,
	"get_project_config":   executeGetProjectConfig,
}

// allTools returns all registered MCP tool definitions
func allTools() []Tool {
	return []Tool{
		// Taskfile
		getTaskfileTool(),
		// Database tools
		getQueryDatabaseTool(),
		getMigrateDatabaseTool(),
		getSeedDatabaseTool(),
		getIntrospectSchemaTool(),
		// Context tools
		getListProtosTool(),
		getListServicesTool(),
		getGetServiceContractTool(),
		getGetProjectConfigTool(),
	}
}

// executeTool routes tool calls to their implementations
func executeTool(name string, arguments json.RawMessage) (string, error) {
	fn, ok := toolRegistry[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return fn(arguments)
}
