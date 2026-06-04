// File: internal/config/exclude.go
//
// MatchExclude is the single source of truth for `contracts.exclude`
// matching across forge. Previously the same three-rule matcher was
// hand-copied into:
//
//   - internal/config/config.go      (ContractsConfig.IsExcluded)
//   - internal/linter/contract/exclude.go (the `go vet`-driven shim)
//   - internal/linter/forgeconv/internal_pkg_contract.go (inline closure)
//
// All three implemented the SAME high-level rule (equality | "/"-suffix
// | substring) but had silently diverged in two places:
//
//  1. Empty-pattern handling. config.go did NOT skip "" entries, so a
//     stray empty line in forge.yaml's contracts.exclude: would make
//     strings.Contains("", "") fire and silently exclude every
//     package. The other two filtered "" out before matching. The
//     shared helper here adopts the SKIP-EMPTY behaviour — matching
//     everything on an empty entry is never what a user wants, and the
//     YAML loader trims values but doesn't drop empties.
//
//  2. Slash normalisation. forgeconv called filepath.ToSlash(pat) before
//     comparing; the other two used the pattern verbatim. On Linux/macOS
//     this is a no-op; on Windows the pattern from forge.yaml is already
//     in slash form (it's a YAML literal) while the pkgPath the analyzer
//     produces was filepath.Join'd. The shared helper normalises BOTH
//     sides to forward slashes before matching, eliminating the
//     Windows-only divergence without changing behaviour on POSIX.
//
// In code-comment terms, the helper is the MOST-DEFENSIVE variant: it
// drops the latent empty-pattern bug from config.go and adopts
// forgeconv's portability fix.

package config

import (
	"path/filepath"
	"strings"
)

// MatchExclude reports whether pkgPath matches any of the configured
// exclude patterns. The matching rule for each pattern is the union of:
//
//   - equality      (pattern == pkgPath)
//   - "/"-suffix    (pkgPath ends with "/"+pattern)
//   - substring     (pattern appears anywhere in pkgPath)
//
// Empty patterns are skipped — see the package doc for why. Both
// pkgPath and patterns are normalised to forward-slash form before
// comparison so the helper behaves the same on every OS.
//
// The substring rule is intentionally lenient: it's the only one that
// catches the common shorthand `mypkg` (rather than `internal/mypkg`),
// at the cost of also matching unrelated packages whose path happens
// to embed the pattern as a substring. Project owners who hit the
// over-match can spell the path out in full — config and linter
// surfaces all now agree on what "in full" means.
func MatchExclude(patterns []string, pkgPath string) bool {
	pkgPath = filepath.ToSlash(pkgPath)
	for _, pattern := range patterns {
		pattern = filepath.ToSlash(pattern)
		if pattern == "" {
			continue
		}
		if pattern == pkgPath ||
			strings.HasSuffix(pkgPath, "/"+pattern) ||
			strings.Contains(pkgPath, pattern) {
			return true
		}
	}
	return false
}
