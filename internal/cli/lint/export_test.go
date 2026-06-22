package lint

import (
	"os"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/factory"
)

// testFactory returns a Factory for constructing the lint command in tests.
// newCmd does not consult the Factory (the lint substrate reaches the
// project-store loader package-level), so an empty Factory suffices.
func testFactory() *factory.Factory { return &factory.Factory{} }

// captureStderr redirects os.Stderr to a buffer for the duration of the
// returned restore func. A copy of internal/cli's test_helpers.go helper,
// duplicated here because the lint group is its own package. Calling restore
// is mandatory. The reader goroutine drains the pipe until restore closes
// the writer, so the Builder is safe to read after restore returns.
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
