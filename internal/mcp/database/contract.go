// Package database is the MCP-side helper for talking to project databases:
// per-environment connection management, read-only query execution, schema
// introspection, and JSON-fixture seeding.
//
// The package wraps three concerns behind one [Service]: connection pool
// management (Connect / Query / Close), schema introspection (ListTables /
// IntrospectTable / FormatTable), and seeding (LoadFixture / ApplySeed /
// ListFixtures). Tests inject a mock to avoid live PostgreSQL.
//
// Data carriers (Environment, ConnectionConfig, QueryResult, TableInfo,
// ColumnInfo, IndexInfo, SeedData) remain plain types — they are not
// behavior to mock.
package database

import "database/sql"

// Service is the behavioral surface of the MCP database helper.
type Service interface {
	// SetConfig replaces the connection configuration for env. Existing
	// open connections for env are closed.
	SetConfig(env Environment, cfg *ConnectionConfig)

	// GetConnection returns a live *sql.DB for env, lazily opening one
	// against the configured DSN.
	GetConnection(env Environment) (*sql.DB, error)

	// CloseAll drops every open connection.
	CloseAll()

	// ExecuteQuery runs a SELECT/WITH query with a row limit and returns
	// columns + rows. Multi-statement and write queries are rejected.
	ExecuteQuery(env Environment, query string, limit int) (*QueryResult, error)

	// ListTables returns all base table names in the public schema.
	ListTables(env Environment) ([]string, error)

	// IntrospectTable returns columns + indexes for tableName.
	IntrospectTable(env Environment, tableName string) (*TableInfo, error)

	// LoadFixture parses fixturesDir/name.json into a SeedData document.
	LoadFixture(fixturesDir, name string) (*SeedData, error)

	// ApplySeed inserts the seed rows against env, optionally clearing
	// the target tables first.
	ApplySeed(env Environment, seed *SeedData, clearFirst bool) error

	// ListFixtures returns the set of fixture names available under
	// fixturesDir (".json" suffix stripped).
	ListFixtures(fixturesDir string) ([]string, error)
}

// Deps is the dependency set for the MCP database Service. Empty today —
// the underlying ConnectionManager owns its pool.
type Deps struct{}

// New constructs an MCP database.Service backed by the process-wide
// connection manager.
func New(_ Deps) Service { return &svc{mgr: getConnectionManager()} }

type svc struct {
	mgr *connectionManager
}

func (s *svc) SetConfig(env Environment, cfg *ConnectionConfig) { s.mgr.setConfig(env, cfg) }

func (s *svc) GetConnection(env Environment) (*sql.DB, error) {
	return s.mgr.getConnection(env)
}

func (s *svc) CloseAll() { s.mgr.closeAll() }

func (s *svc) ExecuteQuery(env Environment, query string, limit int) (*QueryResult, error) {
	return s.mgr.executeQuery(env, query, limit)
}

func (s *svc) ListTables(env Environment) ([]string, error) {
	db, err := s.mgr.getConnection(env)
	if err != nil {
		return nil, err
	}
	return newSchemaIntrospector(db).listTables()
}

func (s *svc) IntrospectTable(env Environment, tableName string) (*TableInfo, error) {
	db, err := s.mgr.getConnection(env)
	if err != nil {
		return nil, err
	}
	return newSchemaIntrospector(db).introspectTable(tableName)
}

func (s *svc) LoadFixture(fixturesDir, name string) (*SeedData, error) {
	return newSeedManager(fixturesDir).loadFixture(name)
}

func (s *svc) ApplySeed(env Environment, seed *SeedData, clearFirst bool) error {
	db, err := s.mgr.getConnection(env)
	if err != nil {
		return err
	}
	return newSeedManager("").applySeed(db, seed, clearFirst)
}

func (s *svc) ListFixtures(fixturesDir string) ([]string, error) {
	return newSeedManager(fixturesDir).listFixtures()
}
