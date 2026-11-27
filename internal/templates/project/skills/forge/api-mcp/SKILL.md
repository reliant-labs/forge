---
name: api-mcp
description: Serve your app's own rpcs as MCP tools on the API's /mcp endpoint — opt in per rpc or service with `(forge.v1.method).mcp`, acknowledge mutations with `mcp_mutating`, write descriptions a model can choose from, keep the tool count small, connect clients with `forge mcp app <env>` and a bearer token, and the three warn-first lint rules.
---

# MCP tools from your rpcs

An agent can call this app's rpcs as MCP tools. You choose which: nothing is
served until an rpc opts in. Each tool call is an in-process call of the rpc,
through the same interceptors as any other client: auth, validation, rate
limits, audit, tracing. The proto stays the one source: the tool's name, its
description and its input schema all come from the rpc.

(`forge mcp` on its own is different: it serves forge's own tools to your
coding agent. This skill is about your app's tools.)

## Opt in

```proto
service OrderService {
  option (forge.v1.service) = { mcp: true };   // every unary rpc, by default

  // Lists the caller's orders, newest first. Call it to find an order id
  // before acting on an order; page with page_token. Returns an empty list
  // when the caller has none.
  rpc ListOrders(ListOrdersRequest) returns (ListOrdersResponse) {
    option idempotency_level = NO_SIDE_EFFECTS;  // announced readOnlyHint
  }

  // Cancels one open order and releases its stock. Fails with
  // FAILED_PRECONDITION when the order has already shipped.
  rpc CancelOrder(CancelOrderRequest) returns (CancelOrderResponse) {
    option (forge.v1.method) = { mcp_mutating: true };  // served, destructiveHint
  }

  rpc RebuildIndex(RebuildIndexRequest) returns (RebuildIndexResponse) {
    option (forge.v1.method) = { mcp: false };   // opted out of the service default
  }
}
```

- `(forge.v1.method).mcp` wins over `(forge.v1.service).mcp`; unset inherits.
- **Mutating rpcs need `mcp_mutating: true` as well.** An rpc mutates unless
  its name is a read verb or it declares `idempotency_level = NO_SIDE_EFFECTS`
  (the same verdict the activity stream uses). Without the acknowledgement it
  is not served, whatever `mcp` says, and lint says why. A served mutation is
  announced `destructiveHint`, so clients ask before calling it.
- `readOnlyHint` needs `NO_SIDE_EFFECTS`, not just a read verb: clients skip
  confirmation on it, so it must be declared.
- Streaming rpcs cannot be tools (MCP tools are request/response); `forge
  generate` refuses `mcp` on one. Set `mcp: false` on it under a service opt-in.
- A public rpc (`auth_required: false`) is served only when it opts in too.

Run `forge generate` after changing an opt-in.

## Tool names are public API

A tool is named `<service>_<rpc>` in snake case, the `Service` suffix dropped:
`OrderService.ListOrders` → `order_list_orders` (at most 64 characters of
`[a-z0-9_]`). Agents and prompts refer to tools by name, so **treat renaming
an rpc as renaming a tool**: the same compatibility care as any rpc rename
(add the new rpc, deprecate the old one). Two rpcs that project to one name
fail `forge generate`.

## Write tools a model can use

A model picks a tool from its name and description alone, and fills the
input from field names and comments.

- **Serve the 5 to 30 operations an agent needs, not the API.** Prefer coarse,
  task-shaped rpcs (`SummarizeAccount`) over a CRUD quintet per entity. Past
  40 tools on one listener, models choose worse and every tool's schema costs
  context; lint warns.
- **Write the rpc's leading comment as when-to-call guidance**: what it does,
  when to call it, and its error cases. Not "ListOrders lists orders."
- **Comment every request field**: what it holds, its format, its default.
- **Keep list rpcs paginated**: tool results are bounded.

## Lint (warn-first)

`forge lint --conventions` runs three rules, warnings in v1.13.0 that gate from
v1.14.0:

| Rule | Finds |
|---|---|
| `forgeconv-mcp-described` | an opted-in rpc with no comment, one under 8 words or one that restates its name; request fields with no comment |
| `forgeconv-mcp-mutating` | `mcp` on a mutating rpc without `mcp_mutating: true` (not served), or `mcp_mutating` on an rpc that does not mutate |
| `forgeconv-mcp-shape` | `mcp` on a streaming rpc; two rpcs with one tool name; more than 40 tools on one listener (a warning) |

## Serving and connecting

`/mcp` is mounted on the app's existing API listener (MCP streamable HTTP,
stateless) by the runtime that lands with the `/mcp` serving release: `forge
generate` emits `pkg/middleware/mcp_tools_gen.go` (an empty table when nothing
opts in), and the scaffolded `internal/app/providers.go` passes it to the API
listener:

```go
MCP: &mcpkit.Config{Tools: middleware.MCPTools},
```

**A project scaffolded before that release** must add that one line to its
providers.go `ListenerDeps` itself (providers.go is yours; forge never
rewrites it). `/mcp` is mounted only when the table is non-empty and the env
has not turned it off. To turn it off in one env, set `MCP_ENABLED=false` in
that env's env vars in `deploy/kcl/<env>/main.k` (default true).

Then:

```bash
forge mcp app dev            # the /mcp URL, every tool, a snippet per client
forge mcp app prod --json    # the same as one document (schema_version 1)
forge mcp app prod --url https://api.example.com   # name the API yourself
```

It prints the endpoint, each tool marked `(read-only)` or `(destructive)`, and
ready config for Claude Code (`claude mcp add --transport http …`), Codex,
Cursor and VS Code.

**Auth is a bearer token** for now (OAuth comes later): whatever token the
app's API accepts, sent as `Authorization: Bearer $MCP_TOKEN`. Tool calls run
as that token's caller, so the rpc's own access checks apply. How this app
issues tokens: `forge skill load auth`.
