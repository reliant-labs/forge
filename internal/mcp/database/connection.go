package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver via pgx
)

// Environment represents a deployment environment
type Environment string

const (
	EnvDev     Environment = "dev"
	EnvStaging Environment = "staging"
	EnvProd    Environment = "prod"
)

// ConnectionConfig holds database connection configuration.
// If DSN is set it is used directly, otherwise Host/Port/etc. are used
// to build a connection string.
type ConnectionConfig struct {
	DSN      string // Full connection string (takes precedence)
	Host     string
	Port     int
	Database string
	User     string
	Password string
	SSLMode  string
	MaxConns int
	MaxIdle  int
}

// connectionManager manages database connections per environment
type connectionManager struct {
	mu          sync.Mutex
	connections map[Environment]*sql.DB
	configs     map[Environment]*ConnectionConfig
}

var (
	globalConnMgr *connectionManager
	connOnce      sync.Once
)

// getConnectionManager returns the singleton connection manager
func getConnectionManager() *connectionManager {
	connOnce.Do(func() {
		globalConnMgr = &connectionManager{
			connections: make(map[Environment]*sql.DB),
			configs:     make(map[Environment]*ConnectionConfig),
		}
		// Load default configs
		globalConnMgr.loadDefaultConfigs()
	})
	return globalConnMgr
}

// envVarForDSN maps each environment to its DSN environment variable name.
var envVarForDSN = map[Environment]string{
	EnvDev:     "forge_DB_DSN_DEV",
	EnvStaging: "forge_DB_DSN_STAGING",
	EnvProd:    "forge_DB_DSN_PROD",
}

// loadDefaultConfigs sets up connection configurations from environment
// variables. For each environment it first checks DATABASE_URL (common
// convention), then the environment-specific variable (e.g.
// forge_DB_DSN_DEV). Only the dev environment falls back to hardcoded
// defaults; staging and prod require an environment variable.
func (cm *connectionManager) loadDefaultConfigs() {
	for _, env := range []Environment{EnvDev, EnvStaging, EnvProd} {
		if cfg := configFromEnv(env); cfg != nil {
			cm.configs[env] = cfg
		}
	}

	// Dev-only fallback so local development works out of the box.
	if _, ok := cm.configs[EnvDev]; !ok {
		cm.configs[EnvDev] = &ConnectionConfig{
			Host:     "localhost",
			Port:     5432,
			Database: "forge_dev",
			User:     "postgres",
			Password: "postgres",
			SSLMode:  "disable",
			MaxConns: 10,
			MaxIdle:  5,
		}
	}
}

// configFromEnv builds a ConnectionConfig from environment variables for
// the given environment. It returns nil if no DSN is set.
func configFromEnv(env Environment) *ConnectionConfig {
	// Check generic DATABASE_URL first (common 12-factor convention), but
	// only when a single environment is expected (controlled by the
	// environment-specific var taking precedence).
	dsn := os.Getenv(envVarForDSN[env])
	if dsn == "" && env == EnvDev {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		return nil
	}
	// Store the raw DSN. GetConnection detects this and uses it directly.
	return &ConnectionConfig{
		DSN:      dsn,
		MaxConns: defaultMaxConns(env),
		MaxIdle:  defaultMaxIdle(env),
	}
}

func defaultMaxConns(env Environment) int {
	switch env {
	case EnvProd:
		return 50
	case EnvStaging:
		return 25
	default:
		return 10
	}
}

func defaultMaxIdle(env Environment) int {
	switch env {
	case EnvProd:
		return 20
	case EnvStaging:
		return 10
	default:
		return 5
	}
}

// SetConfig updates the configuration for an environment
func (cm *connectionManager) setConfig(env Environment, config *ConnectionConfig) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cm.configs[env] = config

	// Close existing connection if any
	if conn, exists := cm.connections[env]; exists {
		conn.Close()
		delete(cm.connections, env)
	}
}

