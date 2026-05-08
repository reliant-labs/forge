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
			err := ValidateOrderBy(tt.clause)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateOrderBy(%q) error = %v, wantErr %v", tt.clause, err, tt.wantErr)
			}
		})
	}
}
