package generator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNewFrontendWorkspaceLayout_DerivesScopeFromProjectName asserts
// that the workspace package names are derived from the project name
// — `my-app` produces `@my-app/api` / `@my-app/hooks`. Uppercase and
// disallowed chars are stripped so the scope is a valid npm segment.
func TestNewFrontendWorkspaceLayout_DerivesScopeFromProjectName(t *testing.T) {
	tests := []struct {
		name       string
		project    string
		wantScope  string
		wantApi    string
		wantHooks  string
	}{
		{"plain name", "myapp", "myapp", "@myapp/api", "@myapp/hooks"},
		{"hyphen preserved", "my-app", "my-app", "@my-app/api", "@my-app/hooks"},
		{"underscore preserved", "my_app", "my_app", "@my_app/api", "@my_app/hooks"},
		{"uppercase lowered", "MyApp", "myapp", "@myapp/api", "@myapp/hooks"},
		{"dots stripped", "my.app", "myapp", "@myapp/api", "@myapp/hooks"},
		{"empty falls back to app", "", "app", "@app/api", "@app/hooks"},
		{"all-invalid falls back to app", "@@@", "app", "@app/api", "@app/hooks"},
		{"leading digit trimmed", "9app", "app", "@app/api", "@app/hooks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewFrontendWorkspaceLayout(tt.project)
			if got.Scope != tt.wantScope {
				t.Errorf("Scope = %q, want %q", got.Scope, tt.wantScope)
			}
			if got.ApiPackage != tt.wantApi {
				t.Errorf("ApiPackage = %q, want %q", got.ApiPackage, tt.wantApi)
			}
			if got.HooksPackage != tt.wantHooks {
				t.Errorf("HooksPackage = %q, want %q", got.HooksPackage, tt.wantHooks)
			}
		})
	}
}

