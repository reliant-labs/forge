package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// An on-demand descriptor refresh streams its progress to stderr, except
// while a caller holds it (`forge lint --quiet`): then a refresh that succeeds
// prints nothing, and one that fails prints everything it held — its progress
// and buf's diagnostics — so the error still has its explanation.
func TestOnDemandDescriptorRefreshIsSilentWhileHeldUnlessItFails(t *testing.T) {
	var fail error
	extract := func(_ string, out, errOut io.Writer) error {
		_, _ = fmt.Fprintln(out, "🔨 Running protoc-gen-forge (mode=descriptor) to extract proto metadata...")
		_, _ = fmt.Fprintln(errOut, "buf: proto/a.proto:3:1: syntax error")
		return fail
	}

	var stderr bytes.Buffer
	if err := refreshDescriptor(t.TempDir(), &stderr, extract); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "Running protoc-gen-forge") {
		t.Errorf("an unheld refresh hid its progress: %q", stderr.String())
	}

	restore := codegen.QuietDescriptorRefresh()
	defer restore()
	stderr.Reset()
	if err := refreshDescriptor(t.TempDir(), &stderr, extract); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Errorf("a held refresh that succeeded printed %q, want nothing", stderr.String())
	}

	fail = errors.New("descriptor generation failed: exit status 100")
	stderr.Reset()
	if err := refreshDescriptor(t.TempDir(), &stderr, extract); !errors.Is(err, fail) {
		t.Fatalf("err = %v, want %v", err, fail)
	}
	for _, want := range []string{"Running protoc-gen-forge", "syntax error"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("a held refresh that failed did not print %q; got %q", want, stderr.String())
		}
	}

	restore()
	restore() // idempotent: a second call must not release another holder
	if codegen.DescriptorRefreshQuiet() {
		t.Error("restore left the refresh held")
	}
}
