package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// TestDecideForgeCompat is the decision table. It is a pure function, so
// every row is deterministic — no module graph, no network, no toolchain.
func TestDecideForgeCompat(t *testing.T) {
	const binary = "v0.1.16"
	cases := []struct {
		name    string
		binary  string
		project string
		raw     string // buildinfo.Version(): what the binary calls itself
		local   bool
		want    compatVerdict
	}{
		{"pin equals binary", binary, "v0.1.16", "", false, compatOK},
		{"pin newer than binary", binary, "v0.1.17", "", false, compatOK},
		{"pin older than binary", binary, "v0.1.15", "", false, compatStalePin},
		{"pin older by patch", binary, "v0.1.16-rc.1", "", false, compatStalePin},

		// A pseudo-version from `go install ...@main` is a real, orderable
		// version: it sorts after the tag it builds on and before the next
		// one, which is exactly what commit-pinning mode needs.
		{"binary is a pseudo-version, pin is the tag it follows",
			"v0.1.16-0.20260916085636-c01e07ec6ef2", "v0.1.15", "", false, compatStalePin},
		{"binary is a pseudo-version, pin is the next tag",
			"v0.1.16-0.20260916085636-c01e07ec6ef2", "v0.1.16", "", false, compatOK},

		// The regression this design exists for: an unreleasable binary
		// (dirty tree, plain `go build`) against a published pin.
		{"unreleasable binary, published pin", "", "v0.1.15", "", false, compatUnreleasableNoBridge},
		{"unreleasable binary, newer published pin", "", "v9.9.9", "", false, compatUnreleasableNoBridge},

		// A local resolution is the supported pairing for an unreleasable
		// binary, and is fine for a released one too.
		{"unreleasable binary, bridged", "", "", "", true, compatOK},
		{"released binary, bridged", binary, "", "", true, compatOK},

		// A BUILD THAT CANNOT VOUCH FOR ITSELF, against a project that
		// already resolves to exactly it. InstallableVersion() is "" for any
		// working-tree build, but the project's own `go list -m` answering
		// with this version proves the proxy served it — so refusing would be
		// a false positive whose message names one version as both "what this
		// forge is" and "the published version" it conflicts with.
		{"unreleasable binary, project resolves to this very build",
			"", "v0.1.17-0.20260918232310-3cfebb459c15", "v0.1.17-0.20260918232310-3cfebb459c15", false, compatOK},
		// Same shape, DIFFERENT commit: no proof, so the refusal stands.
		{"unreleasable binary, project resolves to a different build",
			"", "v0.1.17-0.20260918232310-3cfebb459c15", "v0.1.17-0.20260918144654-6b3a0a0050cd", false, compatUnreleasableNoBridge},

		// Unknown beats guessing.
		{"unknown project version", binary, "", "", false, compatOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideForgeCompat(c.binary, c.raw, c.project, c.local); got != c.want {
				t.Errorf("decideForgeCompat(%q, %q, %q, %v) = %v, want %v",
					c.binary, c.raw, c.project, c.local, got, c.want)
			}
		})
	}
}

// TestCheckPkgCompat_NoForgeDependency is a no-op (nil) when the project
// doesn't depend on forge at all — nothing to check, not our error to raise.
func TestCheckPkgCompat_NoForgeDependency(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), "module example.com/app\n\ngo 1.24\n")
	if err := checkPkgCompat(dir); err != nil {
		t.Fatalf("expected nil for a project without forge, got %v", err)
	}
}

// TestCheckPkgCompat_RetiredPkgModuleExplainsTheAmbiguity: the retired
// forge/pkg submodule is not merely absent, it CONFLICTS — both it and the
// merged forge provide github.com/reliant-labs/forge/pkg/*, so a graph
// holding both answers every such import with "ambiguous import: found
// package ... in multiple modules", once per import, naming no cause and no
// fix. This is the single most confusing state the migration can produce, so
// the refusal has to name it.
func TestCheckPkgCompat_RetiredPkgModuleExplainsTheAmbiguity(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "go.mod"), strings.Join([]string{
		"module example.com/app",
		"",
		"go 1.24",
		"",
		"require github.com/reliant-labs/forge/pkg v0.1.15",
		"",
	}, "\n"))

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected an error for a project still requiring the retired forge/pkg module")
	}
	msg := err.Error()
	for _, want := range []string{
		"ambiguous",            // the error the user would otherwise face
		"-droprequire",         // the literal fix for a DIRECT requirement
		"Import paths did NOT", // the reassurance that matters most
		"No files were changed",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("retired-module error must contain %q, got:\n%s", want, msg)
		}
	}
}

