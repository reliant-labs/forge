---
name: scenarios
description: URL-driven mock states for forge frontends — typed Connect-RPC handler overlays that let agents (and humans) teleport into any server-state shape via `?scenario=name`. The substrate for debugging UI bugs that depend on specific backend responses without a live backend or OAuth.
---

# Frontend Scenarios

## What this is for

Forge frontends ship with a mock transport (`src/lib/mock-transport.ts`) that returns deterministic fixture data when `VITE_MOCK_API=true`. That's enough for the happy path. It is **not** enough when a bug depends on a *specific* server response — e.g. "the credential parser misreads `hasToken` so the UI claims you're not connected even when the backend says you are". Reproducing that bug today requires manually driving OAuth in a real browser before Chrome MCP can take over — defeating the agent loop.

Scenarios are typed RPC handler overlays you select via `?scenario=name` in the URL. Each scenario is a small file declaring per-RPC overrides; anything not overridden falls through to the base fixtures. Chrome MCP, vitest tests, and humans all use the same URL → same state.

## The decision: handlers, not state-seeding

When you want the app to "look like the user has GitHub connected", there are two ways:

| Approach | What runs | What it catches |
|---|---|---|
| Seed React Query / Zustand directly | Bypasses parsers, transformers, serialization | "Does the *render* logic show the right thing for this exact cached value" |
| **Provide a mocked RPC response** | The real `getCredential()` runs, parses the response, populates the cache, then renders | "Does the entire pipeline from wire format → cache → render produce the right thing" |

The bug we hit last week — `cloud.ts` reading `res.has_token` when the server sends `res.hasToken` — would have been **invisible** under direct state-seeding (the parser never runs). It is **immediately visible** under RPC mocking (the parser runs against a realistic response and produces the wrong derived value).

**Use handlers. Reserve `setup()` for state that genuinely doesn't come from the server** — localStorage flags, URL params other than `scenario`, sessionStorage.

> Rule: if you reach for `setup()` to fake server data, you're testing the wrong layer.

## When to use scenarios

- **Debugging a UI bug that depends on backend state.** Write a scenario that reproduces the state; navigate Chrome DevTools MCP to `?scenario=<name>`; inspect what the component renders.
- **Integration tests that need a specific response shape.** Mount the app under test inside a `RouterProvider` with `?scenario=<name>` in the memory history; assert the rendered UI.
- **Demoing a feature that requires a populated backend.** Define a `feature-demo` scenario; ship a button on the docs page that links to `?scenario=feature-demo`.

## When NOT to use scenarios

- **Component-level rendering tests.** Just pass props directly; don't drag in the routing + mock-transport machinery.
- **Tests that should actually exercise the backend.** Use `forge test e2e`, not scenarios.
- **Replacing entity fixtures.** Forge already generates deterministic fixture data per entity from your proto definitions. Scenarios are *overlays* on top of those, not replacements.

## API

```ts
// src/mocks/scenarios/github-connected.ts
import { create } from "@bufbuild/protobuf";
import { GetGitCredentialResponseSchema } from "@/gen/.../git_credential_pb";
import { defineScenario } from "../scenario-types";

export default defineScenario({
  name: "github-connected",
  description: "User signed in with a valid GitHub credential",
  handlers: {
    "controlplane.v1.GitCredentialService/GetGitCredential": (req) =>
      create(GetGitCredentialResponseSchema, {
        provider: req.provider,
        hasToken: true,
        scopes: "user:email repo",
      }),
  },
});
```

Then either:

```bash
# Browser
open "http://localhost:5173/?scenario=github-connected"
```

```ts
// Test
const history = createMemoryHistory({ initialEntries: ["/?scenario=github-connected"] });
render(<RouterProvider router={createRouter({ routeTree, history })} />);
```

```python
# Chrome MCP from an agent
chrome_devtools.new_page(url="http://localhost:5173/?scenario=github-connected")
```

## CLI

```bash
# Scaffold a new scenario
forge add scenario github-connected

# When the project has multiple frontends, name the target
forge add scenario github-connected --frontend web

# Copy an existing scenario as a starting point
forge add scenario github-revoked --from github-connected
```

The CLI writes `src/mocks/scenarios/<name>.ts` and regenerates `src/mocks/scenarios/index.ts` so the new file is registered automatically.

## Stateful scenarios: sequences, click-driven flows, polling

Scenario files are plain JS modules. Anything you declare at module scope persists across handler calls — until a full page reload (which re-evaluates the module and resets state). That gives you sequenced responses, mutation→list interplay, and polling counters for free; no framework needed.

