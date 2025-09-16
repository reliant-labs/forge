package database

import "testing"

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "users", `"users"`},
		{"reserved word user", "user", `"user"`},
		{"reserved word order", "order", `"order"`},
		{"reserved word group", "group", `"group"`},
		{"schema qualified", "public.users", `"public.users"`},
		{"with internal quote", `my"table`, `"my""table"`},
		{"underscore", "created_at", `"created_at"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := quoteIdent(tt.in)
			if got != tt.want {
				t.Errorf("quoteIdent(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
