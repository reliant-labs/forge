// Package kclrender is the single seam through which forge evaluates KCL.
// It renders via the embedded kpm package manager + kcl-go runtime — no
// external `kcl` binary required on PATH — and registers forge's KCL
// plugin namespace (kcl_plugin.forge.*) so KCL can pull host-runtime
// values (e.g. resolve_port) during evaluation.
//
// kpm reads the package's kcl.mod and resolves dependencies — git, local
// path, and OCI/registry — exactly like the `kcl` CLI, so projects
// declare the forge module (and any extra packages) in kcl.mod in
// whatever style they like; forge neither parses nor special-cases deps.
package kclrender

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	kcl "kcl-lang.io/kcl-go"
	"kcl-lang.io/kpm/pkg/client"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/devstack"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclvendor"
	"github.com/reliant-labs/forge/internal/kubeconfig"
)

// forgeModuleArg resolves the external-package binding that supplies
// `import forge` from THIS binary's embedded module, after refusing a
// project whose kcl.mod still declares the module itself.
//
// This is the whole of how a project resolves forge's KCL: no kcl.mod
// dependency, no project-local copy, no network. The binary rendering the
// project is the module version it renders against — for a project pinned
// to a released forge, CI's `go install …@vX.Y.Z` is that release's module.
// See internal/kclvendor.
//
// The unmigrated-kcl.mod refusal lives here, at the one seam every render
// passes through, because the failure it prevents is silent: kpm resolves a
// declared `forge` dependency AHEAD of an external package, so an old
// `forge = { path = "../../.forge-kcl" }` line would render against whatever
// stale copy sat on disk — or fail on a missing directory with kpm's own
// `kcl mod add` advice, which is wrong for a forge project.
func forgeModuleArg(workDir string) (string, error) {
	if err := kclvendor.CheckKclMods(workDir); err != nil {
		return "", err
	}
	// Same seam, same reason: a project still calling the retired
	// forge.registry helper would otherwise fail on KCL's "attribute
	// 'registry' not found in module 'forge'", which names no fix.
	if err := kclvendor.CheckRegistryHelper(workDir); err != nil {
		return "", err
	}
	arg, err := kclvendor.ExternalPkgArg()
	if err != nil {
		return "", fmt.Errorf("materialize the forge KCL module: %w", err)
	}
	return arg, nil
}

// pluginPreflight refuses the render when this binary cannot service
// kcl_plugin.forge.* calls, which is the case exactly when it was built
// with CGO_ENABLED=0 (kclplugin.Register is a no-op under !cgo).
//
// Without this, the no-op registration is silent until render time and
// the user sees KCL's own diagnostic — "the plugin package
// 'kcl_plugin.forge' is not found ... confirm if plugin mode is enabled"
// — which names a knob forge does not have and never mentions CGO. Every
// forge environment's KCL imports kcl_plugin.forge, so a CGO-free binary
// cannot render ANY environment while passing --version, generate, lint
// and build. The failure belongs here, at the one seam every render
// passes through, phrased as a runbook.
//
// available is a parameter rather than a direct kclplugin.Available()
// call so the message is testable in a CGO-enabled test binary — build
// tags cannot be flipped inside one test process.
func pluginPreflight(available bool, version string) error {
	if available {
		return nil
	}
	if version == "" || buildinfo.IsDevVersion(version) {
		// A "(devel)"/+dirty stamp names no ref a module proxy can
		// serve, so `go install ...@<version>` would hand the user a
		// command that fails. Point at the contributor install instead.
		return fmt.Errorf(
			"this forge binary was built without CGO, so the kcl_plugin.forge namespace is\n"+
				"  unavailable and no environment can be rendered.\n"+
				"    expected: a forge built with CGO_ENABLED=1 (registers kcl_plugin.forge in-process)\n"+
				"    found:    this binary (%s) has no plugin namespace to register\n"+
				"  Fix: rebuild this checkout with CGO enabled:\n"+
				"    CGO_ENABLED=1 task install:dev",
			version)
	}
	return fmt.Errorf(
		"this forge binary was built without CGO, so the kcl_plugin.forge namespace is\n"+
			"  unavailable and no environment can be rendered.\n"+
			"    expected: a forge built with CGO_ENABLED=1 (registers kcl_plugin.forge in-process)\n"+
			"    found:    this binary (%s) has no plugin namespace to register\n"+
			"  Fix: reinstall with CGO enabled:\n"+
			"    CGO_ENABLED=1 go install github.com/reliant-labs/forge/cmd/forge@%s",
		version, version)
}

