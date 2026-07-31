// File: internal/codegen/internal_packages.go
//
// Internal-package discovery — the ONE answer to "which contract packages
// does this project have?".
//
// forge.yaml used to carry a `packages:` list alongside this walk. It was
// never a codegen input: the bootstrap/injector pass has always walked
// internal/ for contract.go, so a project whose `packages:` block was
// deleted still got its packages imported, field-declared, constructed and
// middleware-wrapped in internal/app/compose.go. The list only steered the
// REPORTERS (`forge project map`, `forge project audit`, the architecture
// doc), which is the worst possible split: the three surfaces whose whole
// job is to tell you what the project contains were the only three reading
// a source the project did not have to keep true. Renaming a package in
// forge.yaml made `forge project graph` report a package that does not
// exist while the one that IS wired went unlisted.
//
// So discovery moved here, where every caller — codegen and reporters
// alike — resolves the same set from the same source: the code.

package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// Internal-package kinds. There are exactly two, and the distinction is
// read off the package's own source rather than a registry: a package
// either claims to be an outbound I/O boundary (`//forge:outbound-io`) or
// it does not. Every contract package is a Service/Deps/New component —
// what varies is whether it calls OUT, which is the only part a rule can
// check and the only part a reader of `forge project map` needs.
const (
	// InternalPackageService is the default: a Service/Deps/New contract
	// package with no boundary claim.
	InternalPackageService = "service"
	// InternalPackageOutboundIO marks a package that claims to call out
	// to a third-party system and serve nothing inbound
	// (`//forge:outbound-io`).
	InternalPackageOutboundIO = "outbound-io"
)

// InternalPackage is one discovered contract package under internal/.
type InternalPackage struct {
	// Name is the module-relative path under internal/, in nested form:
	// "cache", "mcp/database".
	Name string
	// Dir is the package's on-disk directory.
	Dir string
	// Type is one of the InternalPackage* constants.
	Type string
}

// DiscoverInternalPackages walks internal/ and returns every directory
// holding a contract.go, in stable (walk) order.
//
// Two opt-outs are honored, unioned, exactly as the bootstrap walk has
// always honored them: the forge.yaml `contracts.exclude` list (whole
// subtree) and a package's own `//forge:exclude-contract` directive (that
// package only — descendants may still be components). A testdata/ subtree
// is fixture code, never a package.
//
// The forge.yaml read is best-effort: a project with no (or a broken)
// forge.yaml still has its packages discovered, it just has no excludes to
// apply. The command that loaded the config first has already reported the
// breakage.
//
// A walk error is returned rather than swallowed so a caller emitting code
// can fail the pipeline instead of shipping a partial bootstrap, which
// surfaces much later as a mysterious "undefined: pkg" build error.
func DiscoverInternalPackages(projectDir string) ([]InternalPackage, error) {
	internalDir := filepath.Join(projectDir, "internal")
	if fi, err := os.Stat(internalDir); err != nil || !fi.IsDir() {
		return nil, nil
	}

	var excludes []string
	if cfg, err := config.LoadProjectDir(projectDir); err == nil {
		excludes = cfg.Contracts.Exclude
	}

	var out []InternalPackage
	walkErr := filepath.WalkDir(internalDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		// testdata/ holds fixture contracts, not real packages.
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		// Exclude patterns are matched against the module-relative path
		// ("internal/mcp/database") in forward-slash form regardless of OS,
		// so patterns in forge.yaml stay portable.
		rel, relErr := filepath.Rel(projectDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if config.MatchExclude(excludes, rel) {
			return filepath.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, "contract.go")); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return statErr
		}
		// Per-package opt-out. Do NOT SkipDir: descendants may still be
		// components and carry their own directive; only THIS package opts
		// out.
		if HasExcludeContractDirective(path) {
			return nil
		}
		out = append(out, InternalPackage{
			Name: strings.TrimPrefix(rel, "internal/"),
			Dir:  path,
			Type: internalPackageType(path),
		})
		return nil
	})
	if walkErr != nil && !os.IsNotExist(walkErr) {
		return nil, fmt.Errorf("walking %s: %w", internalDir, walkErr)
	}
	return out, nil
}

// DiscoverInternalPackageNames is DiscoverInternalPackages projected to the
// nested names the bootstrap/injector codegen keys on.
func DiscoverInternalPackageNames(projectDir string) ([]string, error) {
	pkgs, err := DiscoverInternalPackages(projectDir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		names = append(names, p.Name)
	}
	return names, nil
}

// internalPackageType reads the outbound-boundary claim off the package's
// own marker.
func internalPackageType(dir string) string {
	if HasOutboundIODirective(dir) {
		return InternalPackageOutboundIO
	}
	return InternalPackageService
}
