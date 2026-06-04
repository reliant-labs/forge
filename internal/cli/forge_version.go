package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// forgeVersionMismatchWarning returns a warning string when the project's
// pinned forge_version (yamlVersion) doesn't match the running binary's
// version (binaryVersion), or empty if they agree / can't be compared.
//
// Three cases:
//
//  1. Project is missing forge_version entirely (legacy / pre-baseline) —
//     emit the "no forge_version declared" nudge so the user knows to run
//     `forge upgrade` to set a baseline.
//  2. Project has a forge_version that doesn't equal the binary version —
//     emit the migration warning.
//  3. Either side is "dev" / unset / empty / a Go pseudoversion in a way
//     we can't compare — stay silent. We don't want to spam during local
//     development against a tip-of-tree forge build.
func forgeVersionMismatchWarning(yamlVersion, binaryVersion string) string {
	yamlVersion = strings.TrimSpace(yamlVersion)
	binaryVersion = strings.TrimSpace(binaryVersion)

	// Case 3: silence when the binary version isn't a real release.
	// During local forge development the binary reports "dev" / "(devel)";
	// `go install` of an un-tagged commit produces a Go pseudoversion
	// (`v0.0.0-<timestamp>-<commit>`); both are noise to dogfood owners.
	if isUnreleasedBinaryVersion(binaryVersion) {
		return ""
	}

	// Case 1: legacy project, no forge_version pinned. Treat as "0.0.0"
	// per EffectiveForgeVersion semantics.
	if yamlVersion == "" {
		return fmt.Sprintf("⚠️  no forge_version declared in forge.yaml — run '%s upgrade' to set baseline (binary is %s).", Name(), binaryVersion)
	}

	if yamlVersion == binaryVersion {
		return ""
	}

	return fmt.Sprintf("⚠️  forge.yaml declares forge_version: %s but binary is %s. Run '%s upgrade' to migrate.", yamlVersion, binaryVersion, Name())
}

// isUnreleasedBinaryVersion reports whether the binary's reported version
// string corresponds to a non-release build that should suppress the
// version-pin warning. Covers:
//   - empty / unknown
//   - "dev" (local make-build sentinel)
//   - "(devel)" (Go's runtime.BuildInfo placeholder for go-run / go-test)
//   - any Go pseudoversion (`v0.0.0-…`) produced by `go install` of an
//     un-tagged commit.
func isUnreleasedBinaryVersion(v string) bool {
	switch v {
	case "", "dev", "(devel)":
		return true
	}
	return strings.HasPrefix(v, "v0.0.0-")
}

// versionWarnSentinelPath returns the per-binary-path sentinel file used
// to track whether the version-pin warning has already fired this shell
// session. Hashed so multiple forge binaries on the same machine (a tagged
// release + a worktree dev build) don't share state.
//
// Note: a "session" here is approximated by `$TMPDIR` — on macOS each
// shell login gets its own tmp dir, and on Linux $TMPDIR usually defaults
// to /tmp (process-shared). The sentinel-per-binary-path hash keeps the
// warning local to one forge install; the trade-off (warning fires once
// per host /tmp lifetime rather than literally once per shell) is
// acceptable for a low-volume CLI nudge.
func versionWarnSentinelPath(binaryPath string) string {
	sum := sha256.Sum256([]byte(binaryPath))
	return filepath.Join(os.TempDir(), "forge-version-warned-"+hex.EncodeToString(sum[:8]))
}

// shouldEmitVersionWarn returns whether the version-pin warning should
// be printed for this invocation, given the resolved warning string
// and the path to the running forge binary.
//
// The sentinel is honored ONLY for non-silenced binaries (Case 1/Case 2
// above). When isUnreleasedBinaryVersion returns true the caller already
// gets an empty string and shouldn't reach this gate.
func shouldEmitVersionWarn(warning, binaryPath string) bool {
	if warning == "" {
		return false
	}
	sentinel := versionWarnSentinelPath(binaryPath)
	if _, err := os.Stat(sentinel); err == nil {
		// Sentinel exists → we've already warned this session.
		return false
	}
	// Best-effort touch. If the create fails (read-only TMPDIR, etc.)
	// fall through and emit the warning; we'd rather over-warn than
	// silently swallow a real migration nudge.
	if f, err := os.Create(sentinel); err == nil {
		_ = f.Close()
	}
	return true
}