// GetConnection returns a database connection for the specified environment
func (cm *connectionManager) getConnection(env Environment) (*sql.DB, error) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if conn, exists := cm.connections[env]; exists {
		if err := conn.Ping(); err == nil {
			return conn, nil
		}
		// Connection is dead, close and remove
		conn.Close()
		delete(cm.connections, env)
	}

	config, exists := cm.configs[env]
	if !exists {
		return nil, fmt.Errorf("no configuration found for environment: %s", env)
	}

	connDSN := config.DSN
	if connDSN == "" {
		connDSN = fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s",
			config.Host, config.Port, config.Database, config.User, config.Password, config.SSLMode)
	}

	db, err := sql.Open("pgx", connDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Configure connection pool
	db.SetMaxOpenConns(config.MaxConns)
	db.SetMaxIdleConns(config.MaxIdle)
	db.SetConnMaxLifetime(time.Hour)

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping database: %w", err)
	}

	cm.connections[env] = db
	return db, nil
}

// CloseAll closes all database connections
func (cm *connectionManager) closeAll() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	for env, conn := range cm.connections {
		conn.Close()
		delete(cm.connections, env)
	}
}

// QueryResult represents the result of a database query
type QueryResult struct {
	Columns  []string
	Rows     [][]interface{}
	RowCount int
}

// ExecuteQuery executes a read-only query
func (cm *connectionManager) executeQuery(env Environment, query string, limit int) (*QueryResult, error) {
	// Validate query is read-only
	if !isReadOnlyQuery(query) {
		return nil, fmt.Errorf("only SELECT queries are allowed")
	}

	db, err := cm.getConnection(env)
	if err != nil {
		return nil, err
	}

	// Apply limit only if the query doesn't already have one.
	if limit > 0 && !strings.Contains(strings.ToUpper(query), "LIMIT") {
		query = fmt.Sprintf("%s LIMIT %d", query, limit)
	}

	// Use a read-only transaction so the database engine enforces
	// read-only semantics, preventing multi-statement injection
	// (e.g. "SELECT 1; DROP TABLE users").
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("failed to begin read-only transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()

	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("failed to get columns: %w", err)
	}

	// Read rows
	var data [][]interface{}
	for rows.Next() {
		// Create a slice of interface{} to hold each column value
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		data = append(data, values)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("row iteration error: %w", err)
	}

	return &QueryResult{
		Columns:  columns,
		Rows:     data,
		RowCount: len(data),
	}, nil
}

// disallowedKeywords are SQL keywords that indicate a data-modifying or
// administrative statement. Used as a defense-in-depth layer on top of the
// DB-level read-only transaction.
var disallowedKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "ALTER", "CREATE",
	"TRUNCATE", "COPY", "GRANT", "REVOKE", "COMMENT", "SECURITY",
	"EXEC", "EXECUTE", "CALL",
}

// isReadOnlyQuery checks if a query is read-only.
// It rejects multi-statement queries (containing semicolons) and any query
// that contains data-modifying keywords. Only SELECT and WITH ... SELECT
// are allowed. The DB-level read-only transaction is the primary protection;
// this function is a defense-in-depth layer.
func isReadOnlyQuery(query string) bool {
	normalized := strings.ToUpper(strings.TrimSpace(query))

	// Reject multi-statement queries: "SELECT 1; DROP TABLE users" must fail.
	if strings.Contains(normalized, ";") {
		return false
	}

	// Must start with SELECT or WITH (CTE).
	if !strings.HasPrefix(normalized, "SELECT") && !strings.HasPrefix(normalized, "WITH") {
		return false
	}

	// Scan for any disallowed keyword as a whole word.
	for _, kw := range disallowedKeywords {
		if containsWord(normalized, kw) {
			return false
		}
	}

	return true
}

// containsWord reports whether s contains word as a standalone SQL keyword
// (not part of a larger identifier). It checks that the character before and
// after the match is not a letter, digit, or underscore.
func containsWord(s, word string) bool {
	start := 0
	for {
		idx := strings.Index(s[start:], word)
		if idx < 0 {
			return false
		}
		absIdx := start + idx
		end := absIdx + len(word)

		// Check character before the match.
		beforeOK := absIdx == 0 || !isIdentChar(s[absIdx-1])
		// Check character after the match.
		afterOK := end == len(s) || !isIdentChar(s[end])

		if beforeOK && afterOK {
			return true
		}
		start = absIdx + 1
	}
}

func isIdentChar(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
}