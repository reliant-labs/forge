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
	"sync"

	"kcl-lang.io/kpm/pkg/client"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclvendor"
)

// staleWarnOnce keeps the vendored-module drift notice to one line per
// process. A single command renders every env in a loop, and repeating
// the same warning three times reads like three problems.
var staleWarnOnce sync.Once

// warnIfVendorStale tells the user, once, when the vendored forge KCL
// module under workDir was materialized by a different forge version
// than the one rendering now.
//
// This is the counterweight to vendoring being the only resolution
// mechanism: the module refreshes when `forge generate` runs and at no
// other time, so a project can render against a copy an older forge
// wrote. Without this the symptom of that drift is a schema error that
// names a field rather than the stale module, and the fix (`forge
// generate`) is not obvious from it.
func warnIfVendorStale(workDir string) {
	stale, stamped := kclvendor.Stale(workDir)
	if !stale {
		return
	}
	staleWarnOnce.Do(func() {
		was := stamped
		if was == "" {
			was = "an older forge (unstamped)"
		}
		fmt.Fprintf(os.Stderr,
			"⚠️  %s/ was materialized by %s; this is forge %s. Run `forge generate` to refresh the vendored KCL module.\n",
			kclvendor.VendorDirName, was, buildinfo.Version())
	})
}

// Run renders the KCL at source — a package directory or a single .k
// file — and returns the raw JSON result.
//
// workDir is the process cwd KCL resolves relative reads against, and the
// directory kpm resolves the package's kcl.mod dependencies from (including
// the relative `.forge-kcl/` vendor path forge points every project at), so
// it is part of the contract.
// dArgs are `-D key=value` top-level option assignments (e.g. "env=dev").
// kpm progress/diagnostics go to stderr.
func Run(workDir, source string, dArgs []string) ([]byte, error) {
	// Make kcl_plugin.forge (resolve_port, …) available. Idempotent;
	// the registry is process-global.
	kclplugin.Register()

	warnIfVendorStale(workDir)

	c, err := client.NewKpmClient()
	if err != nil {
		return nil, fmt.Errorf("kpm client: %w", err)
	}
	res, err := c.Run(
		client.WithRunSourceUrl(source),
		client.WithWorkDir(workDir),
		client.WithArguments(dArgs),
		client.WithLogger(os.Stderr),
	)
	if err != nil {
		return nil, fmt.Errorf("kpm run %s: %w", source, err)
	}
	return []byte(res.GetRawJsonResult()), nil
}
