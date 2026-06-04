package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// codegenServiceDefForStarterTest returns a 2-RPC ServiceDef suitable as
// input to writeHookStarterTest in the starter-test unit tests. Kept as a
// helper rather than inlined so the three starter tests share one shape.
func codegenServiceDefForStarterTest() codegen.ServiceDef {
	return codegen.ServiceDef{
		Name:      "UserService",
		ProtoFile: "proto/services/users/v1/users.proto",
		Methods: []codegen.Method{
			{Name: "GetUser", InputType: "GetUserRequest", OutputType: "GetUserResponse"},
			{Name: "CreateUser", InputType: "CreateUserRequest", OutputType: "CreateUserResponse"},
		},
	}
}

func codegenHookDataForStarterTest() codegen.FrontendHookTemplateData {
	return codegen.ServiceDefToHookData(codegenServiceDefForStarterTest())
}

// TestWriteHooksIndex_FlatModeNoCollisions asserts the historic shape is
// preserved when no two hook files re-export the same identifier: a flat
// `export * from "./..."` per file. This is the path nearly every
// single-service project takes; regressing it would break every existing
// `import { useGetUser } from "@/hooks"` site.
func TestWriteHooksIndex_FlatModeNoCollisions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.ts")

	files := []hookFileEntry{
		{fileName: "user-service-hooks.ts", nsAlias: "userService", symbols: []string{"useGetUser", "useListUsers"}},
		{fileName: "org-service-hooks.ts", nsAlias: "orgService", symbols: []string{"useGetOrg", "useListOrgs"}},
	}

	if err := writeHooksIndex(path, files); err != nil {
		t.Fatalf("writeHooksIndex: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index.ts: %v", err)
	}
	s := string(got)

	if !strings.Contains(s, "Mode: flat wildcard re-exports") {
		t.Errorf("expected flat-mode comment, got:\n%s", s)
	}
	if !strings.Contains(s, `export * from "./user-service-hooks";`) {
		t.Errorf("expected flat wildcard for user-service-hooks, got:\n%s", s)
	}
	if !strings.Contains(s, `export * from "./org-service-hooks";`) {
		t.Errorf("expected flat wildcard for org-service-hooks, got:\n%s", s)
	}
	if strings.Contains(s, "export * as") {
		t.Errorf("did not expect namespace re-exports in flat mode, got:\n%s", s)
	}
}

// TestWriteHooksIndex_NamespacedModeOnCollision asserts that when two hook
// files export the same identifier (e.g. both have a generic `useList`
// because each service has a List RPC), the entire barrel switches to
// `export * as <alias>` form. This is the collision-aware fix that
// unblocks projects the moment they grow past one service with overlapping
// RPC names.
func TestWriteHooksIndex_NamespacedModeOnCollision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.ts")

	// Both services export `useList` — flat wildcard would generate two
	// `export * from "..."` lines re-exporting the same identifier,
	// which tsc rejects as a duplicate-export error.
	files := []hookFileEntry{
		{fileName: "user-service-hooks.ts", nsAlias: "userService", symbols: []string{"useGet", "useList"}},
		{fileName: "org-service-hooks.ts", nsAlias: "orgService", symbols: []string{"useGet", "useList"}},
	}

	if err := writeHooksIndex(path, files); err != nil {
		t.Fatalf("writeHooksIndex: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read index.ts: %v", err)
	}
	s := string(got)

	if !strings.Contains(s, "Mode: namespaced re-exports") {
		t.Errorf("expected namespaced-mode comment, got:\n%s", s)
	}
	// Both collisions should be documented in the comment block so the
	// user knows exactly which symbols forced the switch.
	if !strings.Contains(s, "useGet (from org-service-hooks.ts, user-service-hooks.ts)") {
		t.Errorf("expected useGet collision in comment, got:\n%s", s)
	}
	if !strings.Contains(s, "useList (from org-service-hooks.ts, user-service-hooks.ts)") {
		t.Errorf("expected useList collision in comment, got:\n%s", s)
	}
	if !strings.Contains(s, `export * as userService from "./user-service-hooks";`) {
		t.Errorf("expected namespaced re-export for userService, got:\n%s", s)
	}
	if !strings.Contains(s, `export * as orgService from "./org-service-hooks";`) {
		t.Errorf("expected namespaced re-export for orgService, got:\n%s", s)
	}
	// Confirm the flat wildcard form is NOT present in namespaced mode.
	if strings.Contains(s, `export * from "./user-service-hooks";`) {
		t.Errorf("did not expect flat wildcard in namespaced mode, got:\n%s", s)
	}
}

