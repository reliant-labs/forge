package cli

import (
	"os"
	"strings"
	"testing"
)

// TestPrintBufLintExceptHint_DetectsCommonRules asserts the migration
// hint surfaces ONLY for the four buf STANDARD rules legacy projects
// commonly trip and that the printed YAML matches the buf.yaml
// `lint.except` snippet the user is expected to paste.
//
// FRICTION 2026-06-02: cp-forge proto port hit four rules on first
// `forge generate`; each one required a manual buf docs lookup.
func TestPrintBufLintExceptHint_DetectsCommonRules(t *testing.T) {
	cases := []struct {
		name      string
		bufOutput string
		want      []string // substrings that MUST be present in the printed hint
		wantNot   []string // substrings that MUST NOT be present
	}{
		{
			name: "package version suffix only",
			bufOutput: "proto/v1/example.proto:1:1: " +
				"PACKAGE_VERSION_SUFFIX: Package name \"example\" should be suffixed.",
			want: []string{
				"Migration hint",
				"PACKAGE_VERSION_SUFFIX",
				"except:",
			},
			wantNot: []string{
				"RPC_REQUEST_STANDARD_NAME",
				"RPC_RESPONSE_STANDARD_NAME",
				"RPC_REQUEST_RESPONSE_UNIQUE",
			},
		},
		{
			name: "all four rules",
			bufOutput: "PACKAGE_VERSION_SUFFIX\n" +
				"RPC_REQUEST_STANDARD_NAME\n" +
				"RPC_RESPONSE_STANDARD_NAME\n" +
				"RPC_REQUEST_RESPONSE_UNIQUE\n",
			want: []string{
				"PACKAGE_VERSION_SUFFIX",
				"RPC_REQUEST_STANDARD_NAME",
				"RPC_RESPONSE_STANDARD_NAME",
				"RPC_REQUEST_RESPONSE_UNIQUE",
			},
		},
		{
			name:      "non-matching rule (no hint)",
			bufOutput: "FIELD_LOWER_SNAKE_CASE: not in the migration set",
			want:      nil, // empty buf output → no hint at all
			wantNot:   []string{"Migration hint"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := captureStderr(t)
			printBufLintExceptHint(tc.bufOutput)
			restore()
			got := buf.String()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("hint missing %q\n--- captured stderr ---\n%s", want, got)
				}
			}
			for _, dont := range tc.wantNot {
				if strings.Contains(got, dont) {
					t.Errorf("hint unexpectedly contains %q\n--- captured stderr ---\n%s", dont, got)
				}
			}
		})
	}
}

// captureStderr redirects os.Stderr to a pipe and returns a Builder
// the caller queries after invoking restore(). Shared with other CLI
// tests that need to assert on warning/hint text printed to stderr.
func captureStderr(t *testing.T) (*strings.Builder, func()) {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	buf := &strings.Builder{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		b := make([]byte, 4096)
		for {
			n, rerr := r.Read(b)
			if n > 0 {
				buf.Write(b[:n])
			}
			if rerr != nil {
				return
			}
		}
	}()
	return buf, func() {
		_ = w.Close()
		<-done
		os.Stderr = orig
	}
}