// TestCheckPkgCompat_UnreleasableBuildNamesTheBridge is the exact failure a
// control-plane `forge generate` hit: a forge built from a dirty local tree,
// generating into a project pinned to a published forge. The old code let it
// through and died in validate with `undefined: testkit.StubNotConfigured`.
//
// The refusal must name the go.work bridge, because that is the supported way
// to generate with an unreleased forge — and it is what forge's own
// project_pkgdep.go documents.
func TestCheckPkgCompat_UnreleasableBuildNamesTheBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("resolves a module graph — skipped under -short")
	}
	dir := t.TempDir()
	writeForgeConsumer(t, dir, "v0.1.15")

	// A dirty local build: no proxy-resolvable version.
	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
	buildinfo.Set("v0.1.16-0.20260916085636-c01e07ec6ef2+dirty", "", "c01e07ec6ef2")

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected an error: an unreleasable forge generating into a proxy-pinned project")
	}
	msg := err.Error()
	for _, want := range []string{
		"go work use",           // the literal fix
		"No files were changed", // the tree is intact
		"v0.1.15",               // what the project resolves
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("unreleasable-build error must contain %q, got:\n%s", want, msg)
		}
	}
}

// TestCheckPkgCompat_BridgedProjectPasses is the mirror: the same
// unreleasable binary is fine once the project resolves forge from source,
// because then there is no published version for the generated code to
// outrun.
func TestCheckPkgCompat_BridgedProjectPasses(t *testing.T) {
	if testing.Short() {
		t.Skip("resolves a module graph — skipped under -short")
	}
	dir := t.TempDir()
	writeForgeConsumer(t, dir, "v0.1.15")
	// Replace forge with a local stub: the "bridged" resolution.
	stub := filepath.Join(dir, "forge-stub")
	mustMkdirAllT(t, stub)
	mustWrite(t, filepath.Join(stub, "go.mod"), "module github.com/reliant-labs/forge\n\ngo 1.24\n")
	appendTo(t, filepath.Join(dir, "go.mod"),
		"\nreplace github.com/reliant-labs/forge => ./forge-stub\n")

	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
	buildinfo.Set("v0.1.16-0.20260916085636-c01e07ec6ef2+dirty", "", "c01e07ec6ef2")

	if err := checkPkgCompat(dir); err != nil {
		t.Fatalf("expected nil for a project bridged to local forge source, got %v", err)
	}
}

// TestCheckPkgCompat_NamesTheRunningToolchain is the toolchain-mismatch
// reproduction, carried over from the symbol-probe era because the diagnosis
// it protects is unchanged.
//
// Two forge builds can sit on one PATH — `forge` (standalone) and `reliant
// forge` (compiled into reliant, and only rebuilt when reliant is). They can
// disagree about the same tree: one `generate` succeeds while the other
// fails. An error naming only the version and telling the user to bump it is
// the WRONG fix when the real cause is that a different forge build generated
// the tree — the agent that hit this concluded the project was unfixable and
// rebuilt it from scratch.
//
// The refusal must therefore be a runbook: which forge build is running, how
// it was invoked, and the literal command to fix it.
func TestCheckPkgCompat_NamesTheRunningToolchain(t *testing.T) {
	if testing.Short() {
		t.Skip("resolves a module graph — skipped under -short")
	}
	dir := t.TempDir()
	writeForgeConsumer(t, dir, "v0.1.15")

	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
	buildinfo.Set("v0.9.9", "", "deadbeef") // a released build, newer than the pin

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected a stale-pin error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "forge build:") {
		t.Errorf("error must name the running forge build, got:\n%s", msg)
	}
	if !strings.Contains(msg, "invoked as:") {
		t.Errorf("error must say how forge was invoked (standalone vs embedded), got:\n%s", msg)
	}
	if !strings.Contains(msg, "reliant forge") || !strings.Contains(msg, "toolchain") {
		t.Errorf("error must raise the two-toolchain possibility, got:\n%s", msg)
	}
	if !strings.Contains(msg, "go get") {
		t.Errorf("error must give the literal fix command, got:\n%s", msg)
	}
}

