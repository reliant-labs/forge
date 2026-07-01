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

	"kcl-lang.io/kpm/pkg/client"

	"github.com/reliant-labs/forge/internal/kclplugin"
)

// Run renders the KCL at source — a package directory or a single .k
// file — and returns the raw JSON result.
//
// workDir is the process cwd KCL's `file.read` resolves against (the
// project root for deploy-as-data main.k files that read
// `deploy/kcl/components_gen.json`), so it is part of the contract.
// dArgs are `-D key=value` top-level option assignments (e.g. "env=dev").
// kpm progress/diagnostics go to stderr.
func Run(workDir, source string, dArgs []string) ([]byte, error) {
	// Make kcl_plugin.forge (resolve_port, …) available. Idempotent;
	// the registry is process-global.
	kclplugin.Register()

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
