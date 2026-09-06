package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTSPluginBin creates an executable stub at dir/node_modules/.bin/protoc-gen-es.
func writeTSPluginBin(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "node_modules", ".bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "protoc-gen-es"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The standalone layout: the frontend has its own node_modules, which is what
// every non-bridged project looks like. The frontend-local path must win.
func TestResolveLocalTSPluginRel_FrontendLocal(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	feDir := filepath.Join("frontends", "web")
	writeTSPluginBin(t, filepath.Join(projectDir, feDir))

	got, ok := resolveLocalTSPluginRel(projectDir, feDir)
	if !ok {
		t.Fatal("plugin installed in the frontend was not found")
	}
	if want := "./frontends/web/node_modules/.bin/protoc-gen-es"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestResolveLocalTSPluginRel_WorkspaceHoisted is the reproduction.
//
// A dev build of forge writes a gitignored npm workspace root above the
// frontends (internal/generator/frontend_webruntime_devlink.go). npm then
// HOISTS every member's dependencies to <project>/node_modules and creates no
// frontends/<name>/node_modules at all — verified against a real scaffold.
//
// Looking only in the frontend made forge report "@bufbuild/protoc-gen-es not
// installed yet" for a plugin that WAS installed one directory up, skip the
// TypeScript pass, and emit no src/gen/ whatsoever. The failure then surfaced
// far away as tsc TS2307 "Cannot find module '@/gen/services/<svc>/v1/<svc>_pb'"
// against files forge had just claimed to generate — which reads as a codegen
// bug rather than a plugin that was never run.
func TestResolveLocalTSPluginRel_WorkspaceHoisted(t *testing.T) {
	t.Parallel()

	projectDir := t.TempDir()
	writeTSPluginBin(t, projectDir) // hoisted to the workspace root

	got, ok := resolveLocalTSPluginRel(projectDir, filepath.Join("frontends", "web"))
	if !ok {
		t.Fatal("plugin hoisted to the workspace root was not found — forge skips TS codegen " +
			"and the scaffold fails tsc with TS2307 on its own generated imports")
	}
	if want := "./node_modules/.bin/protoc-gen-es"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// Neither location has it: the caller must warn and skip rather than invoking
// buf and letting it die on a confusing fork/exec error.
func TestResolveLocalTSPluginRel_Absent(t *testing.T) {
	t.Parallel()

	if got, ok := resolveLocalTSPluginRel(t.TempDir(), filepath.Join("frontends", "web")); ok {
		t.Errorf("reported a plugin at %q when none is installed", got)
	}
}

const tsPluginBufGen = `version: v2
plugins:
  - local: ./frontends/web/node_modules/.bin/protoc-gen-es
    out: frontends/web/src/gen
    include_imports: true
    opt:
      - target=ts
`

// buf.gen.yaml is scaffold-once, so a project whose node_modules layout later
// changed keeps a plugin path that no longer resolves. Forge must heal it.
func TestRetargetLocalTSPlugin_RewritesStalePath(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "buf.gen.yaml")
	if err := os.WriteFile(path, []byte(tsPluginBufGen), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := retargetLocalTSPlugin(path, "./node_modules/.bin/protoc-gen-es"); err != nil {
		t.Fatal(err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	if !strings.Contains(got, "- local: ./node_modules/.bin/protoc-gen-es") {
		t.Errorf("plugin path was not retargeted:\n%s", got)
	}
	if strings.Contains(got, "./frontends/web/node_modules/.bin/protoc-gen-es") {
		t.Errorf("the stale path survived:\n%s", got)
	}
	// Everything else the user owns must be untouched.
	for _, want := range []string{"out: frontends/web/src/gen", "include_imports: true", "- target=ts"} {
		if !strings.Contains(got, want) {
			t.Errorf("retarget disturbed unrelated config (missing %q):\n%s", want, got)
		}
	}
}

// A path that already matches must not be rewritten — `forge generate` twice in
// a row has to report no changes, and a needless write churns the file's mtime,
// which the staleness checks elsewhere key on.
func TestRetargetLocalTSPlugin_Idempotent(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "buf.gen.yaml")
	if err := os.WriteFile(path, []byte(tsPluginBufGen), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := retargetLocalTSPlugin(path, "./frontends/web/node_modules/.bin/protoc-gen-es"); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("rewrote a buf.gen.yaml whose plugin path was already correct")
	}
}