### Sequenced responses

```ts
// src/mocks/scenarios/slow-then-fast.ts
import { create } from "@bufbuild/protobuf";
import { GetStatusResponseSchema } from "@/gen/...";
import { defineScenario } from "../scenario-types";

let call = 0;
export default defineScenario({
  name: "slow-then-fast",
  description: "First two status polls return 'pending', subsequent ones return 'ready'",
  handlers: {
    "demo.v1.JobService/GetStatus": () => {
      call++;
      const status = call <= 2 ? "pending" : "ready";
      return create(GetStatusResponseSchema, { status });
    },
  },
});
```

Use this for retry behavior, polling-until-ready UIs, eventual-consistency renders.

### Click-driven mutation → list interplay

```ts
// src/mocks/scenarios/task-flow.ts
import { create } from "@bufbuild/protobuf";
import { ListTasksResponseSchema, CreateTaskResponseSchema, ApproveTaskResponseSchema, TaskSchema } from "@/gen/...";
import { defineScenario } from "../scenario-types";

const tasks = [
  create(TaskSchema, { id: "t1", title: "Seeded task", status: "open" }),
];

export default defineScenario({
  name: "task-flow",
  description: "Create + approve mutate the in-scenario list; subsequent List calls reflect changes",
  handlers: {
    "demo.v1.TaskService/ListTasks": () =>
      create(ListTasksResponseSchema, { tasks }),

    "demo.v1.TaskService/CreateTask": (req) => {
      const newTask = create(TaskSchema, {
        id: `t${tasks.length + 1}`,
        title: req.title ?? "Untitled",
        status: "open",
      });
      tasks.push(newTask);
      return create(CreateTaskResponseSchema, { task: newTask });
    },

    "demo.v1.TaskService/ApproveTask": (req) => {
      const t = tasks.find(x => x.id === req.id);
      if (t) t.status = "approved";
      return create(ApproveTaskResponseSchema, { task: t });
    },
  },
});
```

When the user clicks "Create" then "Approve" in the UI, the next `ListTasks` query reflects both. This is the right shape for testing optimistic updates, cache invalidation, post-mutation rerenders.

### Rules for stateful scenarios

- **Reset is full-reload.** Navigating to `?scenario=task-flow` again does NOT reset state. That's a feature — one scenario file = one stateful session. If you need a fresh slate, reload the page.
- **State is per-module, not per-tab-across-reloads.** sessionStorage is not used by default; if you want cross-reload persistence, write to sessionStorage in your handler.
- **Mutations should be plausibly atomic.** Handlers are async-but-typically-synchronous; don't rely on partial updates being observable.
- **Document the sequence in `description`.** Future you (or the next agent) reads it to know what the scenario claims to model.

## What the generator owns vs what you own

| File | Who edits |
|---|---|
| `src/mocks/scenario-types.ts` | Generator only — `defineScenario` helper + `Scenario` interface |
| `src/mocks/scenarios/index.ts` | Generator only — registry barrel, regenerated when scenarios are added/removed |
| `src/mocks/scenarios/default.ts` | Generator seeds it once; safe to edit (rarely needed) |
| `src/mocks/scenarios/<name>.ts` | You — created by `forge add scenario`, hand-edited after |
| `src/lib/mock-transport.ts` | Generator only — reads `?scenario=` and dispatches |

`forge generate` regenerates the registry. It does **not** overwrite your scenario files.

## Rules

- **Handlers receive typed proto messages and return typed proto messages.** Use `create(Schema, {...})`. No `as any`, no JSON blobs.
- **The default scenario has empty handlers and no setup.** It exists as the "nothing overridden" fallback; don't put fixtures there.
- **Stateful is fine.** Handlers can close over module-scope state for sequences, click-driven flows, and polling — see the section above. State persists for the lifetime of the loaded module (resets on full reload).
- **No network calls in `setup()`.** It runs synchronously before the transport is mounted.
- **Streaming is supported.** A handler can return an array, iterable, async iterable, or a Promise resolving to any of those. The transport adapts it into `AsyncIterable<Response>`. See "Streaming RPCs" in the ADR for examples.
- **One name per scenario, lowercase-kebab.** Matches the URL param.
- **Scenarios are committed to source.** They are documentation of "what server states this UI must handle correctly" — not local debugging scratch.

## Reference

- ADR: `docs/adr/0002-frontend-scenarios.md`
- Decision drivers, limitations, considered alternatives.
