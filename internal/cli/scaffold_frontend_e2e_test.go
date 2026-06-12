//go:build e2e

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestE2EScaffoldFrontendBuilds scaffolds a project with a --frontend web
// plus ONE CRUD entity, and drives the frontend through its real
// toolchain:
//
//	npm install
//	npm run build
//	npm test
//	npx tsc --noEmit
//
// The CRUD entity (`forge add service item` → Get/List/Create RPCs) is
// load-bearing: it makes `forge generate` emit the dynamic detail route
// (`src/app/items/[id]/page.tsx`), hooks, and dashboard tiles — the
// exact generated surface that historically broke a pristine project:
//
//   - `npm run build` failed under the old static-export default
//     ('Page "/items/[id]" is missing "generateStaticParams()" so it
//     cannot be used with "output: export"') because generated CRUD
//     detail pages are dynamic client routes.
//   - `npm test` failed ('No QueryClient set') because page.test.tsx
//     rendered the dashboard bare while dashboard_gen.tsx calls the
//     generated list hooks once an entity exists.
//
// A zero-entity scaffold would pass both while every real project (the
// moment it has one entity) fails — so the entity is part of the gate.
//
// The step split exists because each step guards a different kind of
// regression:
//
//   - `npm install` catches package.json/lockfile issues (unresolvable
//     deps, version conflicts) before any code runs.
//   - `npm run build` exercises the whole build pipeline (Next compile,
//     buf-generated code import graph, Tailwind, etc). This is the big
//     one — failures here usually point at a template regression in
//     one of the src/**/*.tsx files.
//   - `npm test` runs the scaffolded vitest suite (page.test.tsx and
//     any generated hook tests) — a pristine project must be green.
//   - `npx tsc --noEmit` is a stricter type-only check that catches
//     cases where `next build` might elide typing issues (legacy compat
//     flags, SWC-only paths).
//
// The test skips cleanly if Node isn't installed. In CI, the workflow
// must provision Node before running -tags=e2e.
func TestE2EScaffoldFrontendBuilds(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	if !toolAvailable("node") || !toolAvailable("npm") {
		t.Skip("node/npm not available — skipping frontend build check")
	}

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	linkForgeSibling(t, dir)

	runCmd(t, dir, forgeBin,
		"new", "feapp",
		"--mod", "example.com/feapp",
		"--frontend", "web",
	)

	projectDir := filepath.Join(dir, "feapp")

	// One CRUD entity so the generated frontend surface (dynamic [id]
	// detail page, list/create pages, hooks, dashboard tiles) exists.
	// `forge add service` scaffolds an empty proto; the documented flow
	// is user-written CRUD RPCs + a migration owning the schema —
	// frontend pages are only emitted for entities whose table exists
	// (codegen.ParseEntityProtos → schemadef.ApplyAndIntrospect).
	runCmd(t, projectDir, forgeBin, "add", "service", "item")
	writeFileE2E(t, filepath.Join(projectDir, "proto", "services", "item", "v1", "item.proto"), itemCRUDProto)
	writeFileE2E(t, filepath.Join(projectDir, "db", "migrations", "0001_create_items.up.sql"), itemsTableMigration)

	// Generate the TypeScript stubs the frontend imports. Without this
	// step the frontend build fails with "cannot find module" for every
	// Connect client.
	runCmd(t, projectDir, forgeBin, "generate")

	webDir := filepath.Join(projectDir, "frontends", "web")
	assertPathExistsE2E(t, filepath.Join(webDir, "package.json"))
	// The dynamic detail route must exist — it is the half of this test
	// that guards the build/export-mode interaction.
	assertPathExistsE2E(t, filepath.Join(webDir, "src", "app", "items", "[id]", "page.tsx"))

	// npm install — the longest single step. Use --no-audit/--no-fund
	// to reduce noisy output that would otherwise dominate the test
	// log when this test fails. --prefer-offline accelerates repeat
	// runs on developer machines that have a populated npm cache.
	runCmdTimeout(t, webDir, 5*time.Minute,
		"npm", "install", "--no-audit", "--no-fund", "--prefer-offline")

	// npm run build — the real regression target. If this fails, the
	// output will contain either a Next.js error (template issue) or a
	// missing import (codegen regression).
	runCmdTimeout(t, webDir, 5*time.Minute,
		"npm", "run", "build")

	// npm test — the scaffolded vitest suite must be green on a pristine
	// project with one entity.
	runCmdTimeout(t, webDir, 5*time.Minute,
		"npm", "test")

	// Strict type-check as a belt-and-braces guard — catches the cases
	// where Next's build produces a bundle despite type errors.
	runCmdTimeout(t, webDir, 2*time.Minute,
		"npx", "tsc", "--noEmit")
}

// writeFileE2E writes content to path, creating parent directories.
func writeFileE2E(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// itemCRUDProto is the canonical user-written first service: the same
// Get/List/Create shape forge's own example template documents. It is
// what makes `forge generate` emit the dynamic `[id]` detail route and
// the dashboard tile hook — the surfaces this test guards.
const itemCRUDProto = `syntax = "proto3";

package services.item.v1;

import "google/protobuf/timestamp.proto";

option go_package = "example.com/feapp/gen/services/item/v1;itemv1";

// ItemService defines the item service RPCs.
service ItemService {
  // GetItem retrieves an item by ID.
  rpc GetItem(GetItemRequest) returns (GetItemResponse) {}

  // ListItems returns a list of items.
  rpc ListItems(ListItemsRequest) returns (ListItemsResponse) {}

  // CreateItem creates a new item.
  rpc CreateItem(CreateItemRequest) returns (CreateItemResponse) {}
}

// Item represents an item entity.
message Item {
  string id = 1;
  string name = 2;
  string description = 3;
  google.protobuf.Timestamp created_at = 4;
}

message GetItemRequest {
  string id = 1;
}

message GetItemResponse {
  Item item = 1;
}

message ListItemsRequest {
  int32 page_size = 1;
  string page_token = 2;
}

message ListItemsResponse {
  repeated Item items = 1;
  string next_page_token = 2;
}

message CreateItemRequest {
  string name = 1;
  string description = 2;
}

message CreateItemResponse {
  Item item = 1;
}
`

// itemsTableMigration backs the Item entity with a real table —
// migrations own the schema, and frontend CRUD pages are only emitted
// for entities whose table exists in the shadow-applied schema.
const itemsTableMigration = `CREATE TABLE items (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    description TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
`

// runCmdTimeout is like runCmd but with an explicit timeout. npm install
// in particular can hang on a flaky network; a timeout gives the test a
// way to fail loudly rather than time out the whole test binary.
func runCmdTimeout(t *testing.T, dir string, timeout time.Duration, name string, args ...string) {
	t.Helper()

	done := make(chan error, 1)
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	go func() {
		out, err := cmd.CombinedOutput()
		if err != nil {
			done <- &cmdError{
				name: name, args: args, dir: dir,
				err: err, output: string(out),
			}
			return
		}
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%v", err)
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		t.Fatalf("command %q timed out after %s in %s",
			append([]string{name}, args...), timeout, dir)
	}
}

// cmdError is a small helper so runCmdTimeout surfaces the same debug
// information runCmd does when a command fails.
type cmdError struct {
	name   string
	args   []string
	dir    string
	err    error
	output string
}

func (e *cmdError) Error() string {
	return "command " + e.name + " failed in " + e.dir + ": " + e.err.Error() + "\n" + e.output
}
