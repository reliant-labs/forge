package pack

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/packs"
)

func newListCmd(f *factory.Factory) *cobra.Command {
	var depsFlag bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available packs",
		Long: `List packs available for installation, with installed-status when run
inside a project.

Use --deps to render the pack-to-pack dependency graph: which packs
declare depends_on which other packs. Producers (audit-log, …) appear
as roots; consumers (api-key, …) hang under them. Useful for figuring
out the right install order before running 'forge pack add'.

Examples:
  forge pack list
  forge pack list --deps`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := packsFeatureGate(f); err != nil {
				return err
			}
			if depsFlag {
				return runPackListDeps()
			}
			return runPackList(f)
		},
	}
	cmd.Flags().BoolVar(&depsFlag, "deps", false, "Show the pack-to-pack dependency graph instead of the table")
	return cmd
}

func runPackList(f *factory.Factory) error {
	available, err := packs.ListPacks()
	if err != nil {
		return fmt.Errorf("list packs: %w", err)
	}

	if len(available) == 0 {
		fmt.Println("No packs available.")
		return nil
	}

	// Check which are installed (if we're in a project)
	var installed map[string]bool
	store, cfgErr := f.LoadProjectStore()
	if cfgErr == nil {
		installed = make(map[string]bool)
		for _, name := range store.Packs() {
			installed[name] = true
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tVERSION\tSUBPATH\tSTATUS\tDESCRIPTION")
	for _, p := range available {
		status := ""
		if installed != nil && installed[p.Name] {
			status = "installed"
		}
		// Surface the pack-declared subpath under pkg/ so users can see at a
		// glance what subtree the install touches. Empty subpath = top-level.
		subpath := p.Subpath
		if subpath == "" {
			subpath = "(root)"
		}
		// Truncate description on first newline so the table stays tidy.
		desc := p.Description
		if i := indexByte(desc, '\n'); i >= 0 {
			desc = desc[:i]
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, subpath, status, desc)
	}
	return w.Flush()
}

// runPackListDeps renders the pack-to-pack dependency graph as a
// roots-first tree. Roots are packs no other pack depends on (or that
// declare no `depends_on` themselves and aren't depended on); each node
// indents its dependents one level. Useful as an LLM-friendly reference
// for "what installs which" before `forge pack add`.
func runPackListDeps() error {
	available, err := packs.ListPacks()
	if err != nil {
		return fmt.Errorf("list packs: %w", err)
	}
	if len(available) == 0 {
		fmt.Println("No packs available.")
		return nil
	}

	// Build forward (consumer→producer) and reverse (producer→consumers) maps.
	consumers := map[string][]string{}
	declares := map[string][]string{}
	for _, p := range available {
		declares[p.Name] = append([]string(nil), p.DependsOn...)
		for _, dep := range p.DependsOn {
			consumers[dep] = append(consumers[dep], p.Name)
		}
	}

	// Roots: packs with no producer (no depends_on entries). These are
	// the install-first leaves of the topological order. We render roots
	// alphabetically; consumers indent under each root.
	var roots []string
	for _, p := range available {
		if len(p.DependsOn) == 0 {
			roots = append(roots, p.Name)
		}
	}
	sort.Strings(roots)

	fmt.Println("Pack dependency graph (producers → consumers):")
	fmt.Println()

	// Print each root + its dependents recursively. Use a visited set so
	// a future cycle (forbidden but possible in author error) doesn't
	// loop forever.
	visited := map[string]bool{}
	var walk func(name string, depth int)
	walk = func(name string, depth int) {
		if visited[name] {
			fmt.Printf("%s- %s (cycle detected — already visited)\n",
				strings.Repeat("  ", depth), name)
			return
		}
		visited[name] = true
		marker := "•"
		if depth == 0 {
			marker = "▸"
		}
		fmt.Printf("%s%s %s\n", strings.Repeat("  ", depth), marker, name)
		kids := append([]string(nil), consumers[name]...)
		sort.Strings(kids)
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}

	// Orphan check: any pack neither in roots nor reachable from a root
	// (i.e. part of a cycle, or unreferenced consumer with bad deps).
	for _, p := range available {
		if !visited[p.Name] {
			fmt.Printf("⚠ %s (unreachable — likely cycle or missing producer)\n", p.Name)
		}
	}

	return nil
}
