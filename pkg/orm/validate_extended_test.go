package orm

import (
	"testing"
)

func TestValidateOrderBy_Extended(t *testing.T) {
	tests := []struct {
		name    string
		clause  string
		wantErr bool
	}{
		// Valid cases
		{"numbers only column", "col123", false},
		{"leading underscore", "_hidden ASC", false},
		{"all underscores", "___", false},
		{"mixed case direction", "name Asc", false},
		{"many columns", "a ASC, b DESC, c ASC, d DESC", false},
		{"single char column", "x", false},
		{"digits in middle", "col2name", false},

		// Invalid cases
		{"unicode column", "名前 ASC", true},
		{"emoji", "😀 DESC", true},
		{"dot notation", "users.name ASC", true},
		{"leading space clause", " , name ASC", true},
		{"only comma", ",", true},
		{"multiple commas", "name,,age", true},
		{"backtick", "`name` ASC", true},
		{"double quote", `"name" ASC`, true},
		{"equals sign", "name=1", true},
		{"star", "* ASC", true},
		{"hyphen in column", "first-name", true},
		{"at sign", "@col ASC", true},
		{"hash sign", "#col ASC", true},
		{"space in column", "my column", true}, // "my column" → 3 tokens → invalid
		{"tab character", "name\tASC", false},   // Fields splits on whitespace; should be 2 tokens
		{"newline in clause", "name\nASC", false}, // Fields splits on whitespace
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
