package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/mcp/database"
)

// GetQueryDatabaseTool returns the query_db tool definition
func GetQueryDatabaseTool() Tool {
	return Tool{
		Name: "query_db",
		Description: `Execute read-only database queries safely.

This tool provides direct database access without Bash. Features:
- Read-only queries (SELECT only)
- Environment-specific connections (dev/staging/prod)
- Result limits for safety
- Formatted output

Safe for:
- Debugging data issues
- Verifying migrations
- Exploring schema
- Checking data integrity`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"dev", "staging", "prod"},
					"default":     "dev",
					"description": "Environment to query",
				},
				"query": map[string]interface{}{
					"type":        "string",
					"description": "SQL query to execute (SELECT only)",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum rows to return",
					"default":     10,
				},
			},
			"required": []string{"query"},
		},
	}
}

func executeQueryDB(arguments json.RawMessage) (string, error) {
	var args struct {
		Env   string `json:"env"`
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}

	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Defaults
	if args.Env == "" {
		args.Env = "dev"
	}
	if args.Limit == 0 {
		args.Limit = 10
	}

	env, err := parseEnv(args.Env)
	if err != nil {
		return "", err
	}

	// Execute query
	manager := database.GetConnectionManager()
	result, err := manager.ExecuteQuery(env, args.Query, args.Limit)
	if err != nil {
		return "", fmt.Errorf("query failed: %w", err)
	}

	// Format results
	output := fmt.Sprintf("Query Results (%s)\n\n", args.Env)
	output += fmt.Sprintf("Query: %s\n\n", args.Query)
	output += fmt.Sprintf("Rows returned: %d", result.RowCount)
	if args.Limit > 0 {
		output += fmt.Sprintf(" (limited to %d)", args.Limit)
	}
	output += "\n\n"

	if result.RowCount == 0 {
		output += "No rows found.\n"
		return output, nil
	}

	// Print column headers
	output += strings.Join(result.Columns, " | ") + "\n"
	output += strings.Repeat("-", len(strings.Join(result.Columns, " | "))) + "\n"

	// Print rows
	for _, row := range result.Rows {
		var rowStrs []string
		for _, val := range row {
			if val == nil {
				rowStrs = append(rowStrs, "NULL")
			} else {
				rowStrs = append(rowStrs, fmt.Sprintf("%v", val))
			}
		}
		output += strings.Join(rowStrs, " | ") + "\n"
	}

	return output, nil
}

// GetMigrateDatabaseTool returns the migrate_db tool definition
func GetMigrateDatabaseTool() Tool {
	return Tool{
		Name: "migrate_db",
		Description: `Validate database connectivity for migrations (does NOT actually run migrations).

This tool only checks that the database is reachable. It does not apply or rollback
migration files. To run real migrations, use the taskfile commands from the CLI:
- task db:migrate:up
- task db:migrate:down

Operations:
- up: Validate connectivity (does not apply migrations)
- down: Validate connectivity (does not rollback migrations)
- status: Show migration status (limited)`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"dev", "staging", "prod"},
					"default":     "dev",
					"description": "Environment to migrate",
				},
				"direction": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"up", "down", "status"},
					"default":     "up",
					"description": "Migration direction",
				},
				"steps": map[string]interface{}{
					"type":        "integer",
					"description": "Number of migrations to apply/rollback",
					"default":     0,
				},
			},
			"required": []string{"env"},
		},
	}
}

func executeMigrateDB(arguments json.RawMessage) (string, error) {
	var args struct {
		Env       string `json:"env"`
		Direction string `json:"direction"`
		Steps     int    `json:"steps"`
	}

	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Defaults
	if args.Env == "" {
		args.Env = "dev"
	}
	if args.Direction == "" {
		args.Direction = "up"
	}

	env, err := parseEnv(args.Env)
	if err != nil {
		return "", err
	}

	manager := database.GetConnectionManager()
	db, err := manager.GetConnection(env)
	if err != nil {
		return "", fmt.Errorf("failed to connect to database: %w", err)
	}

	var result string

	switch args.Direction {
	case "status":
		result = fmt.Sprintf("Migration Status (%s)\n\n", args.Env)
		result += "Checking migration status...\n\n"
		result += "Note: Full migration status requires integration with migration tracking table.\n"
		result += "Current implementation uses go-migrate or similar migration tool.\n"

	case "up":
		if err := db.Ping(); err != nil {
			return "", fmt.Errorf("database not reachable: %w", err)
		}

		result = fmt.Sprintf("Database connectivity check PASSED (%s).\n", args.Env)
		result += "Migration execution is not yet implemented. Use `task db:migrate:up` from the command line.\n"

	case "down":
		if err := db.Ping(); err != nil {
			return "", fmt.Errorf("database not reachable: %w", err)
		}

		result = fmt.Sprintf("Database connectivity check PASSED (%s).\n", args.Env)
		result += "Migration execution is not yet implemented. Use `task db:migrate:down` from the command line.\n"

	default:
		return "", fmt.Errorf("invalid direction: %s", args.Direction)
	}

	return result, nil
}

