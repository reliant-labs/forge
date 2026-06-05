package deploytarget

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// what got written. Used by the dry-run tests to assert the provider
// printed "[DRY-RUN]" lines without actually exec'ing anything.
// fmt.Printf in the providers writes to os.Stdout, so the redirect
// catches it directly.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = orig
	return <-done
}

// fakeRunner is the shared test double for commandRunner. It records
// each call's name+args and returns either a canned error or a canned
// Output blob keyed by the joined argv prefix. Used by external_test
// and compose_test alike — provider tests live in this package so the
// fake is unexported, and pulling it into one of the provider test
// files would create an artificial dependency between sibling test
// files.
type fakeRunner struct {
	calls []string

	// envCalls captures the per-call env overlay so tests can confirm
	// env_file contents were threaded through to the exec'd process.
	// One entry per RunWithEnv call (Run is recorded with nil).
	envCalls []map[string]string

	// runErrs returns canned errors keyed by the joined argv prefix.
	// Empty map = always succeed.
	runErrs map[string]error

	// outputs returns canned combined-output blobs keyed by joined
	// argv prefix. Empty map = empty output.
	outputs map[string]string
}

func (f *fakeRunner) record(name string, args []string) string {
	full := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, full)
	return full
}

func (f *fakeRunner) lookupErr(joined string) error {
	for prefix, err := range f.runErrs {
		if strings.HasPrefix(joined, prefix) {
			return err
		}
	}
	return nil
}

func (f *fakeRunner) lookupOutput(joined string) string {
	for prefix, out := range f.outputs {
		if strings.HasPrefix(joined, prefix) {
			return out
		}
	}
	return ""
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	full := f.record(name, args)
	f.envCalls = append(f.envCalls, nil)
	return f.lookupErr(full)
}

func (f *fakeRunner) RunWithEnv(_ context.Context, env map[string]string, name string, args ...string) error {
	full := f.record(name, args)
	// Copy the map so test mutations on f.envCalls don't pun on the
	// caller's overlay.
	var copyEnv map[string]string
	if env != nil {
		copyEnv = make(map[string]string, len(env))
		for k, v := range env {
			copyEnv[k] = v
		}
	}
	f.envCalls = append(f.envCalls, copyEnv)
	return f.lookupErr(full)
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	full := f.record(name, args)
	if err := f.lookupErr(full); err != nil {
		return nil, err
	}
	return []byte(f.lookupOutput(full)), nil
}
