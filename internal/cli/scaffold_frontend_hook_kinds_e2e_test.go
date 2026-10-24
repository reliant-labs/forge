//go:build e2e

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/naming"
)

// TestE2EGeneratedHooksReadNoSideEffectsAndTypeMutationContext is the compile
// gate on two properties of the generated React Query hooks, against the real
// runtime as a scaffolded app installs it:
//
//   - an rpc declaring `option idempotency_level = NO_SIDE_EFFECTS;` is a
//     useQuery whatever its name. Version is a noun, not a read verb, so the
//     name heuristic alone generated a useMutation, and a caller had to
//     hand-build the query key the factory would have given it.
//   - a mutation hook's TContext is inferred from onMutate, so an optimistic
//     update reads its rollback off onError's context with no cast. The
//     hook used to pin TContext to `unknown`.
//
// Positive AND negative, like the error-contract gate beside it: a positive
// half alone passes just as green when the hook is `any`. The negative half
// proves the kind (a query result has no `mutate`) and the context type (a
// field onMutate never returned is rejected) are load-bearing.
//
// COST: one scaffold + one `npm install` + two tsc runs (~2-4 min).
func TestE2EGeneratedHooksReadNoSideEffectsAndTypeMutationContext(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	requireTool(t, "node", "npm")

	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmdTimeout(t, dir, 10*time.Minute, forgeBin,
		"project", "new", "kindsapp",
		"--mod", "example.com/kindsapp",
		"--frontend", "web",
	)
	projectDir := filepath.Join(dir, "kindsapp")
	addCorpusForgePkgReplace(t, projectDir)
	runCmd(t, projectDir, forgeBin, "scaffold", "service", "item")

	// A CRUD entity for the mutation (CreateItem), and a custom read named
	// as a noun that declares itself side-effect free.
	protoPath := filepath.Join(projectDir, "proto", "services", "item", "v1", "item.proto")
	proto := readFileE2E(t, protoPath)
	const rpcTODO = "  // TODO: Add your RPC methods here.\n"
	if !strings.Contains(proto, rpcTODO) {
		t.Fatalf("scaffolded item.proto no longer carries the RPC TODO anchor:\n%s", proto)
	}
	proto = strings.Replace(proto, rpcTODO,
		"  rpc Version(VersionRequest) returns (VersionResponse) {\n    option idempotency_level = NO_SIDE_EFFECTS;\n  }\n", 1)
	proto += `
message VersionRequest {}

message VersionResponse {
  string version = 1;
}

// forge:entity
message Item {
  string id = 1;
  string name = 2;
}
`
	if err := os.WriteFile(protoPath, []byte(proto), 0o644); err != nil {
		t.Fatalf("write item.proto: %v", err)
	}
	runCmd(t, projectDir, forgeBin, "scaffold")
	if got := readFileE2E(t, protoPath); !strings.Contains(got, "rpc CreateItem(") {
		t.Fatalf("scaffold did not inject CreateItem — with no mutation the context half is vacuous:\n%s", got)
	}
	runCmd(t, projectDir, forgeBin, "generate")

	webDir := filepath.Join(projectDir, "frontends", "web")
	hooksFile := naming.ServiceHookFile("ItemService")
	hooksModule := "@/hooks/" + strings.TrimSuffix(hooksFile, ".ts")

	// The classification, read off the artifact: Version is built by the
	// query factory and has a key-factory entry of its own.
	hooks := readFileE2E(t, filepath.Join(webDir, "src", "hooks", hooksFile))
	for _, want := range []string{
		"export const useVersion = createQueryHook<",
		`version: keys.query("version", VersionRequestSchema)`,
		"export const useCreateItem = createMutationHook<",
	} {
		if !strings.Contains(hooks, want) {
			t.Errorf("generated hooks are missing %q:\n%s", want, hooks)
		}
	}

	// ── POSITIVE: the read is a query, the rollback needs no cast ──────
	writeFileE2E(t, filepath.Join(webDir, "src", "app", "hookkinds", "page.tsx"),
		fmt.Sprintf(hookKindsPositiveFixture, hooksModule))
	runCmdTimeout(t, webDir, 3*time.Minute, "npx", "--no-install", "tsc", "--noEmit")

	// ── NEGATIVE: neither property may be `any` ─────────────────────────
	writeFileE2E(t, filepath.Join(webDir, "src", "app", "hookkinds", "negative.ts"),
		fmt.Sprintf(hookKindsNegativeFixture, hooksModule))
	out, err := runCmdCombined(webDir, 3*time.Minute, "npx", "--no-install", "tsc", "--noEmit")
	if err == nil {
		t.Fatalf("tsc accepted a query result's `mutate` and an undeclared context field — "+
			"the hooks are untyped and the positive half proves nothing:\n%s", out)
	}
	if !strings.Contains(out, "Property 'mutate' does not exist") {
		t.Errorf("tsc did not reject `mutate` on useVersion — the NO_SIDE_EFFECTS rpc is not a query hook:\n%s", out)
	}
	if !strings.Contains(out, "nonexistentRollback") {
		t.Errorf("tsc did not reject a field onMutate never returned — TContext is not inferred:\n%s", out)
	}
}

// hookKindsPositiveFixture uses useVersion as a read (request in, data out)
// and rolls an optimistic CreateItem back through onError's typed context.
// %s is the generated hooks module specifier.
const hookKindsPositiveFixture = `"use client";

import { useCreateItem, useVersion } from "%s";

export default function HookKindsPage() {
  const version = useVersion({});
  const label: string = version.data?.version ?? "";
  const create = useCreateItem({
    onMutate: (req) => ({ rollback: () => String(req.name) }),
    onError: (_err, _req, context) => {
      context?.rollback();
    },
  });
  return <button onClick={() => create.mutate({ name: "x" })}>{label}</button>;
}
`

// hookKindsNegativeFixture MUST NOT compile, once per property. %s is the
// generated hooks module specifier.
const hookKindsNegativeFixture = `import { useCreateItem, useVersion } from "%s";

export function bogus() {
  const version = useVersion({});
  version.mutate({});
  useCreateItem({
    onMutate: () => ({ rollback: () => undefined }),
    onError: (_err, _req, context) => context?.nonexistentRollback(),
  });
}
`
