// Package cli — forge KCL module vendor handling.
//
// A scaffolded project's deploy/kcl/kcl.mod depends on the `forge` KCL
// module. `forge generate` resolves that dependency exactly one way, on
// every build of forge: it materializes the module EMBEDDED IN THE
// BINARY into `<project>/.forge-kcl/` and points the kcl.mod at it by
// relative path. Relative — not a hand-patched absolute host path — so
// containers, CI checkouts, and other machines resolve the identical
// vendored copy, offline and with nothing to publish.
//
// There is no un-vendor direction and no release-vs-dev branch. Forge
// once had both, and a release build would delete a project's working
// `.forge-kcl/` and rewrite the dependency to a git tag that had never
// been published — breaking every project it touched. See
// docs/adr/0001-always-vendor-forge-kcl.md.
//
// All kcl.mod surgery lives in internal/kclvendor (shared with the
// scaffolder, which uses the same primitives so projects are born
// already vendored). This file owns pipeline orchestration + printing.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/kclvendor"
)

// kclModCandidates returns the kcl.mod locations forge manages, in
// resolution-priority order: deploy/kcl/kcl.mod (the canonical package
// root for deploy manifests) and the legacy project-root kcl.mod that
// older scaffolds emitted. Each is patched with its own correctly
// depth-adjusted relative path.
func kclModCandidates(projectDir string) []string {
	return []string{
		filepath.Join(projectDir, "deploy", "kcl", "kcl.mod"),
		filepath.Join(projectDir, "kcl.mod"),
	}
}

// stepSyncForgeKCL keeps the project's kcl.mod forge-module dependency
// pointed at the vendored copy of the KCL module embedded in this forge
// binary, and refreshes that copy. Best-effort: failure warns and the
// pipeline continues (--strict promotes to fatal), matching the
// forge/pkg sync step.
func stepSyncForgeKCL(ctx *pipelineContext) error {
	return ctx.warnOrFail("forge KCL module vendor sync", syncForgeKCL(ctx.ProjectDir))
}

// syncForgeKCL implements the sync. Split from the step for direct
// testing.
func syncForgeKCL(projectDir string) error {
	// Ensure every managed kcl.mod resolves the module from the vendored
	// copy, then materialize/refresh that copy. A kcl.mod forge cannot
	// prove it understands is warned about, never edited.
	var patched []string
	referenced := false
	for _, modPath := range kclModCandidates(projectDir) {
		res, err := kclvendor.EnsureVendorDep(modPath, projectDir)
		if err != nil {
			return err
		}
		if res.Warning != "" {
			fmt.Fprintf(os.Stderr, "⚠️  Warning: %s\n", res.Warning)
			continue
		}
		kind, err := kclvendor.InspectDep(modPath)
		if err != nil {
			return err
		}
		if kind == kclvendor.DepVendored {
			referenced = true
			if res.Changed {
				patched = append(patched, projectRelPath(projectDir, modPath))
			}
		}
	}
	if !referenced {
		// Nothing points at the vendor dir (no kcl.mod, or shapes we
		// don't manage) — never materialize an orphan directory.
		return nil
	}

	changed, err := kclvendor.Materialize(projectDir)
	if err != nil {
		return err
	}
	if len(patched) > 0 {
		fmt.Printf("  ✅ Vendored forge KCL module → %s/ (%s now resolve(s) it by relative path)\n",
			kclvendor.VendorDirName, strings.Join(patched, ", "))
	} else if changed {
		fmt.Printf("  ✅ Refreshed %s/ from this forge binary's embedded KCL module\n", kclvendor.VendorDirName)
	}
	return nil
}

// projectRelPath renders path relative to projectDir for messages,
// falling back to the absolute path when Rel fails.
func projectRelPath(projectDir, path string) string {
	if rel, err := filepath.Rel(projectDir, path); err == nil {
		return filepath.ToSlash(rel)
	}
	return path
}
