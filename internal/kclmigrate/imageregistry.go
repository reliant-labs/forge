// Package kclmigrate rewrites a project's deploy/kcl tree across a forge
// change that moved a declaration from one place to another.
//
// It works TEXTUALLY, on purpose. The obvious alternative — render the env and
// read the resolved values — is not available: the old tree no longer compiles
// against the new schema (the field being migrated is now a closed-schema
// error), so there is nothing to render. Working on text also means the rewrite
// preserves comments, ordering and formatting, which a render-and-reprint would
// destroy.
package kclmigrate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ImageRegistryResult is what a migration run did or refused to do.
type ImageRegistryResult struct {
	// Rewrites is one entry per file changed, with a human description.
	Rewrites []string
	// Dropped names env registries that were removed WITHOUT being prefixed
	// onto any image, with the reason — a registry on an env where nothing
	// pulls an image says nothing about where an image should live.
	Dropped []string
	// Ambiguous is non-empty when the migration refused. Each entry is one
	// workload that cannot be rewritten automatically, with the exact per-env
	// KCL to write instead.
	Ambiguous []AmbiguousImage
}

// Applied reports whether anything changed on disk.
func (r ImageRegistryResult) Applied() bool { return len(r.Rewrites) > 0 }

// Refused reports whether the migration declined to rewrite.
func (r ImageRegistryResult) Refused() bool { return len(r.Ambiguous) > 0 }

// AmbiguousImage is one workload whose image cannot be completed
// unambiguously: it is bound to an image-pulling runtime in two or more envs
// that declared DIFFERENT registries, so there is no single reference that is
// correct everywhere.
type AmbiguousImage struct {
	// Workload is the declaration's name in deploy/kcl/workloads.k.
	Workload string
	// Image is the bare image it declares today.
	Image string
	// ByEnv maps env name → the registry that env declared, for the envs
	// where this workload is bound to a runtime that pulls.
	ByEnv map[string]string
}

// Runbook is the exact KCL to write, per env, to resolve this workload by hand.
//
// It is per-env because that is what the situation actually is: the workload
// genuinely has a different reference in each env, so the declaration in
// workloads.k cannot state one, and each env refines it. forge prints this
// rather than guessing, because picking one registry silently would point some
// env at a repository its image was never pushed to — an ImagePullBackOff
// minutes after a deploy reported success.
func (a AmbiguousImage) Runbook() string {
	var b strings.Builder
	fmt.Fprintf(&b, "workload %q declares the bare image %q and is bound to an image-pulling runtime in %d envs that declared different registries:\n",
		a.Workload, a.Image, len(a.ByEnv))
	for _, env := range sortedKeys(a.ByEnv) {
		fmt.Fprintf(&b, "    %-12s %s\n", env, a.ByEnv[env])
	}
	b.WriteString("\n  There is no single reference that is right in every env, so forge will not pick one.\n")
	b.WriteString("  Refine the image per env. In each deploy/kcl/<env>/main.k, alongside the binding:\n\n")
	for _, env := range sortedKeys(a.ByEnv) {
		fmt.Fprintf(&b, "    # deploy/kcl/%s/main.k\n", env)
		fmt.Fprintf(&b, "    %s | {image = %q}\n", "wl."+kclIdent(a.Workload), a.ByEnv[env]+"/"+a.Image)
	}
	b.WriteString("\n  Or, if one registry is the common case, declare THAT in deploy/kcl/workloads.k\n")
	b.WriteString("  and re-point it in the envs that differ with forge.image_on_registry:\n\n")
	b.WriteString("    # deploy/kcl/<the-odd-env>/main.k\n")
	b.WriteString("    " + "wl." + kclIdent(a.Workload) + " | {image = forge.image_on_registry(wl." + kclIdent(a.Workload) + ".image, \"<that env's registry host>\")}\n")
	return b.String()
}

var (
	// A `registry = "<value>"` on its own line, in any indentation. Only a
	// literal is matched: a computed registry (an option(), a concatenation) is
	// not something this migration can resolve to one value, and it is left in
	// place so the closed-schema error names it and a human decides.
	registryLineRe = regexp.MustCompile(`(?m)^[ \t]*registry[ \t]*=[ \t]*"([^"]*)"[ \t]*(#[^\n]*)?\n`)
	// `registry = "<value>"` inline in a braced literal, with a comma either
	// side — `forge.ClusterTarget {cluster = "c", registry = "r", ...}`.
	registryInlineRe = regexp.MustCompile(`,[ \t]*registry[ \t]*=[ \t]*"[^"]*"|registry[ \t]*=[ \t]*"[^"]*"[ \t]*,[ \t]*`)
	// `image = "<value>"` on a workload declaration.
	imageLineRe = regexp.MustCompile(`(?m)^([ \t]*)image[ \t]*=[ \t]*"([^"]*)"[ \t]*(#[^\n]*)?$`)
	// `<ident> = fw.Workload {` — the start of a declaration in workloads.k.
	workloadStartRe = regexp.MustCompile(`(?m)^([A-Za-z_][A-Za-z0-9_]*)[ \t]*=[ \t]*fw\.Workload[ \t]*\{`)
	// A binder lambda's name and the runtime its body constructs.
	binderRe  = regexp.MustCompile(`(?m)^(_[A-Za-z0-9_]*)[ \t]*=[ \t]*lambda\b`)
	runtimeRe = regexp.MustCompile(`forge\.(OnCluster|OnHosted|OnHost|OnCompose|BuildOnly)\b`)
	// A binder application in an env's workload list: `_on_cluster(wl.item)`.
	bindingRe = regexp.MustCompile(`(_[A-Za-z0-9_]*)\(\s*wl\.([A-Za-z_][A-Za-z0-9_]*)`)
)

