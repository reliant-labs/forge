package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/gitsource"
	"github.com/reliant-labs/forge/internal/kclprojects"
	"github.com/reliant-labs/forge/internal/obsdash"
)

// COMPOSED ALERT INVENTORIES.
//
// An env's default alerts watch the services in the deploying project's alert
// inventory (deploy/observability/spec/alert-service-*_gen.json). An env can
// also deploy workloads built from ANOTHER forge project — control-plane's
// prod runs reliant's api server, built by a ShellBuild in ../reliant — and
// those workloads serve that project's rpcs. Its inventory is the other
// project's: forge generate writes it there, from its protos, and this project
// has no copy (and should not keep one by hand). So the env reads it from the
// other project, and each project owns its services' alerts.
//
// WHICH PROJECTS. Not a list anyone declares: the projects the env actually
// builds workloads from. Every workload except a build-only tool contributes
// the directories its build reads source from — a ShellBuild's cwd or
// GitSource, a DockerBuild's named build contexts (or forge.yaml's, which
// they fall back to), a RemoteBuild's source. A directory that sits in a
// forge project other than this one (the nearest forge.yaml at or above it,
// inside its checkout) makes that project composed. A tool workload is
// skipped: it is built, never run, so it serves nothing to alert on.
//
// AT WHICH REVISION. The one this project pins it to, which is the one its
// release builds:
//
//   - a GitSource: the pin's tree (the source cache, or a local override,
//     exactly as the build resolves it);
//   - a checkout of a Go module this project requires: the module at the
//     version go.mod pins, located as the go command and a [forge_projects]
//     KCL import locate it (go.work use, replace, then the module cache,
//     verified against go.sum). The release guard holds such a checkout to
//     that same pin, and control-plane's reliant build compiles that pin
//     whatever the checkout has, so this is the revision that ships, and
//     the result does not depend on what the sibling has checked out;
//   - a checkout nothing pins: the checkout as it is, because that is what
//     the build builds.
//
// A composed source that cannot be read (a sibling not checked out here, a
// pin that cannot be fetched) is reported, not fatal: `forge env shape` must
// still answer on a machine with only this checkout, and a release cannot
// reach this point without it, since its build reads the same source.

// composedAlertSource is one other project an env deploys workloads from.
type composedAlertSource struct {
	// Dir is the project root the inventory is read from, at the revision
	// described by Via.
	Dir string
	// Via says what decided Dir: "go.mod require <mod> <version>",
	// "GitSource <repo>@<ref>", or "checkout <dir> (nothing pins it)".
	Via string
	// Workloads are the env's workloads built from it.
	Workloads []string
}

// buildSourceRef is one place a workload's build reads source from.
type buildSourceRef struct {
	workload string
	path     string // a directory, relative to the project or absolute
	source   *gitsource.Source
}

// composedAlertSources returns the other forge projects env e deploys
// workloads from, each at its pinned revision, plus a note for each source
// whose inventory could not be read. A project that composes none returns
// nothing and touches neither the module cache nor the source cache.
func composedAlertSources(ctx context.Context, projectDir string, e *KCLEntities) ([]composedAlertSource, []string, error) {
	refs := workloadBuildSources(projectDir, e)
	if len(refs) == 0 {
		return nil, nil, nil
	}
	own, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, nil, err
	}
	var resolver *gitsource.Resolver
	byDir := map[string]*composedAlertSource{}
	var notes []string
	for _, r := range refs {
		var (
			checkout, repoRoot string
			pinnedBy           string
		)
		if r.source != nil {
			if resolver == nil {
				// No logger: `forge env shape --json` writes its document to
				// stdout, which the build's resolver logs to.
				if resolver, err = gitsource.NewResolver(projectDir); err != nil {
					return nil, nil, err
				}
			}
			res, rerr := resolver.Resolve(ctx, *r.source)
			if rerr != nil {
				notes = append(notes, fmt.Sprintf("the alert inventory of %s (built by %s) was not read: %v", r.source, r.workload, rerr))
				continue
			}
			checkout = res.Dir
			repoRoot = strings.TrimSuffix(res.Dir, string(filepath.Separator)+filepath.FromSlash(r.source.Subdir))
			pinnedBy = "GitSource " + r.source.String()
		} else {
			checkout = r.path
			if !filepath.IsAbs(checkout) {
				checkout = filepath.Join(own, checkout)
			}
			checkout = filepath.Clean(checkout)
			if isWithin(own, checkout) {
				continue
			}
			if st, serr := os.Stat(checkout); serr != nil || !st.IsDir() {
				// A build context may name an image (docker-image://...)
				// rather than a directory; only a missing directory is news.
				if !strings.Contains(r.path, "://") {
					notes = append(notes, fmt.Sprintf("the alert inventory of %s (built by %s) was not read: the directory is not checked out here, so this shape omits the default alerts of any services it serves", r.path, r.workload))
				}
				continue
			}
		}
		root := forgeProjectRoot(checkout, repoRoot)
		if root == "" || isWithin(own, root) {
			continue
		}
		dir, via := root, pinnedBy
		if via == "" {
			loc, ok, lerr := pinnedProjectDir(own, root, r.workload)
			if lerr != nil {
				notes = append(notes, fmt.Sprintf("the alert inventory of %s (built by %s) was not read: %v", relOrAbs(own, root), r.workload, lerr))
				continue
			}
			if ok {
				dir, via = loc.Dir, loc.Via
			} else {
				via = "checkout " + relOrAbs(own, root) + " (nothing pins it)"
			}
		}
		c := byDir[dir]
		if c == nil {
			c = &composedAlertSource{Dir: dir, Via: via}
			byDir[dir] = c
		}
		c.Workloads = appendUnique(c.Workloads, r.workload)
	}
	out := make([]composedAlertSource, 0, len(byDir))
	for _, c := range byDir {
		sort.Strings(c.Workloads)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, notes, nil
}

