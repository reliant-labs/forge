---
name: services/shutdown
description: Graceful shutdown of a forge server — the drain sequence (readiness flip, pre-stop, listener drain, teardown), long-lived streams (SSE, websockets, Connect streams) ended inside the budget instead of holding it, the client contract for a drained Connect stream (OK + x-forge-drained, or Unavailable via RedialOnDrain), the soft "say goodbye" signal, DrainReserve, and using the same pieces from a main that runs its own listener.
---

# Graceful shutdown

`serverkit.Run` owns shutdown. On SIGTERM it runs, inside `SHUTDOWN_TIMEOUT`:

1. **Readiness flips** — `/readyz` answers 503 `{"status":"draining"}`.
2. **Pre-stop pause** (`PRE_STOP_DELAY`) — the process keeps serving while
   load balancers notice. A pod leaves EndpointSlices and cloud LB backends
   asynchronously to SIGTERM; stopping at once drops requests on every rollout.
3. **The listener drains** — it stops accepting, in-flight unary requests
   finish, and long-lived streams end (below).
4. **Workers, then operators stop; `OnShutdown` runs** (close the DB pool here);
   telemetry flushes last.

forge derives `terminationGracePeriodSeconds` from `PRE_STOP_DELAY +
SHUTDOWN_TIMEOUT`, so both are honoured exactly.

## Long-lived streams

`http.Server.Shutdown` waits for every active request. A stream that never ends
on its own — Server-Sent Events, a websocket, a server-streaming RPC — used to
hold the drain for the whole budget, log `server shutdown error: context
deadline exceeded` at ERROR, and be cut without a status at exit. Every rollout,
every replica.

The listener now owns a drain registry (`serverkit.Streams`) and ends them
inside the budget with cause `svcerr.ErrServerDraining`. Mark a plain-HTTP
streaming route as long-lived:

```go
mux.Handle("/events", serverkit.LongLived(sseHandler))
```

Its request context is then ended **as the drain begins**, after the pre-stop
pause, when this replica is already out of rotation, so the client's reconnect
lands on a live one. `serverkit.Ended(ctx)` tells a drain apart from the client
leaving. Never wrap a unary route: a rollout would cut it short instead of
draining it.

### Connect streaming RPCs

The scaffolded `serve.go` puts `serverkit.StreamDrainInterceptor()` in the
interceptor chain. It binds every server-streaming and bidi handler (unary and
client-streaming RPCs are drained normally, never cut). **The client contract**
for a stream the drain ends:

| Procedure | Client receives |
|---|---|
| default | every message sent, then **OK** with trailer `x-forge-drained: true` |
| listed in `serverkit.Config.RedialOnDrain` (a procedure `"/pkg.v1.Svc/Method"` or a whole service `"pkg.v1.Svc"`) | every message sent, then **Unavailable** ("server draining…") with the same trailer |

OK is the default because a browser client treats Unavailable as "the network
died" and may abandon every read on that connection; a watch client should
reopen the stream (from its resume cursor) when it ends with the trailer. Use
`RedialOnDrain` for peers that already redial on Unavailable, such as a daemon
on a bidi stream. A handler's own error is never rewritten.

### Saying goodbye: the soft signal

A stream with something to say before it goes — a final event, a websocket
close frame, a gateway handing its peer to a live replica — asks for the soft
signal at the top of its handler:

```go
soft := serverkit.Draining(ctx) // claims the soft phase
for {
    select {
    case <-soft:
        send(goodbye) // ctx is still live
        return nil
    case ev := <-events:
        send(ev)
    case <-ctx.Done():
        return ctx.Err()
    }
}
```

A stream that asked is left running when the drain begins, and is ended only
when it returns or at the shutdown deadline minus `DrainReserve` (default 5s,
never more than half the budget; `serverkit.Config.DrainReserve`). The reserve
is what the rest of the teardown runs in. A stream that never asked is ended at
once — waiting would only delay its client's reconnect.

## Without `serverkit.Run`

Every piece is exported for a main that runs its own listener (a worker with a
health port, a gateway):

```go
drainer := serverkit.NewDrainer(cfg)                 // readiness gate + budget
mux.HandleFunc("/readyz", drainer.ReadinessHandler(checks, cfg.ReadinessTimeout, logger))
streams := serverkit.NewStreams()
srv := serverkit.NewHTTPServer(cfg, mux, streams)    // installs streams into every request
// ... serve; mark ready once bound:
drainer.MarkReady()
// on SIGTERM:
drainer.Begin()                                      // 503 "draining", then PRE_STOP_DELAY
ctx, cancel := drainer.ShutdownContext()             // from Background, SHUTDOWN_TIMEOUT
defer cancel()
err := serverkit.DrainHTTP(ctx, srv, streams)        // Shutdown + streams, soft then hard
```

A bare `srv.Shutdown` still ends the streams that did not claim the soft phase;
`DrainHTTP` adds the reserve-bounded end of the ones that did.