// runtimesThatPull are the runtimes an image reference is needed for. A
// workload bound OnHost or OnCompose runs from the local filesystem or a
// compose file: nothing pulls an image for it, so the env's registry says
// nothing about where that workload's image should live. This is the whole
// basis of the ambiguity judgment.
var runtimesThatPull = map[string]bool{"OnCluster": true, "OnHosted": true}

// ImageRegistry migrates a project off env-level registries: it removes
// `registry = …` from every ClusterTarget / ControlPlane in deploy/kcl, and
// prefixes the removed value onto a workload's bare `image` when — and only
// when — that is unambiguous.
//
// UNAMBIGUOUS means: across every env where this workload is bound to a runtime
// that PULLS an image, exactly one registry was declared. An env that binds the
// workload OnHost, OnCompose or BuildOnly, or does not bind it at all, is not
// consulted — its registry was never going to be this workload's.
//
// So a project whose dev env declares a localhost registry and binds everything
// to the host, while prod declares ghcr.io and deploys to a cluster, migrates
// cleanly to ghcr.io: dev's registry is DROPPED, because nothing in dev ever
// pulled an image.
//
// Genuinely ambiguous workloads are REFUSED as a set, with the exact per-env
// KCL to write. Nothing is rewritten in that case — a partial migration would
// leave the tree in a state neither the old nor the new forge understands.
//
// USER BINDER EXPRESSIONS ARE NEVER EDITED. The binders are lambdas, and
// rewriting a lambda body is how a migration corrupts a file it does not
// understand. This reads them to learn which runtime each binder produces, and
// writes only to `registry = …` lines and `image = …` lines.
func ImageRegistry(projectDir string, apply bool) (ImageRegistryResult, error) {
	var res ImageRegistryResult
	kclDir := filepath.Join(projectDir, "deploy", "kcl")
	if _, err := os.Stat(kclDir); err != nil {
		return res, nil // no deploy tree: nothing to migrate
	}

	envs, err := readEnvs(kclDir)
	if err != nil {
		return res, err
	}
	workloadsPath := filepath.Join(kclDir, "workloads.k")
	workloadsSrc, _ := os.ReadFile(workloadsPath)

	// Which envs each workload is bound to a PULLING runtime in, and what
	// registry each of those envs declared.
	pullRegistries := map[string]map[string]string{} // workload -> env -> registry
	for _, e := range envs {
		if e.registry == "" {
			continue
		}
		for workload, runtime := range e.bindings {
			if !runtimesThatPull[runtime] {
				continue
			}
			if pullRegistries[workload] == nil {
				pullRegistries[workload] = map[string]string{}
			}
			pullRegistries[workload][e.name] = e.registry
		}
	}

	// Judge ambiguity BEFORE writing anything.
	bare := bareImages(string(workloadsSrc))
	for workload, image := range bare {
		byEnv := pullRegistries[workload]
		if len(distinctValues(byEnv)) > 1 {
			res.Ambiguous = append(res.Ambiguous, AmbiguousImage{Workload: workload, Image: image, ByEnv: byEnv})
		}
	}
	if len(res.Ambiguous) > 0 {
		sort.Slice(res.Ambiguous, func(i, j int) bool { return res.Ambiguous[i].Workload < res.Ambiguous[j].Workload })
		return res, nil
	}

	// Rewrite workloads.k: complete each bare image whose registry is settled.
	newWorkloads := string(workloadsSrc)
	for workload, image := range bare {
		regs := distinctValues(pullRegistries[workload])
		if len(regs) != 1 {
			continue // no pulling binding anywhere: it needs no registry
		}
		newWorkloads = setWorkloadImage(newWorkloads, workload, regs[0]+"/"+image)
		res.Rewrites = append(res.Rewrites, fmt.Sprintf("%s: workload %q image %q → %q",
			"deploy/kcl/workloads.k", workload, image, regs[0]+"/"+image))
	}

	// Remove every env-level registry.
	for _, e := range envs {
		stripped := stripRegistry(e.src)
		if stripped == e.src {
			continue
		}
		used := false
		for workload := range bare {
			if pullRegistries[workload][e.name] != "" {
				used = true
				break
			}
		}
		rel := filepath.Join("deploy", "kcl", e.name, "main.k")
		res.Rewrites = append(res.Rewrites, fmt.Sprintf("%s: removed `registry = %q`", rel, e.registry))
		if !used {
			res.Dropped = append(res.Dropped, fmt.Sprintf("%s: `registry = %q` dropped — no workload this env binds to a cluster or hosted runtime needed it",
				rel, e.registry))
		}
		if apply {
			if err := os.WriteFile(e.path, []byte(stripped), 0o644); err != nil {
				return res, fmt.Errorf("write %s: %w", e.path, err)
			}
		}
	}
	if apply && newWorkloads != string(workloadsSrc) {
		if err := os.WriteFile(workloadsPath, []byte(newWorkloads), 0o644); err != nil {
			return res, fmt.Errorf("write %s: %w", workloadsPath, err)
		}
	}
	sort.Strings(res.Rewrites)
	sort.Strings(res.Dropped)
	return res, nil
}