// TestDetectIndexCollisions_NoOverlap asserts the no-collision path
// returns nil, which is the signal writeHooksIndex uses to pick flat-mode
// emission.
func TestDetectIndexCollisions_NoOverlap(t *testing.T) {
	files := []hookFileEntry{
		{fileName: "a.ts", symbols: []string{"useGetA", "useListA"}},
		{fileName: "b.ts", symbols: []string{"useGetB", "useListB"}},
	}
	if got := detectIndexCollisions(files); len(got) != 0 {
		t.Errorf("expected zero collisions, got %+v", got)
	}
}

// TestDetectIndexCollisions_ListsAllOverlapsSorted asserts ALL colliding
// symbols are reported (not just the first) and the result is sorted so
// the comment block at the top of index.ts is byte-stable across runs.
func TestDetectIndexCollisions_ListsAllOverlapsSorted(t *testing.T) {
	files := []hookFileEntry{
		{fileName: "a.ts", symbols: []string{"useList", "useGet", "useUnique"}},
		{fileName: "b.ts", symbols: []string{"useGet", "useList"}},
	}
	got := detectIndexCollisions(files)
	if len(got) != 2 {
		t.Fatalf("expected 2 collisions, got %d: %+v", len(got), got)
	}
	if got[0].symbol != "useGet" || got[1].symbol != "useList" {
		t.Errorf("expected sorted symbols [useGet, useList], got [%s, %s]", got[0].symbol, got[1].symbol)
	}
	// File slices inside each collision must also be sorted.
	want := []string{"a.ts", "b.ts"}
	for _, c := range got {
		if len(c.files) != 2 || c.files[0] != want[0] || c.files[1] != want[1] {
			t.Errorf("expected files %v for %s, got %v", want, c.symbol, c.files)
		}
	}
}

