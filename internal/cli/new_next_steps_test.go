// File: internal/cli/new_next_steps_test.go
//
// The paste-ability contract for the `forge project new` next-steps block
// (newNextSteps in new.go): every forge command it prints must be a command
// that RESOLVES in the assembled command tree, with an argument list the
// target command accepts. The block is the first thing a new user reads, so a
// line that names a command the binary no longer has (the `forge project add
// entity` → `forge scaffold entity` rename is exactly that class of defect)
// is a broken first ten minutes.
//
// The check is deliberately not a regex over the strings: it resolves the
// tokens through cobra, so it fails the moment the command surface moves
// underneath the text.

package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// pasteableInvocations extracts every forge command a next-steps line offers
// for pasting: each run of tokens starting at the forge invocation prefix
// (Name(), which is "forge" standalone and "<host> forge" when embedded) and
// running to the first token that carries sentence punctuation. Backticks are
// treated as whitespace so an inline `forge run` reads as a command.
//
// The returned slices are the tokens AFTER the invocation prefix — i.e. what
// the root command would receive as os.Args[1:].
func pasteableInvocations(line, name string) [][]string {
	fields := strings.Fields(strings.ReplaceAll(line, "`", " "))
	prefix := strings.Fields(name)
	var out [][]string
	for i := 0; i+len(prefix) <= len(fields); i++ {
		match := true
		for j, want := range prefix {
			if fields[i+j] != want {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		var args []string
		for _, tok := range fields[i+len(prefix):] {
			trimmed := strings.TrimRight(tok, ".,;")
			if trimmed != "" {
				args = append(args, trimmed)
			}
			if trimmed != tok {
				break // sentence punctuation ends the command
			}
		}
		out = append(out, args)
		i += len(prefix) - 1
	}
	return out
}

// resolvePasteable runs one extracted invocation through the real command
// tree. It returns the resolved command path (e.g. "forge scaffold entity")
// so callers can pin WHICH command the block names, not just that it exists.
func resolvePasteable(t *testing.T, args []string) string {
	t.Helper()
	root := NewRootCmd()
	resolved, rest, err := root.Find(args)
	if err != nil {
		t.Errorf("next-steps command %q does not resolve: %v", strings.Join(args, " "), err)
		return ""
	}
	if resolved == root {
		t.Errorf("next-steps command %q matched no subcommand — it would print root usage", strings.Join(args, " "))
		return ""
	}
	if !resolved.Runnable() {
		t.Errorf("next-steps command %q resolves to %q, which is a group with no RunE — pasting it prints usage",
			strings.Join(args, " "), resolved.CommandPath())
		return ""
	}
	// --help is always accepted; skip the arity check for those so a
	// "see --help" pointer isn't judged as a real invocation.
	help := false
	for _, a := range rest {
		if a == "--help" || a == "-h" {
			help = true
		}
	}
	resolved.InitDefaultHelpFlag()
	if err := resolved.ParseFlags(rest); err != nil {
		t.Errorf("next-steps command %q has a flag %q does not accept: %v",
			strings.Join(args, " "), resolved.CommandPath(), err)
		return strings.TrimPrefix(resolved.CommandPath(), root.Name()+" ")
	}
	if !help {
		if err := resolved.ValidateArgs(resolved.Flags().Args()); err != nil {
			t.Errorf("next-steps command %q fails %q's own arg validation: %v",
				strings.Join(args, " "), resolved.CommandPath(), err)
		}
	}
	return strings.TrimPrefix(resolved.CommandPath(), root.Name()+" ")
}

// TestNewNextStepsArePasteable is the contract new.go's printNewNextSteps
// comment claims: every forge command the post-`project new` block prints
// resolves in the assembled tree, accepts the flags shown, and satisfies its
// own Args validator. It also pins that no line carries an unsubstituted
// placeholder — the block promises copy-paste, not fill-in-the-blank.
func TestNewNextStepsArePasteable(t *testing.T) {
	n := Name()

	cases := []struct {
		name     string
		kind     string
		inPlace  bool
		services []string
		// wantCmds are command paths (cobra CommandPath minus the root
		// prefix) the block MUST name. Empty means "no forge commands
		// expected" (the CLI / library skeletons are prose-only).
		wantCmds []string
	}{
		{
			name:     "service kind, no services yet",
			kind:     config.ProjectKindService,
			services: nil,
			wantCmds: []string{"scaffold service", "run"},
		},
		{
			// An entity is declared in the proto, so the block names the
			// bare sweep (`forge scaffold`) rather than a per-entity
			// command with a field list.
			name:     "service kind, one service",
			kind:     config.ProjectKindService,
			services: []string{"catalog"},
			wantCmds: []string{"scaffold", "run", "project annotations"},
		},
		{
			name:     "service kind, two services (the sweep needs --service)",
			kind:     config.ProjectKindService,
			services: []string{"catalog", "orders"},
			wantCmds: []string{"scaffold", "run", "project annotations"},
		},
		{
			name:     "cli kind",
			kind:     config.ProjectKindCLI,
			wantCmds: nil,
		},
		{
			name:     "library kind",
			kind:     config.ProjectKindLibrary,
			wantCmds: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := newNextSteps("demo", tc.inPlace, tc.kind, tc.services)
			if len(lines) == 0 {
				t.Fatal("newNextSteps returned nothing")
			}

			seen := map[string]bool{}
			for _, line := range lines {
				// No fill-in-the-blank: the block substitutes every
				// name before printing.
				for _, ph := range []string{"<", ">", "%s", "%q", "%d", "${"} {
					if strings.Contains(line, ph) {
						t.Errorf("line %q carries the unsubstituted placeholder %q", line, ph)
					}
				}
				for _, args := range pasteableInvocations(line, n) {
					if len(args) == 0 {
						t.Errorf("line %q names %q with no subcommand", line, n)
						continue
					}
					if path := resolvePasteable(t, args); path != "" {
						seen[path] = true
					}
				}
			}

			for _, want := range tc.wantCmds {
				if !seen[want] {
					t.Errorf("next-steps block never names %q; it named %v", want, keysOf(seen))
				}
			}
			if len(tc.wantCmds) == 0 && len(seen) > 0 {
				t.Errorf("expected a prose-only block, but it named forge commands %v", keysOf(seen))
			}
		})
	}
}

// TestNewNextStepsResolveHelper guards the helper itself: a bogus command
// must be reported, so a future rename cannot pass the suite by silently
// extracting nothing.
func TestNewNextStepsResolveHelper(t *testing.T) {
	got := pasteableInvocations("  forge scaffold entity item --from-proto catalog", "forge")
	want := []string{"scaffold", "entity", "item", "--from-proto", "catalog"}
	if len(got) != 1 || strings.Join(got[0], " ") != strings.Join(want, " ") {
		t.Fatalf("pasteableInvocations = %v, want one run %v", got, want)
	}

	// Inline + sentence-terminated forms both parse.
	got = pasteableInvocations("  then rerun `forge run`. Field types: forge scaffold entity --help", "forge")
	if len(got) != 2 {
		t.Fatalf("expected two invocations from a two-command line, got %v", got)
	}
	if strings.Join(got[0], " ") != "run" {
		t.Errorf("first invocation = %v, want [run]", got[0])
	}
	if strings.Join(got[1], " ") != "scaffold entity --help" {
		t.Errorf("second invocation = %v, want [scaffold entity --help]", got[1])
	}

	// A command that does not exist must be caught by the resolver.
	root := NewRootCmd()
	if _, _, err := root.Find([]string{"project", "add", "entity"}); err == nil {
		var found *cobra.Command
		for _, c := range root.Commands() {
			if c.Name() == "project" {
				found = c
			}
		}
		if found != nil {
			for _, c := range found.Commands() {
				if c.Name() == "add" {
					t.Fatal("`forge project add` still exists — the rename to `forge scaffold` is incomplete")
				}
			}
		}
	}
}
