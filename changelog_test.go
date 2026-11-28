package forge_test

import (
	"bytes"
	"os"
	"testing"

	forge "github.com/reliant-labs/forge"
)

// `forge project upgrade` reads release notes from the embedded copy, so the
// embed must be the CHANGELOG.md at the repository root, byte for byte, and
// must carry the Keep-a-Changelog header the upgrade command parses.
func TestChangelogIsTheEmbeddedRepositoryChangelog(t *testing.T) {
	onDisk, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	got := forge.Changelog()
	if !bytes.Equal(got, onDisk) {
		t.Errorf("Changelog() is %d bytes, CHANGELOG.md is %d: the embed is not the file", len(got), len(onDisk))
	}
	if !bytes.HasPrefix(got, []byte("# Changelog\n")) {
		t.Errorf("Changelog() starts %q, want the \"# Changelog\" header", got[:min(len(got), 20)])
	}
	if !bytes.Contains(got, []byte("## [Unreleased]")) {
		t.Error("Changelog() has no [Unreleased] section")
	}
}
