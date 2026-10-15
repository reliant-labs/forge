// Command affected is `forge ci affected` without the forge CLI around it.
//
// forge's own Taskfile runs it before every `task test`, so it must start in a
// second, which the 1,600-package forge binary cannot: it links only the
// affected-package selector and the forge.yaml loader. Everything it decides
// lives in internal/affected, shared with the forge verb.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/reliant-labs/forge/internal/affected"
	"github.com/reliant-labs/forge/internal/config"
)

func main() {
	var o affected.Options
	flag.StringVar(&o.Base, "base", "", "ref the change is measured from (default origin/<default branch>; also CI_BASE)")
	flag.StringVar(&o.Files, "files", "", "judge this comma-separated file list (or @file) instead of the work tree's diff")
	flag.StringVar(&o.Lane, "lane", "unit", "unit, e2e (-tags e2e) or integration (-tags integration)")
	flag.BoolVar(&o.Packages, "packages", false, "print only the package list on stdout; the plan goes to stderr")
	flag.BoolVar(&o.JSON, "json", false, "print the whole decision as JSON")
	flag.BoolVar(&o.Full, "full", false, "select the whole module (also CI_FULL=1)")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		fail(err)
	}
	cfg, err := config.LoadProjectDir(root)
	if err != nil {
		fail(fmt.Errorf("read forge.yaml: %w", err))
	}
	o.Root, o.Gate = root, cfg.CI.Gate
	if err := affected.Run(context.Background(), o, os.Stdout, os.Stderr); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "affected:", err)
	os.Exit(2)
}