// withKubeconfigDArg appends `kubeconfig=<quoted path>` — the machine's
// default kubeconfig — to the render's `-D` bindings. It backs
// `forge.default_kubeconfig()`, and through it `Cluster.kubeconfig`, which is
// how a HOST process declares where a cluster's kubeconfig lives.
//
// It is applied HERE, in the one seam every render passes through, rather than
// at each caller. There are a dozen call sites (env up, env deploy, doctor,
// new-env validation, the config probes, the tests), and a binding that only
// some of them passed would make `<cluster>.kubeconfig` resolve under `forge
// env up` and come back empty under `forge env render` — a difference between
// two renders of the same file, which is the failure this field exists to
// remove.
//
// Quoted with strconv.Quote so KCL types it as `str`, for the same reason
// image_tag is quoted (internal/cluster.renderDArgs): an unquoted value is
// type-inferred, and a path is not reliably a string to KCL.
//
// A caller that already supplied its own `kubeconfig=` wins — nothing does
// today, but an explicit binding should not be silently overridden by a
// derived one. An unresolvable path (no KUBECONFIG, no home directory) appends
// nothing, leaving `option("kubeconfig")` as None and the accessor's "" —
// forge does not guess a path it cannot derive.
func withKubeconfigDArg(dArgs []string) []string {
	for _, a := range dArgs {
		if strings.HasPrefix(a, "kubeconfig=") {
			return dArgs
		}
	}
	path := kubeconfig.DefaultPath()
	if path == "" {
		return dArgs
	}
	// Copy rather than append in place: callers hand us slices they build up
	// and reuse across renders, and appending to a shared backing array would
	// let one render's binding leak into another's.
	out := make([]string, 0, len(dArgs)+1)
	out = append(out, dArgs...)
	return append(out, "kubeconfig="+strconv.Quote(path))
}

// withDevStackDArgs appends the ACTIVE parallel-dev-stack git facts —
// `worktree=` and `branch=` — to the render's `-D` bindings, for the same
// reason and at the same seam as withKubeconfigDArg.
//
// These bindings are not cosmetic: a project keys its NAMESPACE on them.
// control-plane's deploy/kcl/dev/main.k computes
// `_namespace = option("namespace") or identity.namespace(option("worktree"))`,
// so a render that omits `worktree=` resolves to the unsuffixed namespace
// while the deploy that applied the objects resolved to the suffixed one.
//
// That is exactly the defect this centralization removes. `forge env deploy`
// (internal/cluster.renderDArgs) and `forge env up`'s entity render
// (internal/cli.renderKCLRaw) both passed these bindings; the doctor's
// cluster-health render (internal/doctor.renderEnvForCluster) did not. So
// `forge env status dev` listed pods in `control-plane-dev`, found none —
// they were all in `control-plane-dev-<worktree>`, running — and reported
// four healthy workloads as "NO PODS", failing the check and the `forge env
// up` exit code with it.
//
// Applying it here rather than at each caller is what makes the omission
// unrepresentable: there is one place a render can be constructed, so a
// render cannot be constructed without the facts that decide where its
// objects live. A caller that already bound a key wins, matching
// withKubeconfigDArg — an explicit binding is never overridden by a derived
// one.
//
// devstack.ActiveDArgs() is the zero value (no args) in any process that
// never called devstack.SetActive — `forge ci`, `forge project audit`, the
// tests — so every render outside the up/deploy path stays byte-identical.
func withDevStackDArgs(dArgs []string) []string {
	add := devstack.ActiveDArgs()
	if len(add) == 0 {
		return dArgs
	}
	// Copy rather than append in place, for the reason withKubeconfigDArg
	// copies: callers build a slice up and reuse it across renders.
	out := make([]string, 0, len(dArgs)+len(add))
	out = append(out, dArgs...)
	for _, a := range add {
		key, _, ok := strings.Cut(a, "=")
		if !ok {
			continue
		}
		bound := false
		for _, existing := range dArgs {
			if strings.HasPrefix(existing, key+"=") {
				bound = true
				break
			}
		}
		if !bound {
			out = append(out, a)
		}
	}
	return out
}

