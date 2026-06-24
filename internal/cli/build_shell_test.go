package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildtarget"
)

// TestBuildServiceShell_CwdAndSubstitution verifies the de-footgunned
// ShellBuild contract: the command runs from the PROJECT ROOT (spec.ProjectDir,
// NOT the build output dir), and the same ${X} tokens the build_cmd escape
// hatch expands are substituted into the command before exec.
//
// This used a real `sh -c` rather than a mocked runner because
// buildServiceShell intentionally execs inline (it owns its own
// exec.CommandContext, matching buildServiceDocker) — so the faithful test
// drives a real shell and inspects what it observed (its cwd via $PWD, and
// the already-expanded ${PROJECT_DIR}/${IMAGE}/${TAG}/${REGISTRY} tokens).
func TestBuildServiceShell_CwdAndSubstitution(t *testing.T) {
	projDir := t.TempDir()
	// A relative script path under the project root — the exact shape that
	// used to fail with exit 127 when cwd was bin/. Resolving it from the
	// project root is the whole point of the fix.
	scriptsDir := filepath.Join(projDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "build-image.sh"), []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	outFile := filepath.Join(projDir, "observed.txt")
	spec := buildtarget.Spec{
		Service:    "gw",
		Image:      "my-gw",
		Tag:        "v1.2.3",
		Registry:   "reg.example.com",
		ProjectDir: projDir,
		TargetArch: "arm64",
		Env:        "dev",
	}
	// The command exercises: (1) a relative scripts/ path that must resolve
	// against the project root, and (2) ${X} substitution. It records $PWD
	// and the post-substitution token values into observed.txt so the test
	// can assert both behaviors from the shell's point of view.
	// Note: $(pwd) (command substitution) is used rather than $PWD because
	// forge's substitution runs os.Expand over the whole string first, which
	// would consume a bare $PWD (it's an unknown token → empty). $( is left
	// untouched by os.Expand. This $PWD-is-eaten subtlety is itself identical
	// to the build_cmd path — consistency is the point.
	sh := &ShellBuild{Cmd: "sh scripts/build-image.sh > /dev/null && " +
		"printf 'pwd=%s\\nimage=%s\\ntag=%s\\nregistry=%s\\nproject_dir=%s\\narch=%s\\n' " +
		"\"$(pwd)\" '${IMAGE}' '${TAG}' '${REGISTRY}' '${PROJECT_DIR}' '${TARGETARCH}' > observed.txt"}

	res := buildServiceShell(context.Background(), "gw", sh, buildOptions{outputDir: "bin"}, spec)
	if res.err != nil {
		t.Fatalf("buildServiceShell returned error: %v", res.err)
	}

	raw, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read observed.txt (command did not run from project root?): %v", err)
	}
	got := string(raw)

	// macOS /tmp is a symlink to /private/tmp; resolve both sides before
	// comparing the cwd the shell observed against the project root.
	wantPWD, _ := filepath.EvalSymlinks(projDir)
	observedPWD := parseField(t, got, "pwd")
	gotPWD, _ := filepath.EvalSymlinks(observedPWD)
	if gotPWD != wantPWD {
		t.Errorf("cwd: command ran from %q, want project root %q", gotPWD, wantPWD)
	}

	// ${X} tokens must have been expanded BEFORE the shell saw them — the
	// shell echoes whatever forge substituted in.
	for _, tc := range []struct{ field, want string }{
		{"image", "my-gw"},
		{"tag", "v1.2.3"},
		{"registry", "reg.example.com"},
		{"project_dir", projDir},
		{"arch", "arm64"},
	} {
		if g := parseField(t, got, tc.field); g != tc.want {
			t.Errorf("token %s: expanded to %q, want %q", tc.field, g, tc.want)
		}
	}
}

// TestBuildServiceShell_NoopTrue confirms the no-op ShellBuild the reliant
// sibling services declare (`cmd = "true  # ..."`) still succeeds: no
// relative paths, no ${} tokens, so the cwd + substitution change is a
// harmless no-op for them.
func TestBuildServiceShell_NoopTrue(t *testing.T) {
	projDir := t.TempDir()
	spec := buildtarget.Spec{Service: "reliant-noop", ProjectDir: projDir}
	res := buildServiceShell(context.Background(), "reliant-noop",
		&ShellBuild{Cmd: "true  # built upstream; nothing to do here"},
		buildOptions{outputDir: "bin"}, spec)
	if res.err != nil {
		t.Fatalf("no-op ShellBuild should succeed, got: %v", res.err)
	}
}

// TestBuildServiceShell_EmptyCmd keeps the empty-cmd guard intact.
func TestBuildServiceShell_EmptyCmd(t *testing.T) {
	res := buildServiceShell(context.Background(), "svc", &ShellBuild{Cmd: ""},
		buildOptions{}, buildtarget.Spec{ProjectDir: t.TempDir()})
	if res.err == nil {
		t.Fatalf("empty cmd should error")
	}
}

func parseField(t *testing.T, blob, key string) string {
	t.Helper()
	for _, line := range strings.Split(blob, "\n") {
		if strings.HasPrefix(line, key+"=") {
			return strings.TrimPrefix(line, key+"=")
		}
	}
	t.Fatalf("field %q not found in observed output:\n%s", key, blob)
	return ""
}
