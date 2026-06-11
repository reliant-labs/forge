package orm

import (
	"testing"
)

func TestValidateOrderBy(t *testing.T) {
	tests := []struct {
		name    string
		clause  string
		wantErr bool
	}{
		// Valid cases
		{name: "empty string", clause: "", wantErr: false},
		{name: "single column", clause: "name", wantErr: false},
		{name: "column ASC", clause: "name ASC", wantErr: false},
		{name: "column DESC", clause: "name DESC", wantErr: false},
		{name: "column asc lowercase", clause: "name asc", wantErr: false},
		{name: "multiple columns", clause: "name ASC, age DESC", wantErr: false},
		{name: "column with underscore", clause: "created_at DESC", wantErr: false},
		{name: "column with digits", clause: "field1 ASC", wantErr: false},

		// Invalid cases
		{name: "SQL injection semicolon", clause: "name; DROP TABLE", wantErr: true},
		{name: "invalid direction", clause: "name ASCENDING", wantErr: true},
		{name: "too many tokens", clause: "name ASC DESC", wantErr: true},
		{name: "trailing comma empty part", clause: "name,", wantErr: true},
		{name: "special characters", clause: "na-me ASC", wantErr: true},
		{name: "parentheses", clause: "name()", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOrderBy(tt.clause, nil)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateOrderBy(%q, nil) error = %v, wantErr %v", tt.clause, err, tt.wantErr)
			}
		})
	}
}

func TestValidateOrderBy_Allowlist(t *testing.T) {
	columns := []string{"id", "name", "created_at"}
	tests := []struct {
		name    string
		clause  string
		allowed []string
		wantErr bool
	}{
		{name: "declared column", clause: "name ASC", allowed: columns, wantErr: false},
		{name: "multiple declared columns", clause: "name ASC, created_at DESC", allowed: columns, wantErr: false},
		{name: "case-insensitive match", clause: "NAME desc", allowed: columns, wantErr: false},
		{name: "allowlist declared upper-case", clause: "name", allowed: []string{"NAME"}, wantErr: false},
		{name: "empty clause with allowlist", clause: "", allowed: columns, wantErr: false},
		{name: "undeclared column rejected", clause: "password_hash ASC", allowed: columns, wantErr: true},
		{name: "one undeclared among declared", clause: "name ASC, secret DESC", allowed: columns, wantErr: true},
		{name: "nil allowlist is shape-only", clause: "password_hash ASC", allowed: nil, wantErr: false},
		{name: "empty allowlist is shape-only", clause: "password_hash ASC", allowed: []string{}, wantErr: false},
		{name: "shape still enforced with allowlist", clause: "name; DROP TABLE", allowed: columns, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateOrderBy(tt.clause, tt.allowed)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateOrderBy(%q, %v) error = %v, wantErr %v", tt.clause, tt.allowed, err, tt.wantErr)
			}
		})
	}
}