// TestWriteFrontendWorkspaceFiles_DisabledNoop asserts that calling
// WriteFrontendWorkspaceFiles with workspaces=false writes nothing.
// This is the load-bearing invariant for snapshot stability: existing
// projects that don't opt in must see byte-identical output.
func TestWriteFrontendWorkspaceFiles_DisabledNoop(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFrontendWorkspaceFiles(dir, "myapp", false); err != nil {
		t.Fatalf("WriteFrontendWorkspaceFiles: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read tempdir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("workspaces=false should produce no files, got: %v", names)
	}
}

// TestWriteFrontendWorkspaceFiles_EmitsWorkspaceLayout asserts the
// pnpm-workspace.yaml + packages/api + packages/hooks scaffolding lands
// at the expected paths with valid JSON manifests and the project-
// derived scope.
func TestWriteFrontendWorkspaceFiles_EmitsWorkspaceLayout(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFrontendWorkspaceFiles(dir, "myapp", true); err != nil {
		t.Fatalf("WriteFrontendWorkspaceFiles: %v", err)
	}

	// pnpm-workspace.yaml at root.
	body := mustRead(t, filepath.Join(dir, "pnpm-workspace.yaml"))
	if !strings.Contains(body, "packages/*") {
		t.Errorf("pnpm-workspace.yaml should list packages/*, got:\n%s", body)
	}
	if !strings.Contains(body, "frontends/*") {
		t.Errorf("pnpm-workspace.yaml should list frontends/*, got:\n%s", body)
	}

	// packages/api/package.json declares the scoped name + bufbuild + connectrpc deps.
	apiPkgPath := filepath.Join(dir, "packages", "api", "package.json")
	var apiPkg map[string]any
	if err := json.Unmarshal([]byte(mustRead(t, apiPkgPath)), &apiPkg); err != nil {
		t.Fatalf("parse packages/api/package.json: %v", err)
	}
	if apiPkg["name"] != "@myapp/api" {
		t.Errorf("packages/api name = %v, want @myapp/api", apiPkg["name"])
	}
	apiDeps, _ := apiPkg["dependencies"].(map[string]any)
	if _, ok := apiDeps["@bufbuild/protobuf"]; !ok {
		t.Errorf("packages/api should depend on @bufbuild/protobuf, got: %v", apiDeps)
	}
	if _, ok := apiDeps["@connectrpc/connect"]; !ok {
		t.Errorf("packages/api should depend on @connectrpc/connect, got: %v", apiDeps)
	}

	// packages/hooks/package.json declares workspace:* dep on @myapp/api
	// plus @tanstack/react-query.
	hooksPkgPath := filepath.Join(dir, "packages", "hooks", "package.json")
	var hooksPkg map[string]any
	if err := json.Unmarshal([]byte(mustRead(t, hooksPkgPath)), &hooksPkg); err != nil {
		t.Fatalf("parse packages/hooks/package.json: %v", err)
	}
	if hooksPkg["name"] != "@myapp/hooks" {
		t.Errorf("packages/hooks name = %v, want @myapp/hooks", hooksPkg["name"])
	}
	hooksDeps, _ := hooksPkg["dependencies"].(map[string]any)
	if hooksDeps["@myapp/api"] != "workspace:*" {
		t.Errorf("packages/hooks should declare \"@myapp/api\": \"workspace:*\", got: %v", hooksDeps)
	}
	if _, ok := hooksDeps["@tanstack/react-query"]; !ok {
		t.Errorf("packages/hooks should depend on @tanstack/react-query, got: %v", hooksDeps)
	}

	// Base hook wrappers exist.
	uq := mustRead(t, filepath.Join(dir, "packages", "hooks", "src", "use-api-query.ts"))
	if !strings.Contains(uq, "useApiQuery") {
		t.Errorf("use-api-query.ts should export useApiQuery, got:\n%s", uq)
	}
	um := mustRead(t, filepath.Join(dir, "packages", "hooks", "src", "use-api-mutation.ts"))
	if !strings.Contains(um, "useApiMutation") {
		t.Errorf("use-api-mutation.ts should export useApiMutation, got:\n%s", um)
	}
	// Transport bootstrap is the bridge from per-frontend Transport to
	// the shared hooks — without it generated hooks have no way to
	// reach the network. Pin its presence.
	tr := mustRead(t, filepath.Join(dir, "packages", "hooks", "src", "transport.ts"))
	if !strings.Contains(tr, "setApiTransport") || !strings.Contains(tr, "connectClient") {
		t.Errorf("transport.ts should export setApiTransport + connectClient, got:\n%s", tr)
	}

	// packages/hooks/tsconfig.json must NOT include "DOM" in lib — the
	// package has to stay consumable from React Native (no document/
	// window). This is a regression guardrail: a future contributor who
	// adds DOM-only APIs will hit tsc errors in the hooks workspace.
	hooksTsconfig := mustRead(t, filepath.Join(dir, "packages", "hooks", "tsconfig.json"))
	if strings.Contains(hooksTsconfig, "\"DOM\"") {
		t.Errorf("packages/hooks/tsconfig.json must not include DOM lib (hooks are DOM-free), got:\n%s", hooksTsconfig)
	}
}

// TestWriteFrontendWorkspaceFiles_Idempotent asserts re-running the
// emitter doesn't clobber user edits to package.json / tsconfig.json.
// This is the safety net for `forge generate` cycles: the user owns
// these files once they exist on disk.
func TestWriteFrontendWorkspaceFiles_Idempotent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFrontendWorkspaceFiles(dir, "myapp", true); err != nil {
		t.Fatalf("first emit: %v", err)
	}

	// Simulate a user edit to packages/api/package.json — add a custom
	// dependency. A second `forge generate` must preserve it.
	apiPkgPath := filepath.Join(dir, "packages", "api", "package.json")
	userEdit := `{"name":"@myapp/api","version":"0.0.0","dependencies":{"custom":"^1.0.0"}}`
	if err := os.WriteFile(apiPkgPath, []byte(userEdit), 0o644); err != nil {
		t.Fatalf("write user edit: %v", err)
	}

	if err := WriteFrontendWorkspaceFiles(dir, "myapp", true); err != nil {
		t.Fatalf("second emit: %v", err)
	}

	got := mustRead(t, apiPkgPath)
	if got != userEdit {
		t.Errorf("user edit was clobbered on second emit\nwant: %s\n got: %s", userEdit, got)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
