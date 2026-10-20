---
name: v0.1.47
description: Upgrade a project to forge v0.1.47. One section per change — (1) the interceptor chain's Deadline slot, ahead of Auth: every unary RPC is now bounded by a 30s server deadline by default and watched for slow calls, and `(forge.v1.method).timeout` is enforced through `middleware.ProcedureDeadlines`, which an owned chain builder passes as `Deadline: serverkit.Deadline{Procedures: middleware.ProcedureDeadlines}`. Use when cmd/<bin>/cmd/serve.go or internal/app/providers.go calls serverkit.NewChain and names no Deadline, or when a project hand-wraps a timeout or watchdog interceptor around its authenticator.
version: v0.1.47
detection: for f in cmd/*/cmd/serve.go internal/app/providers.go; do test -f "$f" && grep -qs 'serverkit.NewChain' "$f" && ! grep -qsE 'Deadline:|ProcedureDeadlines' "$f" && exit 0; done; exit 1
---

# Upgrading to v0.1.47

Each section below is independent: apply the ones whose "Use when" matches.

## 1. The Deadline slot: a per-RPC deadline that bounds authentication

Use when `cmd/<bin>/cmd/serve.go` or `internal/app/providers.go` calls
`serverkit.NewChain` and its `ChainDeps` sets no `Deadline:`.

### What changed

`serverkit.ChainDeps` has a `Deadline` slot, between metrics and auth:

    trace → recovery → request-id → logging → tracing → metrics →
    deadline → auth → audit → rate-limit → extras → stream-drain → protovalidate

It runs **before** Auth, so authenticating — a token-store query, an
introspection call — runs inside the RPC's budget, and a stalled credential
lookup ends as `DeadlineExceeded` instead of holding the request until the
client gives up. `Extras` run after Auth and cannot do that, which is why it
is a slot.

**The default applies without any edit.** A nil `Deadline` is
`serverkit.Deadline{}`:

- every unary RPC is bounded by `serverkit.DefaultUnaryDeadline` (30s);
- streams are unbounded;
- a unary RPC still running after `DefaultSlowAfter` (10s) is logged at WARN
  (`rpc still in flight`), again every `DefaultSlowRepeat` (30s), and once
  more when it completes (`rpc completed slowly`).

The server budget is a ceiling: a client's shorter deadline still applies; a
longer one no longer lifts it.

`(forge.v1.method).timeout` used to be documented and read by nothing. It is
now projected into `pkg/middleware/procedures_gen.go` as
`middleware.ProcedureDeadlines` on every `forge generate`.

### What to do

1. **Find unary RPCs that legitimately run longer than 30s** — exports,
   long provisioning calls, anything waiting on a person (an OAuth sign-in).
   Declare each one's budget in its proto, next to the rpc:

   ```protobuf
   rpc Export(ExportRequest) returns (ExportResponse) {
     option (forge.v1.method) = { timeout: { seconds: 120 } };
   }
   ```

   A negative timeout (`{ seconds: -1 }`) is no server deadline.

2. **Pass the projection to your chain.** `serve.go` / `providers.go` are
   yours, so forge will not make this edit:

   ```go
   chain, err := serverkit.NewChain(serverkit.ChainDeps{
       Auth:     ...,
       Logger:   logger,
       Deadline: serverkit.Deadline{Procedures: middleware.ProcedureDeadlines},
       ...
   })
   ```

3. **Delete any hand-rolled timeout or slow-call interceptor**, in particular
   one merged into the `Auth` slot so that it would wrap authentication. Its
   budgets go in the proto (step 1) or in `Deadline.Procedures`; its default
   in `Deadline.Unary`; its thresholds in `SlowAfter` / `SlowRepeat`; a
   metric it bumped in `Deadline.OnSlow`, which replaces the WARN log (log
   and count in the one func).

4. Run `forge generate`, then `go build ./...`.

To keep the old behaviour — no server deadline at all — set
`Deadline: serverkit.NoDeadline`. That leaves a wedged handler holding its
request until the client gives up, with nothing reporting it.
