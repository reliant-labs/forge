package codegen

import (
	"os"

	"github.com/reliant-labs/forge/internal/checksums"
)

// Two writer tiers, made legible.
//
// Generated output splits into two ownership tiers, and the tier dictates the
// write mechanism:
//
//   - forge-owned: files forge stamps and re-stamps on every `forge generate`
//     (wire_gen.go, bootstrap.go, *_gen.go, the rendered CI/deploy configs).
//     These go through checksums.WriteGeneratedFile so the checksum ledger can
//     later tell "stale codegen forge may overwrite" apart from "a user edit
//     forge must preserve". Always forced here (force=true) — the generate
//     pipeline owns these paths.
//
//   - user-scaffold: one-time starting points forge writes once and then
//     leaves to the user (service.go, handlers.go, authorizer.go, the local
//     k3d ingress override, etc.). These are raw os.WriteFile — deliberately
//     NOT checksum-tracked, because forge is not the steward of their later
//     contents.
//
// The distinction used to be implicit in the choice of call (WriteGeneratedFile
// vs os.WriteFile). These thin wrappers name the intent at each call site
// without changing which files are checksummed.

// writeForgeOwned writes a forge-owned, checksum-tracked file via the checksum
// ledger (force=true — the generate pipeline owns this path).
func writeForgeOwned(root, relPath string, content []byte, cs *checksums.FileChecksums) error {
	_, err := checksums.WriteGeneratedFile(root, relPath, content, cs, true)
	return err
}

// writeUserScaffold writes a one-time user-scaffold file. Deliberately not
// checksum-tracked: forge writes it once and leaves later edits to the user.
func writeUserScaffold(path string, content []byte) error {
	return os.WriteFile(path, content, 0o644)
}