// composedAlertServices reads each composed project's alert inventory.
func composedAlertServices(sources []composedAlertSource) ([][]obsdash.AlertService, error) {
	out := make([][]obsdash.AlertService, 0, len(sources))
	for _, c := range sources {
		svcs, err := obsdash.ReadAlertServices(c.Dir)
		if err != nil {
			return nil, fmt.Errorf("the alert inventory of %s (%s; workloads %s): %w",
				c.Dir, c.Via, strings.Join(c.Workloads, ", "), err)
		}
		out = append(out, svcs)
	}
	return out, nil
}

// workloadBuildSources lists every source directory or pin the env's
// non-tool workloads build from.
func workloadBuildSources(projectDir string, e *KCLEntities) []buildSourceRef {
	if e == nil {
		return nil
	}
	var cfgContexts map[string]config.BuildContext
	cfgLoaded := false
	var out []buildSourceRef
	addContexts := func(w string, contexts map[string]config.BuildContext) {
		for _, name := range sortedKeys(contexts) {
			c := contexts[name]
			switch {
			case c.Source != nil:
				s := toGitSource(c.Source)
				out = append(out, buildSourceRef{workload: w, source: &s})
			case c.Path != "":
				out = append(out, buildSourceRef{workload: w, path: c.Path})
			}
		}
	}
	for _, w := range e.Workloads {
		if w.Kind == "tool" {
			continue
		}
		b := w.Build
		switch {
		case b.Shell != nil:
			if b.Shell.Source != nil {
				s := toGitSource(b.Shell.Source)
				out = append(out, buildSourceRef{workload: w.Name, source: &s})
			} else if b.Shell.Cwd != "" {
				out = append(out, buildSourceRef{workload: w.Name, path: b.Shell.Cwd})
			}
		case b.Docker != nil:
			if b.Docker.Context != "" {
				out = append(out, buildSourceRef{workload: w.Name, path: b.Docker.Context})
			}
			if len(b.Docker.BuildContexts) > 0 {
				addContexts(w.Name, b.Docker.BuildContexts)
				continue
			}
			if !cfgLoaded {
				cfgLoaded = true
				if cfg, err := config.LoadProjectDir(projectDir); err == nil && cfg != nil {
					cfgContexts = cfg.Docker.BuildContexts
				}
			}
			addContexts(w.Name, cfgContexts)
		case b.Remote != nil && b.Remote.Source.Repo != "":
			s := gitsource.Source{Repo: b.Remote.Source.Repo, Ref: b.Remote.Source.Ref, Subdir: b.Remote.Source.Subdir}
			out = append(out, buildSourceRef{workload: w.Name, source: &s})
		}
	}
	return out
}

// pinnedProjectDir is root (a forge project's checkout) at the version this
// project's go.mod pins its module to; ok is false when nothing pins it.
func pinnedProjectDir(own, root, workload string) (kclprojects.Location, bool, error) {
	modRoot := findUpBounded(root, "go.mod", "")
	mod := kclprojects.ModuleAt(modRoot)
	if mod == "" {
		return kclprojects.Location{}, false, nil
	}
	loc, ok, err := kclprojects.LocateModule(own, mod, "the alert inventory of the workloads built from "+relOrAbs(own, root)+" ("+workload+")")
	if err != nil || !ok {
		return loc, ok, err
	}
	// The forge project may sit below its module's root.
	if rel, rerr := filepath.Rel(modRoot, root); rerr == nil && rel != "." {
		loc.Dir = filepath.Join(loc.Dir, rel)
	}
	return loc, true, nil
}

// forgeProjectRoot is the nearest directory at or above dir holding a
// forge.yaml, looking no higher than its checkout: stop, if not given one,
// at the first directory holding .git. "" when there is none.
func forgeProjectRoot(dir, stop string) string {
	return findUpBounded(dir, "forge.yaml", stop)
}

// findUpBounded returns the nearest directory at or above dir holding name,
// never above stop; with no stop, never above the first directory holding
// .git (the checkout's root).
func findUpBounded(dir, name, stop string) string {
	for cur := dir; ; {
		if _, err := os.Stat(filepath.Join(cur, name)); err == nil {
			return cur
		}
		if cur == stop {
			return ""
		}
		if stop == "" {
			if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
				return ""
			}
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return ""
		}
		cur = parent
	}
}

// isWithin reports whether p is root or below it.
func isWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// relOrAbs is p relative to base when that stays readable, else p.
func relOrAbs(base, p string) string {
	if rel, err := filepath.Rel(base, p); err == nil {
		return rel
	}
	return p
}

func appendUnique(xs []string, x string) []string {
	for _, y := range xs {
		if y == x {
			return xs
		}
	}
	return append(xs, x)
}
