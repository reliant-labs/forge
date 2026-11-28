package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// An on-demand descriptor refresh streams its progress to stderr, except
// while a caller holds it (`forge lint --quiet`): then a refresh that succeeds
// prints nothing, and one that fails prints everything it held — its progress
// and buf's diagnostics — so the error still has its explanation.
func TestOnDemandDescriptorRefreshIsSilentWhileHeldUnlessItFails(t *testing.T) {
	var fail error
	quiet := false
	extract := func(_ string, out, errOut io.Writer) error {
		_, _ = fmt.Fprintln(out, "🔨 Running protoc-gen-forge (mode=descriptor) to extract proto metadata...")
		_, _ = fmt.Fprintln(errOut, "buf: proto/a.proto:3:1: syntax error")
		return fail
	}

	var stderr bytes.Buffer
	if err := refreshDescriptor(t.TempDir(), quiet, &stderr, extract); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Running protoc-gen-forge") {
		t.Errorf("an unheld refresh hid its progress: %q", stderr.String())
	}

	quiet = true
	stderr.Reset()
	if err := refreshDescriptor(t.TempDir(), quiet, &stderr, extract); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a held refresh that succeeded printed %q, want nothing", stderr.String())
	}

	fail = errors.New("descriptor generation failed: exit status 100")
	stderr.Reset()
	if err := refreshDescriptor(t.TempDir(), quiet, &stderr, extract); !errors.Is(err, fail) {
		t.Fatalf("err = %v, want %v", err, fail)
	}
	for _, want := range []string{"Running protoc-gen-forge", "syntax error"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("a held refresh that failed did not print %q; got %q", want, stderr.String())
		}
	}

}

func TestConcurrentDescriptorRefreshOutputIsInvocationLocal(t *testing.T) {
	root := t.TempDir()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var quietOut, verboseOut bytes.Buffer
	extract := func(_ string, out, errOut io.Writer) error {
		_, _ = fmt.Fprintln(out, "progress")
		_, _ = fmt.Fprintln(errOut, "diagnostics")
		return nil
	}
	go func() {
		done <- refreshDescriptor(root, true, &quietOut, func(dir string, out, errOut io.Writer) error {
			close(started)
			<-release
			return extract(dir, out, errOut)
		})
	}()
	<-started
	err := refreshDescriptor(root, false, &verboseOut, extract)
	close(release)
	quietErr := <-done
	if err != nil || quietErr != nil {
		t.Fatalf("refresh errors: verbose=%v quiet=%v", err, quietErr)
	}
	if quietOut.Len() != 0 || verboseOut.String() != "progress\ndiagnostics\n" {
		t.Fatalf("quiet=%q verbose=%q", quietOut.String(), verboseOut.String())
	}
}
