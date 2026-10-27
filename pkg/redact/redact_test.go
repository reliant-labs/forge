package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
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
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"exact", "héllo", 6, "héllo"},
		{"exact multibyte", strings.Repeat("😀", 4), 16, strings.Repeat("😀", 4)},
		{"one over boundary", strings.Repeat("😀", 4) + "!", 16, "😀" + TruncatedMarker},
		{"redaction expansion", "token=x", 15, "tok" + TruncatedMarker},
		{"marker included", strings.Repeat("x ", 20), 16, "x x " + TruncatedMarker},
		{"unicode boundary", strings.Repeat("界", 10), 16, "界" + TruncatedMarker},
		{"emoji boundary", strings.Repeat("😀", 10), 16, "😀" + TruncatedMarker},
		{"small bound", "héllo wörld", 5, " [tru"},
		{"marker only", strings.Repeat("界", 10), len(TruncatedMarker), TruncatedMarker},
		{"one byte", "界界", 1, " "},
		{"invalid input", "bad \xff byte", 10, " [truncate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Bounded(tc.in, tc.max)
			if got != tc.want || len(got) > tc.max || !utf8.ValidString(got) {
				t.Errorf("Bounded = %q (%d bytes), want %q within %d valid UTF-8 bytes", got, len(got), tc.want, tc.max)
			}
		})
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
