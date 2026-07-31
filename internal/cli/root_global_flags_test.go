// File: internal/cli/root_global_flags_test.go
//
// A global flag is a promise made to EVERY command. `--verbose`/`-v` was
// registered on the root as a PersistentFlag bound to a function-local that
// nothing ever read, so `forge lint -v`, `forge build -v`, `forge db seed
// apply -v` all parsed cleanly and exited 0 having changed nothing — and it
// shadowed the one command that does consume a verbose flag, so
// `forge generate -v` produced ordinary output while `forge generate
// --verbose` produced nine extra lines.

package cli

import (
	"sort"
	"testing"

	"github.com/spf13/pflag"
)

// TestRootPersistentFlagsArePinned: a new global flag must be a conscious
// decision, because it lands on every command in the tree whether or not
// that command can honor it.
func TestRootPersistentFlagsArePinned(t *testing.T) {
	root := NewRootCmd()

	var got []string
	root.PersistentFlags().VisitAll(func(f *pflag.Flag) { got = append(got, f.Name) })
	sort.Strings(got)

	want := []string{"silence-experimental"}
	if len(got) != len(want) {
		t.Fatalf("root persistent flags = %v, want %v\n"+
			"A global flag must be honored by EVERY command. If the new flag only "+
			"applies to some, register it on those commands instead.", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("root persistent flag[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestGenerateVerboseShorthandReachesItsConsumer: -v must bind to the flag
// `forge generate` actually reads, not to an inherited global that swallows
// it.
func TestGenerateVerboseShorthandReachesItsConsumer(t *testing.T) {
	root := NewRootCmd()
	gen, _, err := root.Find([]string{"generate"})
	if err != nil {
		t.Fatalf("`forge generate` does not resolve: %v", err)
	}

	f := gen.Flags().Lookup("verbose")
	if f == nil {
		t.Fatal("`forge generate` has no --verbose flag")
	}
	if f.Shorthand != "v" {
		t.Errorf("`forge generate --verbose` shorthand = %q, want \"v\" — "+
			"without it, -v falls through to whatever the root declares", f.Shorthand)
	}

	if err := gen.Flags().Parse([]string{"-v"}); err != nil {
		t.Fatalf("`forge generate -v` failed to parse: %v", err)
	}
	if gen.Flags().Lookup("verbose").Value.String() != "true" {
		t.Error("`forge generate -v` parsed but did not set --verbose")
	}
}
