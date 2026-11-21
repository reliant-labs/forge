package cluster

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeFlakyKubectl puts a kubectl on PATH that fails its first `fails` runs
// with stderr `failure`, then prints `ok` to stdout. It returns a func that
// counts the runs so far.
func fakeFlakyKubectl(t *testing.T, fails int, failure string) func() int {
	t.Helper()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	script := "#!/bin/sh\n" +
		"n=$(cat " + count + " 2>/dev/null || echo 0)\n" +
		"n=$((n+1)); echo $n > " + count + "\n" +
		"if [ $n -le " + strconv.Itoa(fails) + " ]; then echo '" + failure + "' >&2; exit 1; fi\n" +
		"printf '%s\\n' tls.crt tls.key\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() int {
		b, _ := os.ReadFile(count)
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		return n
	}
}

func shortKubectlReadRetry(t *testing.T) {
	t.Helper()
	prev := kubectlReadRetryDelay
	kubectlReadRetryDelay = time.Millisecond
	t.Cleanup(func() { kubectlReadRetryDelay = prev })
}

// TestPreflightSecretReadRetriesTransportFailure pins obs 449eddec: one TLS
// handshake timeout reading observability/otel-agent-token failed a whole prod
// promotion at preflight, though the context answered a minute later.
func TestPreflightSecretReadRetriesTransportFailure(t *testing.T) {
	shortKubectlReadRetry(t)
	runs := fakeFlakyKubectl(t, 1, "Unable to connect to the server: net/http: TLS handshake timeout")
	keys, exists, err := KubectlSecretGetter{}.GetSecretKeys(context.Background(), "prod", "observability", "otel-agent-token")
	if err != nil || !exists {
		t.Fatalf("GetSecretKeys = exists %v, err %v; want the retried read to succeed", exists, err)
	}
	if _, ok := keys["tls.crt"]; !ok || runs() != 2 {
		t.Fatalf("keys %v after %d runs; want tls.crt after 2", keys, runs())
	}
}

func TestPreflightSecretReadGivesUpAfterBoundedRetries(t *testing.T) {
	shortKubectlReadRetry(t)
	runs := fakeFlakyKubectl(t, 99, "Unable to connect to the server: net/http: TLS handshake timeout")
	_, _, err := KubectlSecretGetter{}.GetSecretKeys(context.Background(), "prod", "observability", "otel-agent-token")
	if err == nil || !strings.Contains(err.Error(), "TLS handshake timeout") {
		t.Fatalf("err = %v; want the transport failure after the last attempt", err)
	}
	if runs() != kubectlReadAttempts {
		t.Fatalf("%d runs; want %d", runs(), kubectlReadAttempts)
	}
}

// An answer from the API server is never retried: NotFound is "missing", and
// a refusal is the verdict.
func TestPreflightSecretReadDoesNotRetryServerAnswers(t *testing.T) {
	shortKubectlReadRetry(t)
	for name, tc := range map[string]struct {
		failure    string
		wantExists bool
		wantErr    bool
	}{
		"not found": {failure: `Error from server (NotFound): secrets "otel-agent-token" not found`},
		"forbidden": {failure: `Error from server (Forbidden): secrets "otel-agent-token" is forbidden`, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			runs := fakeFlakyKubectl(t, 99, tc.failure)
			_, exists, err := KubectlSecretGetter{}.GetSecretKeys(context.Background(), "prod", "observability", "otel-agent-token")
			if exists != tc.wantExists || (err != nil) != tc.wantErr {
				t.Fatalf("exists %v err %v; want exists %v err %v", exists, err, tc.wantExists, tc.wantErr)
			}
			if runs() != 1 {
				t.Fatalf("%d runs; a server answer must not be retried", runs())
			}
		})
	}
}

func TestPreflightCombinedReadsRetryToo(t *testing.T) {
	shortKubectlReadRetry(t)
	runs := fakeFlakyKubectl(t, 1, "read tcp 10.0.0.1:443: i/o timeout")
	if _, err := (KubectlServedKinds{}).ServedKinds(context.Background(), "prod"); err != nil {
		t.Fatalf("ServedKinds: %v", err)
	}
	if runs() != 2 {
		t.Fatalf("%d runs; want 2", runs())
	}
}