// TestGenerateFrontendHooks_PerFrontendByDefault asserts that the
// default (workspaces=false) emit lands at frontends/<name>/src/hooks/
// with the historic relative-import shape. This is the snapshot
// stability test — projects that don't opt in must see the exact
// pre-workspaces layout.
func TestGenerateFrontendHooks_PerFrontendByDefault(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name: "myapp",
		Frontends: []config.FrontendConfig{
			{Name: "web", Type: "nextjs", Path: "frontends/web"},
		},
	}
	services := []codegen.ServiceDef{
		fakeService("UserService", "proto/services/users/v1/users.proto"),
	}
	if err := os.MkdirAll(filepath.Join(dir, "frontends/web/src/hooks"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := generateFrontendHooks(cfg, services, dir); err != nil {
		t.Fatalf("generateFrontendHooks: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "frontends/web/src/hooks/user-service-hooks.ts"))
	if err != nil {
		t.Fatalf("read per-frontend hooks: %v", err)
	}
	s := string(got)
	// Per-frontend mode imports from @/lib/connect (frontend-local).
	if !strings.Contains(s, `from "@/lib/connect"`) {
		t.Errorf("per-frontend hook should import connectClient from @/lib/connect, got:\n%s", s)
	}
	// And from @/gen for the proto types.
	if !strings.Contains(s, `from "@/gen/`) {
		t.Errorf("per-frontend hook should import proto types from @/gen, got:\n%s", s)
	}
	// Workspace import shapes must NOT appear in default mode.
	if strings.Contains(s, "../transport") || strings.Contains(s, "@myapp/api") {
		t.Errorf("per-frontend hook should not import from workspace paths, got:\n%s", s)
	}

	// The shared packages/hooks/ dir should NOT be touched.
	if _, err := os.Stat(filepath.Join(dir, "packages/hooks")); !os.IsNotExist(err) {
		t.Errorf("workspaces=false should not create packages/hooks/, got err=%v", err)
	}
}

// TestGenerateFrontendHooks_WorkspaceModeEmitsToSharedDir asserts that
// the workspace-mode emit lands at packages/hooks/src/generated/ with
// imports rewritten to @<scope>/api and "../transport".
func TestGenerateFrontendHooks_WorkspaceModeEmitsToSharedDir(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.ProjectConfig{
		Name: "myapp",
		Frontends: []config.FrontendConfig{
			{Name: "web", Type: "nextjs", Path: "frontends/web"},
			{Name: "mobile", Type: "react-native", Path: "frontends/mobile"},
		},
		Frontend: config.FrontendProjectConfig{Workspaces: true},
	}
	services := []codegen.ServiceDef{
		fakeService("UserService", "proto/services/users/v1/users.proto"),
	}

	if err := generateFrontendHooks(cfg, services, dir); err != nil {
		t.Fatalf("generateFrontendHooks: %v", err)
	}

	// Workspace mode writes one file shared across all frontends.
	got, err := os.ReadFile(filepath.Join(dir, "packages/hooks/src/generated/user-service-hooks.ts"))
	if err != nil {
		t.Fatalf("read workspace hooks: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `from "../transport"`) {
		t.Errorf("workspace hook should import connectClient from ../transport, got:\n%s", s)
	}
	if !strings.Contains(s, `from "@myapp/api/services/users/v1/users_pb"`) {
		t.Errorf("workspace hook should import proto types from @myapp/api/<path>, got:\n%s", s)
	}
	// Per-frontend hooks dirs should NOT be written in workspace mode.
	for _, fe := range cfg.Frontends {
		p := filepath.Join(dir, fe.Path, "src/hooks/user-service-hooks.ts")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("workspaces=true should not write per-frontend %s, got err=%v", p, err)
		}
	}

	// Barrel index.ts re-exports the generated file.
	idx, err := os.ReadFile(filepath.Join(dir, "packages/hooks/src/generated/index.ts"))
	if err != nil {
		t.Fatalf("read workspace hooks index: %v", err)
	}
	if !strings.Contains(string(idx), `export * from "./user-service-hooks"`) {
		t.Errorf("workspace hooks index should re-export user-service-hooks, got:\n%s", idx)
	}
}

// fakeService synthesizes a minimal ServiceDef suitable for hook
// rendering. The minimum the renderer needs is a name, a proto file
// (drives the import path), and at least one non-streaming RPC.
func fakeService(name, protoFile string) codegen.ServiceDef {
	return codegen.ServiceDef{
		Name:      name,
		ProtoFile: protoFile,
		Methods: []codegen.Method{
			{Name: "GetUser", InputType: "GetUserRequest", OutputType: "GetUserResponse"},
			{Name: "CreateUser", InputType: "CreateUserRequest", OutputType: "CreateUserResponse"},
		},
	}
}

// TestWriteHookStarterTest_EmitsStarterWhenNoSiblingPresent asserts that
// the starter test scaffolds next to the hooks file with one row per RPC
// and the right `.starter` suffix. The activation contract is "rename
// .tsx.starter to .tsx to opt in" — so the suffix must be exact.
func TestWriteHookStarterTest_EmitsStarterWhenNoSiblingPresent(t *testing.T) {
	dir := t.TempDir()
	svc := codegenServiceDefForStarterTest()
	data := codegenHookDataForStarterTest()

	if err := writeHookStarterTest(dir, "user-service-hooks.ts", svc, data); err != nil {
		t.Fatalf("writeHookStarterTest: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "user-service-hooks.test.tsx.starter"))
	if err != nil {
		t.Fatalf("expected starter test to exist: %v", err)
	}
	s := string(got)
	// Per-RPC rows
	for _, want := range []string{
		"useGetUser resolves a happy-path response",
		"useCreateUser resolves a happy-path response",
		`"UserService.GetUser":`,
		`"UserService.CreateUser":`,
		"import { useGetUser }",
		"import { useCreateUser }",
		"mockTransport",
		"setTransport",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("starter missing %q, got:\n%s", want, s)
		}
	}
}

// TestWriteHookStarterTest_SkipsWhenActiveTestExists asserts the starter
// is NOT written when the user has already activated `<file>.test.tsx`.
// Regenerating must not clobber the user's hand-edited tests.
func TestWriteHookStarterTest_SkipsWhenActiveTestExists(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "user-service-hooks.test.tsx")
	if err := os.WriteFile(activePath, []byte("// user-edited"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := codegenServiceDefForStarterTest()
	data := codegenHookDataForStarterTest()
	if err := writeHookStarterTest(dir, "user-service-hooks.ts", svc, data); err != nil {
		t.Fatalf("writeHookStarterTest: %v", err)
	}

	// The user's file must not have been touched.
	got, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "// user-edited" {
		t.Errorf("active test was overwritten: %s", string(got))
	}
	// And no .starter should have been written alongside it.
	if _, err := os.Stat(filepath.Join(dir, "user-service-hooks.test.tsx.starter")); err == nil {
		t.Errorf("starter was written despite active test existing")
	}
}

// TestWriteHookStarterTest_SkipsWhenStarterExists asserts the starter is
// NOT overwritten on re-run. An unactivated starter the user is about to
// rename stays put across regen cycles.
func TestWriteHookStarterTest_SkipsWhenStarterExists(t *testing.T) {
	dir := t.TempDir()
	starterPath := filepath.Join(dir, "user-service-hooks.test.tsx.starter")
	if err := os.WriteFile(starterPath, []byte("// previous starter"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc := codegenServiceDefForStarterTest()
	data := codegenHookDataForStarterTest()
	if err := writeHookStarterTest(dir, "user-service-hooks.ts", svc, data); err != nil {
		t.Fatalf("writeHookStarterTest: %v", err)
	}

	got, err := os.ReadFile(starterPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "// previous starter" {
		t.Errorf("existing starter was overwritten: %s", string(got))
	}
}
