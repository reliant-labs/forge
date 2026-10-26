package redact

import (
	"strings"
	"testing"
)

func TestSecrets(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"url userinfo", "dial postgres://app:hunter2@db:5432/x", "dial postgres://[redacted]@db:5432/x"},
		{"key=value", "PASSWORD=hunter2 next", "PASSWORD=[redacted] next"},
		{"json key", `{"api_key": "abc123"}`, `{"api_key": "[redacted]"}`},
		{"authorization header", "Authorization: Bearer abc.def", "Authorization: [redacted]"},
		{"bare bearer", "sent bearer abcdef123", "sent bearer [redacted]"},
		{"jwt", "tok eyJhbGciOi.eyJzdWIiOi.c2lnbmF0dXJl end", "tok [redacted] end"},
		{"anthropic key", "key sk-ant-api03-abcdefghij", "key [redacted]"},
		{"github token", "ghp_abcdefghijklmnop", "[redacted]"},
		{"aws key", "AKIAABCDEFGHIJKLMNOP", "[redacted]"},
		{"long opaque run", strings.Repeat("a", 40), "[redacted]"},
		{"uuid kept", "run 123e4567-e89b-12d3-a456-426614174000", "run 123e4567-e89b-12d3-a456-426614174000"},
		{"path kept", "internal/cli/report_bug.go:12", "internal/cli/report_bug.go:12"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Secrets(tc.in); got != tc.want {
				t.Errorf("Secrets(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestHomeDirs(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"open /home/alice/proj/forge.yaml: denied", "open ~/proj/forge.yaml: denied"},
		{"/Users/bob/src", "~/src"},
		{`C:\Users\carol\src\x.go`, `~\src\x.go`},
		{"/srv/app/main.go", "/srv/app/main.go"},
	} {
		if got := HomeDirs(tc.in); got != tc.want {
			t.Errorf("HomeDirs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBounded(t *testing.T) {
	if got := Bounded("héllo wörld", 5); got != "héllo"+TruncatedMarker {
		t.Errorf("Bounded cut = %q", got)
	}
	if got := Bounded("short", 10); got != "short" {
		t.Errorf("Bounded under the bound = %q", got)
	}
	if got := Bounded("token=abc /home/al/x", 0); got != "token=[redacted] ~/x" {
		t.Errorf("Bounded unbounded = %q", got)
	}
	if got := Bounded("bad \xff byte", 0); got != "bad \uFFFD byte" {
		t.Errorf("Bounded invalid utf-8 = %q", got)
	}
}
