// Package cli — dev-mode forge KCL module vendor handling.
//
// The KCL sibling of dev_pkg_replace.go. A scaffolded project's
// deploy/kcl/kcl.mod depends on the `forge` KCL module via a published
// `kcl-vX.Y.Z` git tag. Dev builds of forge have no published tag (the
// same way they have no ldflags-stamped forge/pkg version), so `forge
// generate` materializes the module EMBEDDED IN THE BINARY into
// `<project>/.forge-kcl/` and rewrites the kcl.mod dependency to a
// relative path. Relative — not the historical hand-patched absolute
// host path — so containers, CI checkouts, and other machines resolve
// the identical vendored copy.
//
// Release builds swap back: the marker-delimited vendor block is
// restored to the published tag and `.forge-kcl/` is removed, mirroring
// the `.forge-pkg` → published-pin swap semantics.
//
// All kcl.mod surgery lives in internal/kclvendor (shared with the
// scaffolder, which uses the same primitives so dev projects are born
// already vendored). This file owns pipeline orchestration + printing.
package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
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

// stepSyncDevForgeKCL — sibling of stepSyncDevForgePkg. Keep the
// project's kcl.mod forge-module dependency consistent with how this
// forge binary was built: dev builds vendor the embedded KCL module
// into .forge-kcl/ and point the dependency there; release builds
// restore the published tag and drop the vendor dir. Best-effort:
// failure warns and the pipeline continues (--strict promotes to
// fatal), matching the forge/pkg sync step.
func stepSyncDevForgeKCL(ctx *pipelineContext) error {
	return ctx.warnOrFail("forge KCL module dev-mode vendor sync", syncDevForgeKCL(ctx.ProjectDir))
}

// syncDevForgeKCL implements the sync. Split from the step for direct
// testing.
func syncDevForgeKCL(projectDir string) error {
	if buildinfo.PkgVersion() != "" {
		return unvendorForgeKCL(projectDir)
	}

	// Dev build: ensure every managed kcl.mod resolves the module from
	// the vendored copy, then materialize/refresh that copy. A kcl.mod
	// forge cannot prove it understands is warned about, never edited.
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

// unvendorForgeKCL is the release-build direction: restore the
// published tag in every marker-owned vendor block, then remove
// .forge-kcl/ once nothing references it anymore. A kcl.mod that still
// mentions the vendor dir in a shape forge does not manage blocks the
// removal (never delete a directory someone still resolves against)
// and gets a warning instead.
func unvendorForgeKCL(projectDir string) error {
	restored := false
	for _, modPath := range kclModCandidates(projectDir) {
		res, err := kclvendor.RestorePublishedDep(modPath)
		if err != nil {
			return err
		}
		if res.Changed {
			restored = true
			fmt.Printf("  ✅ %s: restored the published forge KCL module reference (tag %s)\n",
				projectRelPath(projectDir, modPath), kclvendor.PublishedTag)
		}
	}
	if !kclvendor.Present(projectDir) {
		return nil
	}
	for _, modPath := range kclModCandidates(projectDir) {
		data, err := os.ReadFile(modPath)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), kclvendor.VendorDirName) {
			fmt.Fprintf(os.Stderr,
				"⚠️  Warning: %s still references %s/ in a shape forge does not manage — leaving the vendor directory in place; switch the dependency to the published tag (`%s`) and delete %s/ to finish un-vendoring.\n",
				projectRelPath(projectDir, modPath), kclvendor.VendorDirName,
				kclvendor.PublishedDepLine, kclvendor.VendorDirName)
			return nil
		}
	}
	if err := kclvendor.Remove(projectDir); err != nil {
		return err
	}
	if restored {
		fmt.Printf("  ✅ Removed %s/ (project is back on the published forge KCL module)\n", kclvendor.VendorDirName)
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
