package deploytarget

import (
	"context"
	"strings"
)

// fakeRunner is the shared test double for commandRunner. It records
// each call's name+args and returns either a canned error or a canned
// Output blob keyed by the joined argv prefix. Used by external_test
// and compose_test alike — provider tests live in this package so the
// fake is unexported, and pulling it into one of the provider test
// files would create an artificial dependency between sibling test
// files.
type fakeRunner struct {
	calls []string

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
	return f.lookupErr(full)
}

func (f *fakeRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	full := f.record(name, args)
	if err := f.lookupErr(full); err != nil {
		return nil, err
	}
	return []byte(f.lookupOutput(full)), nil
}
