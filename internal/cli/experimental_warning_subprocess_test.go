// File: internal/cli/experimental_warning_subprocess_test.go
//
// The experimental-features warning is a nudge aimed at a person: "the
// schema under features.experimental may break between versions". Forge
// also runs PARTS OF ITSELF as subprocesses — protoc-gen-forge is a
// hidden subcommand that buf spawns once per proto file — and each of
// those is a fresh process, so the once-per-process atomic guard on the
// warning does not cover them.
//
// (operators was the original fixture here; it has since GRADUATED to a
// top-level feature flag, so the fixture uses strict_wiring — one of the
// two features that are still genuinely experimental.)
//
// The result, measured in control-plane (features.experimental: ingress,
// external_builds, operators), was 27 copies of
//
//	warning: experimental: ingress, external_builds, operators (--silence-experimental to hide)
//
// in one `forge generate`: one the user earned, and 26 from plugin
// subprocesses nobody invoked. That is the warning telling the truth in
// a way that makes the real output unreadable, which is a forge defect
// rather than a project one — the fix belongs here, not behind
// --silence-experimental in every project that turns a feature on.

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
)

// TestProtocGenForgeIsMachineInvoked pins the annotation on the one
// command that has it today. A forge-spawned subprocess must not emit
// interactive nudges.
func TestProtocGenForgeIsMachineInvoked(t *testing.T) {
	if !machineInvoked(newProtocGenForgeCmd()) {
		t.Error("protoc-gen-forge is spawned by buf once per proto file, " +
			"so it must carry machineInvokedAnnotation — otherwise every " +
			"user-facing nudge in PersistentPreRun is multiplied by the " +
			"number of protos in the project")
	}
}

// TestPinMismatchIsWarnedOnce: one condition, one warning.
//
// `forge generate` in the forge repo (whose forge.yaml pins an old
// `+dirty` pseudo-version) printed BOTH of these, back to back:
//
//	⚠️  forge.yaml pins forge_version v0.0.4-…+dirty; this is forge v0.1.23. Generating with it anyway — …
//	⚠️  forge.yaml declares forge_version: v0.0.4-…+dirty but binary is v0.1.23. Run 'forge project upgrade' to migrate.
//
// Same fact, same advice, two spellings — which reads as two problems
// and teaches the user that forge warnings are noise. checkPkgCompat no
// longer duplicates what stepAnnounceProject already says, so a grep for
// the emitting call sites must find exactly one.
func TestPinMismatchIsWarnedOnce(t *testing.T) {
	pkg, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var emitters []string
	for _, e := range pkg {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// The distinguishing phrase of the PIN-vs-RUNNING-BINARY
		// warning. Not merely "pins forge_version": generate_forge_pin.go
		// legitimately reports a different comparison (go.mod's require
		// vs the pin), and collapsing those two would be wrong.
		if strings.Contains(string(src), "generate does not re-pin the project") {
			emitters = append(emitters, name)
		}
	}
	if len(emitters) != 1 {
		t.Errorf("pin-mismatch warning is composed in %d files (%v), want exactly 1 — "+
			"two functions saying the same thing is how one condition became two "+
			"warnings in every generate against a pinned project", len(emitters), emitters)
	}
}

// TestUserCommandsAreNotMachineInvoked guards the other direction: the
// annotation must stay scoped to subprocesses. Silencing a user command
// would hide the warning from the person it exists for.
func TestUserCommandsAreNotMachineInvoked(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{{"generate"}, {"lint"}, {"build"}} {
		cmd, _, err := root.Find(path)
		if err != nil {
			continue // command set varies; absence is not this test's concern
		}
		if machineInvoked(cmd) {
			t.Errorf("`forge %s` is a user command and must NOT be "+
				"machine-invoked: the experimental warning is for the "+
				"person who typed it", strings.Join(path, " "))
		}
	}
}

// TestExperimentalWarningSilencedForMachineInvoked runs the REAL root
// pre-run against a real project with experimental features on, so the
// annotation is proven to suppress output rather than merely to exist.
//
// Before the fix this fails with the warning in the buffer: the pre-run
// consulted only --silence-experimental and FORGE_SILENCE_EXPERIMENTAL,
// so a plugin subprocess reached emitExperimentalWarning and printed.
func TestExperimentalWarningSilencedForMachineInvoked(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "forge.yaml"),
		"name: demo\nmodule_path: github.com/example/demo\n"+
			"database:\n  driver: postgres\n  migrations_dir: db/migrations\n"+
			"ci:\n  provider: github\n"+
			"k8s:\n  kcl_dir: deploy/kcl\n"+
			"features:\n  experimental:\n    strict_wiring: true\n")

	run := func(cmd *cobra.Command) string {
		// Each subprocess is a fresh process in production, so the
		// once-per-process guard must not be what makes this pass.
		experimentalWarningEmitted.Store(false)
		t.Cleanup(func() { experimentalWarningEmitted.Store(false) })

		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		root := NewRootCmd()
		root.SetErr(&stderr)
		// -C is how the pre-run itself learns where the project is; it
		// calls SetProjectDir from this flag, so setting the flag is the
		// only way to point it at the fixture.
		if err := root.PersistentFlags().Set("project-dir", dir); err != nil {
			t.Fatalf("set --project-dir: %v", err)
		}
		t.Cleanup(func() { _ = cmdutil.SetProjectDir("") })

		if err := root.PersistentPreRunE(cmd, nil); err != nil {
			t.Fatalf("PersistentPreRunE: %v", err)
		}
		return stderr.String()
	}

	// Sanity: an ordinary user command in this project DOES warn.
	// Without this the test would pass against a forge that had simply
	// stopped warning at all.
	if got := run(&cobra.Command{Use: "generate"}); !strings.Contains(got, "experimental") {
		t.Fatalf("a user command in a project with experimental features must warn, got %q", got)
	}

	if got := run(newProtocGenForgeCmd()); got != "" {
		t.Errorf("protoc-gen-forge is a buf subprocess spawned once per proto "+
			"file and must print nothing, got %q", got)
	}
}
