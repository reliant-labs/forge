package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/reliant-labs/forge/internal/codemod"
)

// generate_template_fixes.go — `forge generate` names the owned files a forge
// template fix has not reached.
//
// A fix to a template whose output the project owns (an internal package's
// observe_chain.go, the command tree's serve.go, an edited Taskfile.yml)
// ships as an Advise codemod (internal/codemod/templatefix.go). `forge
// project upgrade` applies it on the crossing, but a project can move its pin
// past the release without one (--skip codemods, a hand edit), and then
// nothing would ever mention the old shape again. So every generate dry-runs
// the Advise codemods — each reads a handful of named files — and prints one
// note per fix still pending, with the command that applies it. It writes
// nothing and never fails the run.

const adviseTemplateFixesStep = "advise owned-file template fixes"

// adviseTemplateFixesMaxFiles bounds how many files one note lists.
const adviseTemplateFixesMaxFiles = 3

func stepAdviseTemplateFixes(ctx *pipelineContext) error {
	adviseTemplateFixes(os.Stdout, ctx.AbsPath, codemod.All())
	return nil
}

// adviseTemplateFixes prints a note for each Advise codemod in mods that
// would rewrite something under dir. A codemod that errors is skipped: the
// note is advice, and `forge fix <name>` reports the error itself.
func adviseTemplateFixes(w io.Writer, dir string, mods []codemod.Codemod) {
	for _, c := range mods {
		if !c.Advise {
			continue
		}
		rep, err := c.Run(dir, codemod.Options{})
		if err != nil || (!rep.Changed() && len(rep.Next) == 0) {
			continue
		}
		files := reportFiles(rep)
		list := files
		if len(list) > adviseTemplateFixesMaxFiles {
			list = append(append([]string(nil), list[:adviseTemplateFixesMaxFiles]...),
				fmt.Sprintf("%d more", len(files)-adviseTemplateFixesMaxFiles))
		}
		fmt.Fprintf(w, "ℹ️  forge fixed the template %s came from (%s, %s): %s\n",
			ownedFilesPhrase(len(files)), c.Version, c.Name, c.Short)
		fmt.Fprintf(w, "   %s\n", strings.Join(list, ", "))
		if rep.Changed() {
			fmt.Fprintf(w, "   `%s fix %s` applies it (`--dry-run` shows the rewrite); `%s project disown <path>` keeps yours and silences this.\n\n",
				Name(), c.Name, Name())
		} else {
			fmt.Fprintf(w, "   Edited past what `%s fix %s` can rewrite — it prints the edit to make; `%s project disown <path>` keeps yours and silences this.\n\n",
				Name(), c.Name, Name())
		}
	}
}

// reportFiles is the distinct files a codemod report names, in order: each
// Rewrites and Next line leads with "<path>: ".
func reportFiles(rep codemod.Report) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range append(append([]string(nil), rep.Rewrites...), rep.Next...) {
		f, _, ok := strings.Cut(line, ": ")
		if !ok || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func ownedFilesPhrase(n int) string {
	if n == 1 {
		return "this owned file"
	}
	return fmt.Sprintf("these %d owned files", n)
}
