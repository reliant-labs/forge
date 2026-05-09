package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SeedData represents a fixture for seeding the database
type SeedData struct {
	Name        string
	Description string
	Tables      map[string][]map[string]interface{}
}

// seedManager manages database seeding
type seedManager struct {
	fixturesDir string
}

// newSeedManager creates a new seed manager
func newSeedManager(fixturesDir string) *seedManager {
	return &seedManager{
		fixturesDir: fixturesDir,
	}
}

// LoadFixture loads a fixture file
func (sm *seedManager) loadFixture(name string) (*SeedData, error) {
	// Validate fixture name to prevent path traversal
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		return nil, fmt.Errorf("invalid fixture name: %q", name)
	}
	absFixtures, err := filepath.Abs(sm.fixturesDir)
	if err != nil {
		return nil, fmt.Errorf("resolving fixtures dir: %w", err)
	}
	fp := filepath.Join(sm.fixturesDir, name+".json")
	absPath, err := filepath.Abs(fp)
	if err != nil {
		return nil, fmt.Errorf("resolving fixture path: %w", err)
	}
	if !strings.HasPrefix(absPath, absFixtures+string(os.PathSeparator)) {
		return nil, fmt.Errorf("fixture path escapes fixtures directory")
	}

	data, err := os.ReadFile(fp)
	if err != nil {
		return nil, fmt.Errorf("failed to read fixture file: %w", err)
	}

	var seed SeedData
	if err := json.Unmarshal(data, &seed); err != nil {
		return nil, fmt.Errorf("failed to unmarshal fixture: %w", err)
	}

	seed.Name = name
	return &seed, nil
}

// validIdentifier matches safe SQL identifiers (alphanumeric, underscore, and dot for schema-qualified names).
var validIdentifier = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.]*$`)

// quoteIdent quotes a SQL identifier with double quotes per the SQL standard.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// validateTableName ensures a table name is safe to interpolate into SQL.
func validateTableName(name string) error {
	if !validIdentifier.MatchString(name) {
		return fmt.Errorf("invalid table name: %q (must match [a-zA-Z_][a-zA-Z0-9_.]*)", name)
	}
	return nil
}

// ApplySeed applies seed data to the database
func (sm *seedManager) applySeed(db *sql.DB, seed *SeedData, clearFirst bool) error {
	// Validate all table names before starting the transaction
	for table := range seed.Tables {
		if err := validateTableName(table); err != nil {
			return err
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Sort tables for deterministic ordering
	tables := make([]string, 0, len(seed.Tables))
	for table := range seed.Tables {
		tables = append(tables, table)
	}
	sort.Strings(tables)

	// Clear tables in reverse order (child tables first for FK safety)
	if clearFirst {
		for i := len(tables) - 1; i >= 0; i-- {
			table := tables[i]
			if _, err := tx.Exec(fmt.Sprintf("DELETE FROM %s", quoteIdent(table))); err != nil {
				return fmt.Errorf("failed to clear table %s: %w", table, err)
			}
		}
	}

	// Insert data in forward order (parent tables first)
	for _, table := range tables {
		for _, row := range seed.Tables[table] {
			if err := sm.insertRow(tx, table, row); err != nil {
				return fmt.Errorf("failed to insert into %s: %w", table, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	return nil
}

// insertRow inserts a single row into a table
func (sm *seedManager) insertRow(tx *sql.Tx, table string, row map[string]interface{}) error {
	if len(row) == 0 {
		return nil
	}

	// Build column names and placeholders
	var columns []string
	var placeholders []string
	var values []interface{}
	i := 1

	for col, val := range row {
		// Validate column name to prevent SQL injection
		if !validIdentifier.MatchString(col) {
			return fmt.Errorf("invalid column name: %q", col)
		}
		columns = append(columns, quoteIdent(col))
		placeholders = append(placeholders, fmt.Sprintf("$%d", i))
		values = append(values, val)
		i++
	}

	query := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)",
		quoteIdent(table),
		strings.Join(columns, ", "),
		strings.Join(placeholders, ", "))

	_, err := tx.Exec(query, values...)
	return err
}

// ListFixtures returns all available fixtures
func (sm *seedManager) listFixtures() ([]string, error) {
	entries, err := os.ReadDir(sm.fixturesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read fixtures directory: %w", err)
	}

	var fixtures []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			name := entry.Name()[:len(entry.Name())-5] // Remove .json extension
			fixtures = append(fixtures, name)
		}
	}

	return fixtures, nil
}