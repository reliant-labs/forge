package testcapture

import "testing"

func TestScrubberLineRedactsEveryPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"token ghp_abcdefghijklmnopqrstuvwxyz0123", "token [redacted]"},
		{"pat github_pat_11ABCDEFG0123456789_abcdef", "pat [redacted]"},
		{"slack xoxb-1234567890-abcdef", "slack [redacted]"},
		{"aws AKIAABCDEFGHIJKLMNOP done", "aws [redacted] done"},
		{"gcp AIza" + "abcdefghijklmnopqrstuvwxyz012345678", "gcp [redacted]"},
		{"stripe sk_live_abcdefghij12", "stripe [redacted]"},
		{"openai sk-abcdefghijklmnopqrstuvwx", "openai [redacted]"},
		{"jwt eyJhbGciOiJI.eyJzdWIiOiIx.SflKxwRJSMeK", "jwt [redacted-jwt]"},
		{"dial postgres://app:s3cret@db:5432/x", "dial postgres://[redacted]@db:5432/x"},
		// Both the bearer and the authorization patterns fire, as in sed.
		{"Authorization: Bearer abcdefghijkl1234", "Authorization: [redacted] [redacted]"},
		{"curl -H 'bearer abcdefghijkl1234'", "curl -H 'bearer [redacted]'"},
		{`password="hunter22"`, `password="[redacted]"`},
		{"API_KEY: abcd1234", "API_KEY: [redacted]"},
		{"client-secret=abcd,next", "client-secret=[redacted],next"},
		// Not secrets: left exactly as they were.
		{"--- FAIL: TestPassword (0.00s)", "--- FAIL: TestPassword (0.00s)"},
		{"ok  \texample.com/x\t0.01s", "ok  \texample.com/x\t0.01s"},
		{"token: abc", "token: abc"},
	} {
		var s Scrubber
		got, drop := s.Line(tc.in)
		if drop || got != tc.want {
			t.Errorf("Line(%q) = %q, %v; want %q", tc.in, got, drop, tc.want)
		}
	}
}

func TestScrubberReplacesAPrivateKeyBlockWithOneLine(t *testing.T) {
	t.Parallel()
	var s Scrubber
	in := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nIBAAKC\n-----END RSA PRIVATE KEY-----\nafter\n"
	if got, want := s.Text(in), "before\n[redacted private key]\nafter\n"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	// The block spans lines, so the state carries across calls, as it does
	// across the events of one stream.
	var ev Scrubber
	var got string
	for _, line := range []string{"-----BEGIN PRIVATE KEY-----\n", "abc\n", "-----END PRIVATE KEY-----\n", "next\n"} {
		got += ev.Text(line)
	}
	if want := "[redacted private key]\nnext\n"; got != want {
		t.Fatalf("across calls = %q, want %q", got, want)
	}
	// A one-line key ends where it began.
	var one Scrubber
	if got, want := one.Text("k=-----BEGIN EC PRIVATE KEY-----abc-----END EC PRIVATE KEY-----\nnext\n"), "[redacted private key]\nnext\n"; got != want {
		t.Fatalf("one-line key = %q, want %q", got, want)
	}
}

func TestScrubberTextKeepsLineEndings(t *testing.T) {
	t.Parallel()
	var s Scrubber
	for _, in := range []string{"", "a", "a\n", "a\nb", "a\n\nb\n"} {
		if got := s.Text(in); got != in {
			t.Errorf("Text(%q) = %q, want it unchanged", in, got)
		}
	}
}
