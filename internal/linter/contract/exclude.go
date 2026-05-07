package contract

import (
	"flag"
	"strings"
	"sync"
)

// excludeMu guards excludePatterns.
var excludeMu sync.RWMutex

// excludePatterns is the package-level list of package paths that should be
// skipped by every analyzer in this package. It is populated by SetExcludes
// (called from cmd/contractlint/main.go after parsing the -exclude flag) or
// by the per-analyzer -exclude flag handler below.
var excludePatterns []string

// SetExcludes replaces the package-level exclude list. It's called from
// cmd/contractlint after parsing a top-level -exclude flag, and may also be
// called from tests that want to exercise exclusion behavior.
func SetExcludes(patterns []string) {
	excludeMu.Lock()
	defer excludeMu.Unlock()
	excludePatterns = append(excludePatterns[:0:0], patterns...)
}

// GetExcludes returns a copy of the current exclude list.
func GetExcludes() []string {
	excludeMu.RLock()
	defer excludeMu.RUnlock()
	out := make([]string, len(excludePatterns))
	copy(out, excludePatterns)
	return out
}

// IsExcluded reports whether the given package path matches any configured
// exclude pattern. The matching semantics mirror
// config.ContractsConfig.IsExcluded: an entry matches if it equals the path,
// is a "/"-suffix of the path, or is a substring of the path. We deliberately
// duplicate the logic here so the linter has no dependency on internal/config
// (which would cause an import cycle for the analyzer's own package tests).
func IsExcluded(pkgPath string) bool {
	excludeMu.RLock()
	defer excludeMu.RUnlock()
	for _, pattern := range excludePatterns {
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

// excludeFlag is a flag.Value that accepts comma-separated package paths and
// stores them in the shared excludePatterns slice. It is registered on every
// analyzer so that go vet -vettool=contractlint and direct invocation both
// work even without going through cmd/contractlint's top-level parsing.
type excludeFlag struct{}

func (excludeFlag) String() string {
	return strings.Join(GetExcludes(), ",")
}

func (excludeFlag) Set(value string) error {
	if value == "" {
		SetExcludes(nil)
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	SetExcludes(out)
	return nil
}

// registerExcludeFlag attaches a -exclude flag to the given FlagSet. The flag
// value is shared across analyzers via the package-level excludePatterns
// slice, so setting it on any one analyzer is sufficient.
func registerExcludeFlag(fs *flag.FlagSet) {
	fs.Var(excludeFlag{}, "exclude", "comma-separated list of package paths to skip (matches forge.yaml contracts.exclude)")
}