// GetSeedDatabaseTool returns the seed_db tool definition
func GetSeedDatabaseTool() Tool {
	return Tool{
		Name: "seed_db",
		Description: `Load test data from fixtures into the database.

Fixtures are JSON files defining table data. Useful for:
- Setting up test data
- Populating demo environments
- Creating consistent test scenarios
- Debugging with known data

Fixtures are located in db/fixtures/ directory.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"dev", "staging", "prod"},
					"default":     "dev",
					"description": "Environment to seed",
				},
				"fixture": map[string]interface{}{
					"type":        "string",
					"description": "Fixture name (without .json extension)",
				},
				"clear_first": map[string]interface{}{
					"type":        "boolean",
					"description": "Clear existing data before seeding",
					"default":     false,
				},
			},
			"required": []string{"env", "fixture"},
		},
	}
}

func executeSeedDB(arguments json.RawMessage) (string, error) {
	var args struct {
		Env        string `json:"env"`
		Fixture    string `json:"fixture"`
		ClearFirst bool   `json:"clear_first"`
	}

	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Defaults
	if args.Env == "" {
		args.Env = "dev"
	}

	env, err := parseEnv(args.Env)
	if err != nil {
		return "", err
	}

	// Get database connection
	connManager := database.GetConnectionManager()
	db, err := connManager.GetConnection(env)
	if err != nil {
		return "", fmt.Errorf("failed to connect to database: %w", err)
	}

	// Load fixture
	seedManager := database.NewSeedManager("db/fixtures")
	seed, err := seedManager.LoadFixture(args.Fixture)
	if err != nil {
		return "", fmt.Errorf("failed to load fixture: %w", err)
	}

	// Apply seed
	if err := seedManager.ApplySeed(db, seed, args.ClearFirst); err != nil {
		return "", fmt.Errorf("failed to apply seed: %w", err)
	}

	result := fmt.Sprintf("Database seeded successfully (%s)\n\n", args.Env)
	result += fmt.Sprintf("Fixture: %s\n", args.Fixture)
	if seed.Description != "" {
		result += fmt.Sprintf("Description: %s\n", seed.Description)
	}
	result += fmt.Sprintf("Clear first: %v\n\n", args.ClearFirst)

	result += "Tables seeded:\n"
	for table, rows := range seed.Tables {
		result += fmt.Sprintf("  - %s: %d rows\n", table, len(rows))
	}

	return result, nil
}

// GetIntrospectSchemaTool returns the introspect_schema tool definition
func GetIntrospectSchemaTool() Tool {
	return Tool{
		Name: "introspect_schema",
		Description: `Inspect database schema and compare with proto definitions.

Shows:
- Table structure (columns, types, constraints)
- Indexes and their configuration
- Primary keys and foreign keys
- Comparison with proto entity definitions (when available)

Useful for:
- Understanding database layout
- Verifying migrations
- Finding schema drift
- Debugging ORM issues`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"env": map[string]interface{}{
					"type":        "string",
					"enum":        []string{"dev", "staging", "prod"},
					"default":     "dev",
					"description": "Environment to introspect",
				},
				"table": map[string]interface{}{
					"type":        "string",
					"description": "Specific table to inspect (optional)",
				},
				"compare_proto": map[string]interface{}{
					"type":        "boolean",
					"description": "Compare with proto definitions",
					"default":     false,
				},
			},
			"required": []string{"env"},
		},
	}
}

func executeIntrospectSchema(arguments json.RawMessage) (string, error) {
	var args struct {
		Env          string `json:"env"`
		Table        string `json:"table"`
		CompareProto bool   `json:"compare_proto"`
	}

	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Defaults
	if args.Env == "" {
		args.Env = "dev"
	}

	env, err := parseEnv(args.Env)
	if err != nil {
		return "", err
	}

	// Get database connection
	connManager := database.GetConnectionManager()
	db, err := connManager.GetConnection(env)
	if err != nil {
		return "", fmt.Errorf("failed to connect to database: %w", err)
	}

	introspector := database.NewSchemaIntrospector(db)

	var result string

	if args.Table != "" {
		// Introspect specific table
		result = fmt.Sprintf("Schema Introspection (%s)\n\n", args.Env)

		if args.CompareProto {
			comparison, err := introspector.CompareWithProto(args.Table, "")
			if err != nil {
				return "", fmt.Errorf("failed to compare with proto: %w", err)
			}
			result += comparison
		} else {
			table, err := introspector.IntrospectTable(args.Table)
			if err != nil {
				return "", fmt.Errorf("failed to introspect table: %w", err)
			}
			result += database.FormatTableInfo(table)
		}
	} else {
		// List all tables
		result = fmt.Sprintf("Database Schema (%s)\n\n", args.Env)

		tables, err := introspector.ListTables()
		if err != nil {
			return "", fmt.Errorf("failed to list tables: %w", err)
		}

		result += fmt.Sprintf("Found %d tables:\n\n", len(tables))
		for _, tableName := range tables {
			table, err := introspector.IntrospectTable(tableName)
			if err != nil {
				result += fmt.Sprintf("  %s: %v\n", tableName, err)
				continue
			}

			result += fmt.Sprintf("Table: %s (%d columns, %d indexes)\n",
				tableName, len(table.Columns), len(table.Indexes))

			// Show primary key columns
			var pkCols []string
			for _, col := range table.Columns {
				if col.IsPrimaryKey {
					pkCols = append(pkCols, col.Name)
				}
			}
			if len(pkCols) > 0 {
				result += fmt.Sprintf("  Primary Key: %s\n", strings.Join(pkCols, ", "))
			}

			result += "\n"
		}

		result += "\nUse introspect_schema with table parameter to see detailed schema for a specific table.\n"
	}

	return result, nil
}

// parseEnv parses an environment string into a database.Environment
func parseEnv(s string) (database.Environment, error) {
	switch s {
	case "dev":
		return database.EnvDev, nil
	case "staging":
		return database.EnvStaging, nil
	case "prod":
		return database.EnvProd, nil
	default:
		return "", fmt.Errorf("invalid environment: %s", s)
	}
}