// Run renders the KCL at source — a package directory or a single .k
// file — and returns the raw JSON result.
//
// workDir is the process cwd KCL resolves relative reads against, and the
// project root whose managed kcl.mod files are checked for a legacy `forge`
// dependency (forgeModuleArg), so it is part of the contract. The `forge`
// module itself is supplied from this binary as an external package.
// dArgs are `-D key=value` top-level option assignments (e.g. "env=dev").
// `kubeconfig` is appended here for every render — see withKubeconfigDArg —
// as are the active dev-stack git facts, see withDevStackDArgs.
// kpm progress/diagnostics go to stderr.
func Run(workDir, source string, dArgs []string) ([]byte, error) {
	return RunIn(workDir, workDir, source, dArgs)
}

// RunIn is Run with the two directories Run collapses into one held apart:
// projectDir is the project root the kcl.mod migration checks read, and
// workDir is the cwd KCL evaluates in.
//
// Every env render wants them equal — an env package is `deploy/kcl/<env>`
// and relative `lib.*` imports resolve from the project root either way — so
// Run stays the one-argument form and this is the exception.
//
// The exception is real, not hypothetical. A KCL file that is not an env
// resolves its imports from ITS OWN kcl.mod package root:
// control-plane's `deploy/kcl/lib/platform_local.k` says `import
// lib.barman_plugin`, which resolves only with `deploy/kcl` as the cwd (the
// scripts reading it all `cd` there first). Evaluating it with the project
// root as workDir fails on the import; evaluating it with `deploy/kcl` as
// BOTH would point the migration checks at a subtree, and
// CheckRegistryHelper walks that subtree — so a stale helper elsewhere in the
// project would go unreported for this render and be reported for every
// other, which is the kind of difference between two renders of one project
// that forge exists to remove.
func RunIn(projectDir, workDir, source string, dArgs []string) ([]byte, error) {
	// Make kcl_plugin.forge (resolve_port, …) available. Idempotent;
	// the registry is process-global.
	kclplugin.Register()

	// Register is a no-op in a CGO-free build, so verify the namespace
	// is actually there before handing KCL a program that imports it.
	if err := pluginPreflight(kclplugin.Available(), buildinfo.Version()); err != nil {
		return nil, err
	}

	forgeArg, err := forgeModuleArg(projectDir)
	if err != nil {
		return nil, err
	}

	c, err := client.NewKpmClient()
	if err != nil {
		return nil, fmt.Errorf("kpm client: %w", err)
	}
	// Serialized: concurrent evaluations in one process corrupt each other's
	// refusals (a refused render can come back as success, or carrying
	// another render's message). See kclplugin.Serialized.
	res, err := kclplugin.Serialized(func() (*kcl.KCLResultList, error) {
		return c.Run(
			client.WithRunSourceUrl(source),
			client.WithWorkDir(workDir),
			client.WithArguments(withKubeconfigDArg(withDevStackDArgs(dArgs))),
			client.WithExternalPkgs([]string{forgeArg}),
			client.WithLogger(os.Stderr),
		)
	})
	if err != nil {
		return nil, fmt.Errorf("kpm run %s: %w", source, err)
	}
	return []byte(res.GetRawJsonResult()), nil
}
