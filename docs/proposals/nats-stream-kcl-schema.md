# Declarative NATS JetStream provisioning via `forge.NATSStream`

- **Status**: proposed
- **Author**: cp-forge NATS agent (2026-06-08 post-port audit)
- **Related**: `pkg/runtime` CRD-provisioning runtime hook (existing precedent for "ensure-at-boot" shape), `internal/codegen/wire_gen.go` (where the hook would be invoked from)
- **Touches**: `kcl/schema.k` (new `NATSStream` schema + `Bundle.nats_streams`), `kcl/render.k` (project the streams into Application reflection or a sidecar ConfigMap), `pkg/runtime/nats.go` (new — `EnsureStreams` helper), generated `pkg/app/bootstrap.go` (call the helper after NATS connect).

## Context

Every forge project that uses NATS JetStream repeats the same boilerplate:

1. A package-local `internal/natsio/streams.go` declaring stream specs as a `[]jetstream.StreamConfig`.
2. An `EnsureStreams(ctx, js)` function called once at admin-server boot before any consumer starts.
3. Hand-maintained subjects, retention, storage, replicas, max-age values that drift across environments because the test/staging/prod defaults aren't first-class.

cp-forge's `internal/natsio/streams.go` declares 6 streams (DAEMON_EVENTS, DAEMON_PENDING_COMMANDS, DAEMON_STATE, WORKSPACE_COMMANDS, WORKSPACE_EVENTS, WORKSPACE_ACTIVITY). The shape is mechanical — every project with NATS will replicate it. forge already provisions Kubernetes CRDs via a runtime hook (`pkg/runtime` ensures CRDs from a typed manifest list at boot); JetStream streams fit the same model.

## The class of bugs the current shape produces

- **Drift**: dev declares replicas=1 file, staging declares replicas=3 file, prod gets whatever someone typed into a `nats stream add` shell command three months ago. No single source of truth.
- **Silent runtime failures**: a consumer subscribes to `EVENTS.workspace.*` before the WORKSPACE_EVENTS stream exists, then the publisher publishes, then `consumer.Next()` times out at 30s in a test. No "stream missing" error path — JetStream just doesn't deliver.
- **Drift between Go const and stream subject pattern**: `const StreamWorkspaceEvents = "WORKSPACE_EVENTS"` and the subjects `["events.workspace.>"]` live in two places that have to stay in lockstep manually.
- **Cross-env config gaps**: staging needs 7d retention, prod needs 30d, dev needs 1h. The current shape forces this into a runtime env-var read inside `streams.go` or a per-env build.

## Proposed change

### KCL schema

Add to `kcl/schema.k`:

```python
schema NATSStream:
    """Declarative JetStream stream spec — provisioned at boot by pkg/runtime.EnsureStreams.

    Renders into the generated bootstrap so the stream exists before any consumer
    starts. Subjects MAY include the `>` and `*` wildcards JetStream understands.
    """
    name: str                                       # canonical stream name (UPPER_SNAKE)
    subjects: [str]                                 # subject patterns this stream captures
    retention: "limits" | "interest" | "workqueue" = "limits"
    storage: "file" | "memory" = "file"
    replicas: int = 1
    max_age?: str                                   # Go duration form: "168h", "30d" (parsed at runtime)
    max_bytes?: int                                 # byte cap; -1 / omitted = unlimited
    discard: "old" | "new" = "old"                  # what to drop when the cap hits
```

And extend the project bundle:

```python
schema Bundle:
    # ... existing fields ...
    nats_streams: [NATSStream] = []
```

### Per-env config

The per-env override pattern stays consistent with the existing KCL config story (`environments[].config`):

```python
# kcl/dev/manifests.k
bundle = Bundle {
    nats_streams = [
        NATSStream { name = "WORKSPACE_EVENTS"
                     subjects = ["events.workspace.>"]
                     replicas = 1
                     max_age = "1h" },
        # ...
    ]
}
```

