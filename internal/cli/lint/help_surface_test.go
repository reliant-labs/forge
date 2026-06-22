// Tests for the user-vs-maintainer CLI surface split as it applies to
// `forge lint`. The visible flag set is pinned here on purpose: a new lint
// flag must consciously pick a side (visible user surface vs hidden
// --help-dev surface) or these tests fail. The generate half of the split
// stays in internal/cli/help_surface_test.go.

package lint

import (
	"bytes"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// visibleFlagNames returns the sorted names of all non-hidden local flags
// on cmd — exactly what cobra renders under "Flags:" in --help.
func visibleFlagNames(cmd *cobra.Command) []string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			names = append(names, f.Name)
		}
	})
	sort.Strings(names)
	return names
}

// hiddenFlagNames returns the sorted names of all hidden local flags.
func hiddenFlagNames(cmd *cobra.Command) []string {
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			names = append(names, f.Name)
		}
	})
	sort.Strings(names)
	return names
}

func assertStringSlicesEqual(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s mismatch:\n  got:  %v\n  want: %v\n(new flags must consciously pick a side — see cmdutil.HideDevFlags)", what, got, want)
	}
}

func TestLintHelpSurface(t *testing.T) {
	cmd := newCmd(testFactory())

	assertStringSlicesEqual(t, "lint visible flags", visibleFlagNames(cmd), []string{
		"contract",
		"conventions",
		"fix",
		"help-dev",
		"json",
		"migration-safety",
		"strict",
		"tests",
	})

	assertStringSlicesEqual(t, "lint hidden flags", hiddenFlagNames(cmd), []string{
		"banners",
		"bootstrap-deps-coverage",
		"check-workarounds",
		"config-deps",
		"exported-vars",
		"frontend-packs",
		"frontend-stores",
		"optional-deps-guard",
		"scaffolds",
		"suggest-buf-excepts",
		"suggest-excludes",
		"wire-coverage",
	})

	// Hidden flags must not leak into the rendered help.
	usage := cmd.UsageString()
	for _, name := range hiddenFlagNames(cmd) {
		if strings.Contains(usage, "--"+name) {
			t.Errorf("hidden lint flag --%s leaked into --help usage output", name)
		}
	}
	// The Long text must point at the discoverability mechanism.
	if !strings.Contains(cmd.Long, "--help-dev") {
		t.Error("lint Long help must mention --help-dev so hidden flags stay discoverable")
	}
}

// TestLintHiddenFlagsStillParse proves hiding is help-only: hidden lint
// flags must still parse and set their values exactly as before.
func TestLintHiddenFlagsStillParse(t *testing.T) {
	cmd := newCmd(testFactory())
	args := []string{"--banners", "--wire-coverage", "--config-deps"}
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("Parse(%v) error = %v (hidden flags must remain functional)", args, err)
	}
	for name, want := range map[string]string{"banners": "true", "wire-coverage": "true", "config-deps": "true"} {
		f := cmd.Flags().Lookup(name)
		if f == nil {
			t.Fatalf("flag --%s not registered", name)
		}
		if got := f.Value.String(); got != want {
			t.Errorf("--%s parsed value = %q, want %q", name, got, want)
		}
	}
}

// TestLintHelpDevListsHiddenFlags runs `forge lint --help-dev` end-to-end
// and asserts it exits cleanly, lists every hidden flag, and does NOT run
// the underlying command.
func TestLintHelpDevListsHiddenFlags(t *testing.T) {
	cmd := newCmd(testFactory())
	hidden := hiddenFlagNames(cmd)

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--help-dev"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forge lint --help-dev: unexpected error: %v", err)
	}
	for _, name := range hidden {
		if !strings.Contains(out.String(), "--"+name) {
			t.Errorf("forge lint --help-dev output missing hidden flag --%s\noutput:\n%s", name, out.String())
		}
	}
}
