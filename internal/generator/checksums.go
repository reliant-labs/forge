// Package generator: checksums shim.
//
// The canonical FileChecksums type and helpers live in
// internal/checksums so that internal/codegen (per-feature emitters) can
// also import them without an import cycle (generator already imports
// codegen). Existing call sites in the cli/ and generator/ packages
// continue to work via the re-exports below.
package generator

import (
	"github.com/reliant-labs/forge/internal/checksums"
)

// checksumFile is the on-disk path of the checksum file relative to the
// project root. Mirrored from internal/checksums for tests in this
// package that exercise file-level state (LoadChecksums round-trips).
const checksumFile = ".forge/checksums.json"

// testHistoryLimit mirrors the history-bound constant in internal/checksums
// so tests in this package can assert on the bound without exporting the
// private constant from the canonical location. Kept in sync by hand —
// flip both sites when changing the cap.
const testHistoryLimit = 20

// FileChecksums is re-exported as a type alias from internal/checksums.
type FileChecksums = checksums.FileChecksums

// FileChecksumEntry is re-exported as a type alias from internal/checksums.
type FileChecksumEntry = checksums.FileChecksumEntry

// LoadChecksums loads the checksum file from the project root.
func LoadChecksums(root string) (*FileChecksums, error) {
	return checksums.Load(root)
}

// SaveChecksums writes the checksum file in the structured shape.
func SaveChecksums(root string, cs *FileChecksums) error {
	return checksums.Save(root, cs)
}

// HashContent returns the sha256 hex digest of content.
func HashContent(content []byte) string {
	return checksums.Hash(content)
}

// WriteGeneratedFile writes content to a file and records its checksum.
// See checksums.WriteGeneratedFile for full semantics.
func WriteGeneratedFile(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	return checksums.WriteGeneratedFile(root, relPath, content, cs, force)
}

// WriteGeneratedFileTier1 writes a Tier-1 file and tags the checksum as
// Tier-1. See checksums.WriteGeneratedFileTier1 for full semantics.
func WriteGeneratedFileTier1(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	return checksums.WriteGeneratedFileTier1(root, relPath, content, cs, force)
}

// WriteGeneratedFileTier2 writes a Tier-2 file and tags the checksum as
// Tier-2. See checksums.WriteGeneratedFileTier2 for full semantics.
func WriteGeneratedFileTier2(root, relPath string, content []byte, cs *FileChecksums, force bool) (bool, error) {
	return checksums.WriteGeneratedFileTier2(root, relPath, content, cs, force)
}

// Tier1DriftEntry is re-exported as a type alias from internal/checksums.
type Tier1DriftEntry = checksums.Tier1DriftEntry
