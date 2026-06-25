package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestShellBuild_CwdAndSubstitution verifies the unified ShellBuild
// contract through the single dispatcher (buildExternalServices): the
// command runs from the resolved cwd (here the PROJECT ROOT, since the
// ShellBuild declares no cwd), and the documented ${X} tokens are
// substituted into the command before exec.
//
// It drives a real `sh -c` and inspects what the shell observed (its cwd
// via $(pwd), and the already-expanded ${PROJECT_DIR}/${IMAGE}/${TAG}/
// ${REGISTRY}/${TARGETARCH} tokens).
func TestShellBuild_CwdAndSubstitution(t *testing.T) {
	projDir := t.TempDir()
	// A relative script path under the project root — resolving it from
	// the project root is the whole point of the cwd contract.
	scriptsDir := filepath.Join(projDir, "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(scriptsDir, "build-image.sh"), []byte("#!/bin/sh\necho ran\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	outFile := filepath.Join(projDir, "observed.txt")
	// The command exercises: (1) a relative scripts/ path that must resolve
	// against the project root, and (2) ${X} substitution. It records $(pwd)
	// and the post-substitution token values into observed.txt.
	// Note: $(pwd) (command substitution) is used rather than $PWD because
	// forge's substitution runs os.Expand over the whole string first, which
	// would consume a bare $PWD (it's an unknown token → empty). $( is left
	// untouched by os.Expand.
	cmd := "sh scripts/build-image.sh > /dev/null && " +
		"printf 'pwd=%s\\nimage=%s\\ntag=%s\\nregistry=%s\\nproject_dir=%s\\narch=%s\\n' " +
		"\"$(pwd)\" '${IMAGE}' '${TAG}' '${REGISTRY}' '${PROJECT_DIR}' '${TARGETARCH}' > observed.txt"
	svcs := []ServiceEntity{shellSvc("gw", "my-gw", cmd, "", nil)}

	opts := buildOptions{env: "dev", parallel: false, outputDir: "bin"}
	results := buildExternalServices(context.Background(), noBaseImagesCfg(), svcs, opts,
		"reg.example.com", "v1.2.3", projDir, "arm64", nil)
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("buildExternalServices: %+v", results)
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

	// ${X} tokens must have been expanded BEFORE the shell saw them.
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

// TestShellBuild_NoopTrue confirms the no-op ShellBuild the reliant
// sibling services declare (`cmd = "true  # ..."`) still succeeds through
// the unified dispatcher: no relative paths, no ${} tokens, nothing
// pushed — so the digest lookup finds nothing and the build is a harmless
// success.
func TestShellBuild_NoopTrue(t *testing.T) {
	projDir := t.TempDir()
	svcs := []ServiceEntity{shellSvc("reliant-noop", "reliant", "true  # built upstream; nothing to do here", "", nil)}
	results := buildExternalServices(context.Background(), noBaseImagesCfg(), svcs,
		buildOptions{env: "dev", outputDir: "bin"},
		"reg", "dev", projDir, "amd64", nil)
	if len(results) != 1 || results[0].err != nil {
		t.Fatalf("no-op ShellBuild should succeed, got: %+v", results)
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
