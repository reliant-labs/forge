package orm

import "testing"

func TestPostgresQuoteIdentifier(t *testing.T) {
	d := &PostgresDialect{}
	tests := []struct {
		input    string
		expected string
	}{
		{"users", `"users"`},
		{`col"name`, `"col""name"`},
		{"", `""`},
	}
	for _, tt := range tests {
		got := d.QuoteIdentifier(tt.input)
		if got != tt.expected {
			t.Errorf("QuoteIdentifier(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}