```python
# kcl/prod/manifests.k
bundle = Bundle {
    nats_streams = [
        NATSStream { name = "WORKSPACE_EVENTS"
                     subjects = ["events.workspace.>"]
                     replicas = 3
                     max_age = "720h"
                     storage = "file"
                     max_bytes = 10_737_418_240 },  # 10 GiB
    ]
}
```

### Runtime helper

New file `pkg/runtime/nats.go`:

```go
// EnsureStreams is the boot-time JetStream provisioner. Generated bootstrap
// calls it after the NATS connection is established and before any consumer
// starts. Idempotent: existing streams are updated to match spec; missing
// streams are created. A drift between spec and existing stream surfaces
// as an UpdateStream error (NATS returns "stream config changes not allowed
// for limits/interest retention" for some transitions) rather than a silent
// data-loss event.
type StreamSpec struct {
    Name      string
    Subjects  []string
    Retention jetstream.RetentionPolicy
    Storage   jetstream.StorageType
    Replicas  int
    MaxAge    time.Duration
    MaxBytes  int64
    Discard   jetstream.DiscardPolicy
}

func EnsureStreams(ctx context.Context, js jetstream.JetStream, specs []StreamSpec) error
```

### Codegen integration

The bootstrap generator (currently `internal/codegen/wire_gen.go` + the bootstrap.go template) gains a step:

1. Parse the rendered KCL manifest (already happens for Application reflection).
2. If `nats_streams` is non-empty AND the project's NATS adapter is wired, emit a `runtime.EnsureStreams(ctx, js, streamSpecs)` call into bootstrap.go's NATS-init block.
3. The `streamSpecs` literal is rendered from the KCL spec, so the source of truth stays in KCL.

This mirrors how the operator CRD-provisioning hook works today: KCL declares CRDs → render writes a manifest → bootstrap calls `runtime.EnsureCRDs`.

## What this would mean for cp-forge

The 6 streams in `internal/natsio/streams.go` would move to `kcl/dev/manifests.k` + `kcl/staging/manifests.k` + `kcl/prod/manifests.k` (or a shared `kcl/streams.k` imported by all three). The hand-written `EnsureStreams()` and the `Bootstrap` call site go away. Per-env values (replicas, retention) become first-class instead of env-var-conditional.

A migration would be a `forge upgrade v0.x → v0.y` codemod that reads `internal/natsio/streams.go`'s `[]jetstream.StreamConfig` literal, projects it into KCL, and deletes the Go file.

## Why proposal-only right now

The other forge agent has 156 dirty files including `kcl/schema.k`, `kcl/render.k`, `kcl/base.k`, plus build-target and ingress work that touches `internal/templates/deploy/kcl/**`. Landing a `NATSStream` schema + render path + runtime helper + codegen integration without coordinating against the gateway / external-build refactor would near-certainly produce merge conflicts at the KCL render boundary. Once the sibling work lands, this proposal can be implemented in a focused pass: ~150 lines of KCL, ~80 lines of Go runtime helper, ~40 lines of bootstrap template extension, plus a migration codemod.

## Open questions

1. **Should streams move under `services[].nats_streams` (per-service ownership) or stay at the bundle level (shared)?** cp-forge's 6 streams are owned by different services (DAEMON_* by admin-server, WORKSPACE_* by workspace-proxy). Per-service ownership would let `forge add service --with-nats-stream FOO` work; bundle-level matches the current "infrastructure shared across binaries" shape.
2. **Drift handling**: when a stream exists with `replicas=1` and the KCL declares `replicas=3`, do we update (potentially expensive replication catch-up) or warn-and-skip? Default proposed: warn-and-skip with `--force-stream-update` opt-in, matching how CRD-version-mismatches are handled.
3. **Consumer specs**: same proposal could extend to `NATSConsumer` (durable consumer name + filter subject + max-deliver). Out of scope for v1; the consumer side is currently mostly hand-rolled in handler code and migrating it is a separate effort.