// TestCheckPkgCompat_PseudoVersionFixNamesTheCommitAndTheBridge: when the
// generating binary is a pseudo-version, `go get <that version>` resolves ONLY
// if the commit is pushed. Asserting it unconditionally would send the reader
// to "unknown revision" — the same shape of wrong advice this check exists to
// prevent — so the refusal names the commit and offers the source bridge.
func TestCheckPkgCompat_PseudoVersionFixNamesTheCommitAndTheBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("resolves a module graph — skipped under -short")
	}
	dir := t.TempDir()
	writeForgeConsumer(t, dir, "v0.1.15")

	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
	buildinfo.Set("v0.1.16-0.20260916100640-e69bb5851a27", "", "e69bb5851a27")

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected a stale-pin error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "e69bb5851a27") {
		t.Errorf("must name the commit the pseudo-version points at, got:\n%s", msg)
	}
	if !strings.Contains(msg, "PUSHED") {
		t.Errorf("must say the ref only resolves once the commit is pushed, got:\n%s", msg)
	}
	if !strings.Contains(msg, "go work use") {
		t.Errorf("must offer the source bridge as the alternative, got:\n%s", msg)
	}
}

// A RELEASE tag carries no such caveat: it is on the proxy or it is not a
// release. Adding the note there would be noise on the common path.
func TestCheckPkgCompat_ReleaseVersionFixHasNoPseudoCaveat(t *testing.T) {
	if testing.Short() {
		t.Skip("resolves a module graph — skipped under -short")
	}
	dir := t.TempDir()
	writeForgeConsumer(t, dir, "v0.1.15")

	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
	buildinfo.Set("v0.9.9", "", "deadbeef")

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected a stale-pin error")
	}
	if strings.Contains(err.Error(), "PUSHED") {
		t.Errorf("a release tag must not carry the pseudo-version caveat, got:\n%s", err)
	}
}

// TestCheckPkgCompat_ReportsGeneratingBuildMismatch pins the recorded-build
// comparison: when the tree records the forge build that generated it and a
// DIFFERENT build is now running, the refusal must say so explicitly rather
// than blaming the version pin alone.
func TestCheckPkgCompat_ReportsGeneratingBuildMismatch(t *testing.T) {
	if testing.Short() {
		t.Skip("resolves a module graph — skipped under -short")
	}
	dir := t.TempDir()
	writeForgeConsumer(t, dir, "v0.1.15")
	writeGeneratingBuild(t, dir, "v0.0.1 (embedded in github.com/reliant-labs/reliant v1.5.1)")

	t.Cleanup(func() { buildinfo.Set("dev", "", "unknown") })
	buildinfo.Set("v0.9.9", "", "deadbeef")

	err := checkPkgCompat(dir)
	if err == nil {
		t.Fatal("expected a compat error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "generated by:") {
		t.Errorf("error must report the build that generated the tree, got:\n%s", msg)
	}
	if !strings.Contains(msg, "v0.0.1") {
		t.Errorf("error must quote the recorded generating build, got:\n%s", msg)
	}
}

// writeGeneratingBuild seeds the .forge/ record of which forge build last
// generated the tree, so the mismatch path can be exercised deterministically.
func writeGeneratingBuild(t *testing.T, dir, identity string) {
	t.Helper()
	path := generatingBuildPath(dir)
	mustMkdirAllT(t, filepath.Dir(path))
	mustWrite(t, path, identity+"\n")
}

// writeForgeConsumer lays down a module requiring forge at the given version,
// with a go.sum-free graph: `go list -m` reports the requirement from go.mod
// without fetching anything, which is all the check reads.
func writeForgeConsumer(t *testing.T, dir, forgeVersion string) {
	t.Helper()
	mustWrite(t, filepath.Join(dir, "go.mod"), strings.Join([]string{
		"module example.com/app",
		"",
		"go 1.24",
		"",
		"require github.com/reliant-labs/forge " + forgeVersion,
		"",
	}, "\n"))
	mustWrite(t, filepath.Join(dir, "doc.go"), "package app\n")
}

func mustMkdirAllT(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, extra string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(data)+extra)
}
