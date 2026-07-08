package codegen

import (
	"os"
	"path/filepath"

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

// writeForgeScaffoldOnce writes a scaffold-once, USER-OWNED file addressed by
// (root, relPath): forge emits it exactly ONCE, when it does not yet exist, and
// then never regenerates or overwrites it. This is the write tier for the
// command tree + component WIRING (cmd/<bin>/main.go, internal/app/compose.go,
// internal/app/lifecycle.go, and the per-worker/operator subcommands): once
// scaffolded, the file is owned code the user hand-maintains (adding a component
// is a hand-edit / `forge add` append, never a re-derivation). It creates parent
// dirs, is deliberately NOT checksum-tracked, and returns true when it wrote a
// fresh file (false when one already existed).
func writeForgeScaffoldOnce(root, relPath string, content []byte) (bool, error) {
	abs := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return false, err
	}
	return writeUserScaffoldIfAbsent(abs, content)
}

// writeUserScaffoldIfAbsent writes a one-time user-scaffold file ONLY when it
// does not already exist. Once forge has scaffolded it, the user owns the
// contents and a later `forge generate` must never clobber their edits — so an
// existing file is left untouched (returns false, nil). Returns true when it
// wrote a fresh file. Like writeUserScaffold, it is deliberately not
// checksum-tracked.
func writeUserScaffoldIfAbsent(path string, content []byte) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil // already present — user owns it, leave it be
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return false, err
	}
	return true, nil
}
