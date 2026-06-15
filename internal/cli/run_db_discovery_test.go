package cli

import "testing"

// J-round fix 3 (dev-loop wiring): the compose template publishes a
// fixed postgres host port and `forge run` injects a discovered
// DATABASE_URL so `forge new && docker compose up && forge run` reaches
// postgres with zero hand-wiring. These tests pin the parsing half.

func TestComposePortToDatabaseURL(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"fixed default port", "127.0.0.1:5432\n", "postgres://postgres:postgres@localhost:5432/bookmarks?sslmode=disable"},
		{"random mapping", "0.0.0.0:49213\n", "postgres://postgres:postgres@localhost:49213/bookmarks?sslmode=disable"},
		{"dual stack takes first", "0.0.0.0:5433\n[::]:5433\n", "postgres://postgres:postgres@localhost:5433/bookmarks?sslmode=disable"},
		{"empty output", "", ""},
		{"garbage", "no port here", ""},
		{"port out of range", "0.0.0.0:99999", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := composePortToDatabaseURL(tc.out, "postgres", "postgres", "bookmarks")
			if got != tc.want {
				t.Errorf("composePortToDatabaseURL(%q) = %q, want %q", tc.out, got, tc.want)
			}
		})
	}
}

func TestRedactDSNPassword(t *testing.T) {
	got := redactDSNPassword("postgres://u:secret@localhost:5432/db")
	if got != "postgres://u:***@localhost:5432/db" {
		t.Errorf("redactDSNPassword = %q", got)
	}
	if redactDSNPassword("plainstring") != "plainstring" {
		t.Error("non-DSN input should pass through")
	}
}
