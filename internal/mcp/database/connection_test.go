package database

import "testing"

func TestIsReadOnlyQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  bool
	}{
		// Allowed queries
		{"simple select", "SELECT * FROM users", true},
		{"select with where", "SELECT id, name FROM users WHERE id = 1", true},
		{"select lowercase", "select * from users", true},
		{"select with subquery", "SELECT * FROM (SELECT 1) t", true},
		{"CTE select", "WITH cte AS (SELECT 1) SELECT * FROM cte", true},
		{"select with alias containing keyword substring", "SELECT updated_at FROM users", true},
		{"select column named insert_count", "SELECT insert_count FROM stats", true},
		{"select from table named grants", "SELECT * FROM grants", true},

		// Rejected: multi-statement injection
		{"semicolon injection", "SELECT 1; DROP TABLE users", false},
		{"trailing semicolon", "SELECT 1;", false},

		// Rejected: data-modifying statements
		{"insert", "INSERT INTO users VALUES (1)", false},
		{"update", "UPDATE users SET name = 'x'", false},
		{"delete", "DELETE FROM users", false},
		{"drop table", "DROP TABLE users", false},
		{"alter table", "ALTER TABLE users ADD COLUMN x INT", false},
		{"create table", "CREATE TABLE evil (id INT)", false},
		{"truncate", "TRUNCATE users", false},
		{"copy", "COPY users TO '/tmp/out'", false},
		{"grant", "GRANT ALL ON users TO evil", false},
		{"revoke", "REVOKE ALL ON users FROM evil", false},

		// Rejected: CTE with data modification
		{"CTE with insert", "WITH cte AS (SELECT 1) INSERT INTO users SELECT * FROM cte", false},
		{"CTE with update", "WITH cte AS (SELECT 1) UPDATE users SET name = 'x'", false},
		{"CTE with delete", "WITH cte AS (SELECT 1) DELETE FROM users", false},

		// Rejected: not a select
		{"bare drop", "DROP TABLE users", false},
		{"explain", "EXPLAIN SELECT 1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isReadOnlyQuery(tt.query)
			if got != tt.want {
				t.Errorf("isReadOnlyQuery(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestContainsWord(t *testing.T) {
	tests := []struct {
		s, word string
		want    bool
	}{
		{"DROP TABLE", "DROP", true},
		{"BACKDROP TABLE", "DROP", false},
		{"DROPPED TABLE", "DROP", false},
		{"DO DROP IT", "DROP", true},
		{"X_DROP_Y", "DROP", false},
		{"INSERT INTO", "INSERT", true},
		{"REINSERT INTO", "INSERT", false},
	}
	for _, tt := range tests {
		t.Run(tt.s+"_"+tt.word, func(t *testing.T) {
			got := containsWord(tt.s, tt.word)
			if got != tt.want {
				t.Errorf("containsWord(%q, %q) = %v, want %v", tt.s, tt.word, got, tt.want)
			}
		})
	}
}
