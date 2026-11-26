package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// newDryRun records --dry-run for the project new command in progress.
var newDryRun bool

// newProjectPlanSteps carries options for post-scaffold steps that a real
// run would take after writing the files.
type newProjectPlanSteps struct {
	skipTools bool
	linkForge bool
}

// previewNewProject renders the real scaffold into a private staging
// directory, then shares the in-place merge's keep/overwrite decisions
// without writing to the target or running any bootstrap steps. Unlike the
// app and site previews, it lists paths rather than printing file contents.
func previewNewProject(ctx context.Context, w io.Writer, in scaffoldFilesInput, steps newProjectPlanSteps) error {
	in.dryRun = true
	plan, err := writeScaffoldFiles(in)
	if err != nil {
		return err
	}
	cfg := in.gen
	where := "a new directory " + cfg.targetPath
	if in.inPlace {
		where = cfg.targetPath + " (in place)"
	}
	fmt.Fprintf(w, "Dry run: forge project new would scaffold %s project %q into %s. Nothing was written.\n", cfg.kind, cfg.projectName, where)
	if len(cfg.serviceNames) > 0 {
		fmt.Fprintf(w, "  services: %s\n", strings.Join(cfg.serviceNames, ", "))
	}
	if len(cfg.frontendNames) > 0 {
		fmt.Fprintf(w, "  frontends: %s\n", strings.Join(cfg.frontendNames, ", "))
	}

	section := func(title string, paths []string) {
		if len(paths) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s (%d):\n", title, len(paths))
		for _, path := range paths {
			fmt.Fprintln(w, "  "+path)
		}
	}
	section("Would write", plan.created)
	section("Would keep existing", plan.kept)
	section("Would replace (--force)", plan.overwritten)
	if plan.gitignoreMerged {
		if plan.gitignoreChanged {
			fmt.Fprintln(w, "\n.gitignore: would merge forge's entries with yours")
		} else {
			fmt.Fprintln(w, "\n.gitignore: already up to date; would keep yours")
		}
	}
	for _, s := range plan.gitignoreSkipped {
		fmt.Fprintf(w, "  .gitignore: would leave out %s (this repository tracks %s)\n", s.pattern, summarizeTracked(s.tracked))
	}

	fmt.Fprintln(w, "\nThen a real run would:")
	for _, s := range newProjectRunSteps(ctx, in, steps) {
		fmt.Fprintln(w, "  - "+s)
	}
	fmt.Fprintln(w, "\nTo apply, run the same command without --dry-run.")
	return nil
}

// newProjectRunSteps describes (but does not run) follow-up steps that can
// write files beyond the initial scaffold, such as codegen and go mod tidy.
func newProjectRunSteps(ctx context.Context, in scaffoldFilesInput, steps newProjectPlanSteps) []string {
	cfg := in.gen
	service := cfg.kind == config.ProjectKindService
	var out []string
	if steps.linkForge {
		out = append(out, "link go.work to this forge's checkout (--link-forge)")
	}
	if service && !steps.skipTools && in.bufPlugins == "local" {
		out = append(out, fmt.Sprintf("install the proto codegen plugins (%s tools install)", Name()))
	}
	if len(cfg.frontendNames) > 0 {
		out = append(out, "install frontend dependencies (writes lockfiles and node_modules/)")
	}
	if service {
		out = append(out, fmt.Sprintf("run %s generate to bootstrap generated code", Name()))
	}
	// A fresh project may not have a target directory yet. Git would
	// discover the repository of its nearest existing ancestor.
	probe := cfg.targetPath
	for {
		if _, err := os.Stat(probe); err == nil || filepath.Dir(probe) == probe {
			break
		}
		probe = filepath.Dir(probe)
	}
	if top, inRepo := gitWorkTreeRoot(ctx, probe); inRepo {
		out = append(out, "leave the git repository at "+top+" alone (no git init, no commit)")
	} else {
		out = append(out, "git init and attempt an initial commit")
	}
	return append(out, "run go mod tidy", "keep the step log at "+newProjectLog)
}
