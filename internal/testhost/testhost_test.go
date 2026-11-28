package testhost

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/pgtest"
)

func TestModuleDownloadFailedMatchesOnlyTransportFailures(t *testing.T) {
	for out, want := range map[string]bool{
		// forge#2479, verbatim: an HTTP/2 stream reset from the proxy.
		`github.com/reliant-labs/forge/pkg/contractkit: github.com/reliant-labs/forge/pkg@v0.1.15: read "https://proxy.golang.org/github.com/reliant-labs/forge/pkg/@v/v0.1.15.mod": stream error: stream ID 25; INTERNAL_ERROR; received from peer`: true,
		`connectrpc.com/connect@v1.19.0: Get "https://proxy.golang.org/connectrpc.com/connect/@v/v1.19.0.mod": net/http: TLS handshake timeout`:                                                                                                      true,
		`example.com/dep@v1.0.0: reading https://proxy.golang.org/example.com/dep/@v/v1.0.0.mod: 502 Bad Gateway`:                                                                                                                                    true,
		`example.com/dep@v1.0.0: reading https://proxy.golang.org/example.com/dep/@v/v1.0.0.mod: 404 Not Found`:                                                                                                                                      false,
		`internal/pipeline/mock_gen.go:12:2: undefined: db.Estimate`:                                                                                                                                                                                 false,
		`missing go.sum entry for module providing package example.com/dep`:                                                                                                                                                                          false,
	} {
		if got := ModuleDownloadFailed([]byte(out)); got != want {
			t.Errorf("ModuleDownloadFailed(%q) = %v, want %v", out, got, want)
		}
	}
}

func TestPostgresDownloadFailedMatchesOnlyTransportFailures(t *testing.T) {
	wrap := func(fe *pgtest.FetchError) error {
		return fmt.Errorf("pgtest: start embedded postgres 18.3.0: %w", fe)
	}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"no answer":           {wrap(&pgtest.FetchError{URL: "https://repo1.maven.org/x.jar", Attempts: 4, Err: errors.New("dial tcp: i/o timeout")}), true},
		"5xx":                 {wrap(&pgtest.FetchError{URL: "https://repo1.maven.org/x.jar", Status: 503, Attempts: 4}), true},
		"body broke off":      {wrap(&pgtest.FetchError{URL: "https://repo1.maven.org/x.jar", Status: 200, Attempts: 4, Err: io.ErrUnexpectedEOF}), true},
		"404 is an answer":    {wrap(&pgtest.FetchError{URL: "https://repo1.maven.org/x.jar", Status: 404, Attempts: 1}), false},
		"checksum mismatch":   {wrap(&pgtest.FetchError{URL: "https://repo1.maven.org/x.jar", Status: 200, Attempts: 1, Err: &pgtest.ChecksumError{}}), false},
		"not a download":      {errors.New("pgtest: connect: connection refused"), false},
		"nil is not a reason": {nil, false},
	} {
		if got := PostgresDownloadFailed(tc.err); got != tc.want {
			t.Errorf("%s: PostgresDownloadFailed(%v) = %v, want %v", name, tc.err, got, tc.want)
		}
	}
}

// recorder is a testing.TB that records Skipf instead of unwinding the
// goroutine, so a test can observe whether a Skip* helper skipped.
type recorder struct {
	testing.TB
	skipped string
}

func (r *recorder) Helper() {}

func (r *recorder) Skipf(format string, args ...any) {
	r.skipped = fmt.Sprintf(format, args...)
}

func TestSkipIfModuleDownloadFailed(t *testing.T) {
	for name, tc := range map[string]struct {
		out      string
		wantSkip bool
	}{
		"proxy unreachable":       {`Get "https://proxy.golang.org/x/@v/v1.mod": net/http: TLS handshake timeout`, true},
		"proxy 5xx":               {`reading https://proxy.golang.org/x/@v/v1.mod: 503 Service Unavailable`, true},
		"404 stays a failure":     {`reading https://proxy.golang.org/x/@v/v1.mod: 404 Not Found`, false},
		"compile error":           {`./main.go:3:2: undefined: foo`, false},
		"no output":               {``, false},
		"download inside a build": {"go: downloading x\nx@v1: read \"https://proxy.golang.org/x/@v/v1.zip\": stream error: INTERNAL_ERROR", true},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			SkipIfModuleDownloadFailed(r, []byte(tc.out))
			if got := r.skipped != ""; got != tc.wantSkip {
				t.Fatalf("skipped = %v (%q), want %v", got, r.skipped, tc.wantSkip)
			}
			if tc.wantSkip && !strings.Contains(r.skipped, "https://") {
				t.Errorf("skip reason %q does not name the failed request", r.skipped)
			}
		})
	}
}

func TestSkipIfPostgresDownloadFailed(t *testing.T) {
	fe := func(status int, err error) error {
		return fmt.Errorf("pgtest: %w", &pgtest.FetchError{URL: "https://repo1.maven.org/x.jar", Status: status, Err: err})
	}
	for name, tc := range map[string]struct {
		err      error
		wantSkip bool
	}{
		"no answer":        {fe(0, errors.New("dial tcp: i/o timeout")), true},
		"5xx":              {fe(502, nil), true},
		"404 is an answer": {fe(404, nil), false},
		"unrelated error":  {errors.New("initdb failed"), false},
		"nil":              {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := &recorder{TB: t}
			SkipIfPostgresDownloadFailed(r, tc.err)
			if got := r.skipped != ""; got != tc.wantSkip {
				t.Fatalf("skipped = %v (%q), want %v", got, r.skipped, tc.wantSkip)
			}
			if tc.wantSkip && !strings.Contains(r.skipped, "FORGE_TEST_POSTGRES_URL") {
				t.Errorf("skip reason %q does not name the escape hatch", r.skipped)
			}
		})
	}
}
