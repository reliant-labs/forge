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
		// Continuous rules about the USER's own code. config-deps,
		// frontend-stores and optional-deps-guard were all hidden as
		// "audits" and were consequently undiscoverable: on the flagship app
		// config-deps alone reported 15 real findings with paste-ready
		// remediation, while a freshly scaffolded project comes up clean on
		// all three — advisory, but not noisy.
		"column-markers",
		// A continuous rule about the user's own protos + handlers: a
		// forge:computed field nothing populates takes the column default,
		// which for the money columns this marker mostly lands on ships as
		// $0.00 with no constraint violated, no test failing, and no log
		// line. Visible because a human reading a screen is otherwise the
		// only detector.
		"computed-fields",
		"config-deps",
		// A continuous rule about the user's own config protos: with
		// per-binary configs a whole config message can be bound to a
		// binary that does not exist, and nothing else in forge reports
		// it — the fields simply generate and are never loaded.
		"config-reach",
		"contract",
		"conventions",
		// A continuous rule about the user's own project, and the only
		// place this failure is legible at all: a foreign key added after
		// handlers_crud_test.go was scaffolded strands the seed literals
		// in a file forge deliberately never regenerates, and the only
		// other signal is a pq constraint error in test setup — one per
		// affected package, reading like a broken harness.
		// A continuous rule about the user's own protos, and the only place
		// this failure is legible: Create<Entity>Request is the one envelope
		// that FLATTENS the entity, so it re-declares each field's
		// `optional` label, and a label lost there collapses absent and
		// empty. The write lands as "" in a nullable column and surfaces as
		// a postgres foreign-key violation naming a constraint, never the
		// proto line that caused it.
		"create-nullability",
		"crud-fixtures",
		"fix",
		"frontend-stores",
		"generated-drift",
		"help-dev",
		"json",
		"migration-safety",
		"no-fix",
		"optional-deps-guard",
		// A continuous rule about the user's own protos, and the only place
		// a misspelled forge:* marker surfaces at all — visible for the same
		// reason column-markers is.
		"proto-markers",
		// The OPTION-field twin of proto-markers, and the rule that would
		// have caught 104 inert authz annotations in the flagship app.
		// Visible for the same reason: it is a continuous rule about the
		// user's own protos, and the only place the failure surfaces.
		"proto-options",
		// User surface, not maintainer: it is the answer to "how do I lint
		// the backend without paying for the Node toolchain", and it mirrors
		// the frontend-skipping vocabulary `forge build` already uses.
		"skip-frontends",
		"strict",
		"tests",
		// Gating, and about the user's project: forge COPIES these protos
		// in and then never tracks them, so a stale vendored forge.proto
		// is invisible to every other command. Hiding the one check that
		// sees it would recreate the silence.
		"vendored-protos",
	})

	// What stays hidden: meaningless outside the forge repo, bundled into
	// a visible flag, or a one-shot you run at setup and never again.
	assertStringSlicesEqual(t, "lint hidden flags", hiddenFlagNames(cmd), []string{
		"banners",
		"check-workarounds",
		"exported-vars",
		"scaffolds",
		"suggest-buf-excepts",
		"suggest-excludes",
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
	args := []string{"--banners", "--check-workarounds", "--config-deps"}
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatalf("Parse(%v) error = %v (hidden flags must remain functional)", args, err)
	}
	for name, want := range map[string]string{"banners": "true", "check-workarounds": "true", "config-deps": "true"} {
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