// envFile is one env's main.k, with what the migration needs read off it.
type envFile struct {
	name     string
	path     string
	src      string
	registry string
	// bindings maps workload name → the runtime the binder it is passed to
	// constructs ("OnCluster", "OnHost", …).
	bindings map[string]string
}

// readEnvs loads every deploy/kcl/<env>/main.k and reads its declared registry
// and its per-workload runtime bindings.
func readEnvs(kclDir string) ([]envFile, error) {
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil, err
	}
	var out []envFile
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(kclDir, entry.Name(), "main.k")
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		src := string(b)
		e := envFile{name: entry.Name(), path: path, src: src, bindings: bindings(src)}
		if m := registryLineRe.FindStringSubmatch(src); m != nil {
			e.registry = m[1]
		} else if m := regexp.MustCompile(`registry[ \t]*=[ \t]*"([^"]*)"`).FindStringSubmatch(src); m != nil {
			e.registry = m[1]
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// bindings maps each workload an env binds to the runtime its binder produces.
//
// It reads the binder lambdas to learn what each one does, rather than assuming
// a naming convention: a project renames `_on_k3d` freely, and a migration that
// keyed off the name would silently misjudge which envs pull an image. The
// lambda bodies are READ, never rewritten.
func bindings(src string) map[string]string {
	// binder name → runtime, from each lambda's body.
	binderRuntime := map[string]string{}
	locs := binderRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if m := runtimeRe.FindStringSubmatch(src[loc[0]:end]); m != nil {
			binderRuntime[name] = m[1]
		}
	}
	out := map[string]string{}
	for _, m := range bindingRe.FindAllStringSubmatch(src, -1) {
		binder, workload := m[1], m[2]
		if rt, ok := binderRuntime[binder]; ok {
			out[workload] = rt
		}
	}
	return out
}

// bareImages maps each workload in workloads.k that declares an image with NO
// registry host to that image. A workload with a complete reference, or none at
// all, is already correct and is not touched.
func bareImages(src string) map[string]string {
	out := map[string]string{}
	locs := workloadStartRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		name := src[loc[2]:loc[3]]
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		if m := imageLineRe.FindStringSubmatch(src[loc[0]:end]); m != nil {
			if image := m[2]; image != "" && !hasRegistryHost(image) {
				out[name] = image
			}
		}
	}
	return out
}

// setWorkloadImage rewrites the `image = …` line inside one workload block.
func setWorkloadImage(src, workload, image string) string {
	locs := workloadStartRe.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		if src[loc[2]:loc[3]] != workload {
			continue
		}
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		block := src[loc[0]:end]
		replaced := imageLineRe.ReplaceAllStringFunc(block, func(line string) string {
			m := imageLineRe.FindStringSubmatch(line)
			out := m[1] + "image = " + fmt.Sprintf("%q", image)
			if m[3] != "" {
				out += " " + m[3]
			}
			return out
		})
		return src[:loc[0]] + replaced + src[end:]
	}
	return src
}

// stripRegistry removes a `registry = "…"` declaration, whether it sits on its
// own line or inline in a braced literal.
func stripRegistry(src string) string {
	out := registryLineRe.ReplaceAllString(src, "")
	return registryInlineRe.ReplaceAllString(out, "")
}

// hasRegistryHost mirrors kcl/lib/images.k: the first path segment is a
// registry host iff it contains a `.` or a `:`, or is exactly `localhost`.
func hasRegistryHost(image string) bool {
	slash := strings.Index(image, "/")
	if slash <= 0 {
		return false
	}
	head := image[:slash]
	return strings.ContainsAny(head, ".:") || head == "localhost"
}

func distinctValues(m map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range m {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// kclIdent folds a workload name into the KCL identifier workloads.k declares
// it under (hyphens become underscores) — matching internal/naming.
func kclIdent(name string) string {
	return strings.NewReplacer("-", "_", ".", "_").Replace(name)
}
