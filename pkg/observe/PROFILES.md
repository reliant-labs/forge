# Full heap snapshots: semantics and cost

The continuous profiler exports CPU windows and goroutine snapshots about once a
minute. It now exports full heap snapshots initially and no more often than once
per 15 minutes (per running profiler). `Config.ProfilesHeapInterval` overrides
that interval; zero/negative uses the default. Scheduling uses elapsed time since
the last attempt, including failed attempts, not a catch-up queue. Process
restart naturally starts a new snapshot series. Other signal cadence is unchanged.

## Why this representation

Go heap pprof has four useful types:

- `alloc_objects/count`, `alloc_space/bytes`: cumulative lifetime allocation
  counters. Old allocation sites remain present after their objects are freed.
- `inuse_objects/count`, `inuse_space/bytes`: current live-heap gauges (as of the
  runtime's sampled heap accounting), not allocation deltas.

The converter preserves all four, their totals, units, period, timestamp and
trace links. It removes a sample only when **all values of that individual OTLP
sample type are zero**, after attaching links in the original pprof order.
Signed nonzero values remain valid. A dead live-heap stack can still have large
allocation counters; it must not be dropped from alloc profiles. An all-zero
type remains in the in-memory snapshot, with no rows; it does not justify an
export full of meaningless zeros. CPU's redundant `samples/count` handling is
unchanged.

Alternatives and trade-offs:

- **Sparse snapshots at a bounded cadence (chosen):** independent, full-fidelity
  stack distributions at each capture, no state needed to interpret the next
  capture after delivery failure. Lower temporal resolution for *all* heap
  types, explicitly 15m by default. Total per-capture size still scales with
  lifetime allocation sites; this is a rate bound, not a maximum-size promise.
  CPU/goroutine samples remain frequent, and pprof remains available for an
  operator needing an immediate heap snapshot.
- **Allocation deltas:** would avoid repeating unchanged allocation counters,
  but cannot silently reuse cumulative sample-type names/queries. The pinned
  OTLP translator/pdata has no temporality field; existing readers sum profile
  samples. A correct delta design needs explicit interval/representation,
  reset handling, delivery/loss semantics and matching query/storage changes.
  Applying subtraction to in-use gauges would instead produce signed heap
  changes, no longer a live-heap snapshot. Neither is done here.
- **Top-N, discarding allocation/object types, or dropping unchanged snapshots:**
  loses valid information or makes time-window queries depend on samples
  outside their window. Not implemented. Value-based deduplication across
  distinct capture times is also incorrect.

A failed heap export is not immediately recaptured; the next scheduled heap
still contains full cumulative counters and the then-current gauges. There is
no claim to recover live-heap history missed during an outage. Retrying the
same captured payload leaves profile IDs/values unchanged. The sender adds no
retry loop or server-side deduplication guarantee.

## Measured fixture, not a production forecast

`TestHeapVolumeEvidence` simulates 60 one-minute snapshots with 1,000→1,590
historical allocation stacks, 24 frames each, ~1% live and an entirely zero
stack. Per-type totals and identity/link behavior are checked separately in
`TestHeapSparseSnapshotsPreserveSemantics` and
`TestProfileRetryKeepsIdentityAndRows`.

| One simulated hour | Rows | Uncompressed OTLP protobuf bytes |
|---|---:|---:|
| Previous converter, every minute | 310,800 | 9,067,104 |
| Sparse converter only, every minute | 156,888 | 7,847,028 |
| Sparse converter + 15m heap cadence | 9,892 | 493,954 |

Combined reduction: **96.8% rows, 94.6% wire bytes**. Dictionary tables remain in
the wire payload, so zero filtering saves fewer wire bytes than rows. These are
not ClickHouse logical row widths, compressed disk occupancy or successful
NewPart accounting bytes; repeated stack/resource expansion can make database
savings differ. The fixture compares equal producer resources, scope and IDs;
the legacy translator retains span labels whereas the new converter lifts them
to links, a small existing conversion difference in the comparison.

Production's ~7% exact duplicate excess remains unexplained. An ambiguous
successful write followed by a retry can still create duplicates downstream.
The transport replay test proves identical payload/IDs, not exactly-once storage.
Do not attribute the observed excess to retries without bounded evidence from
the actual collector/exporter and storage path. No historical data is removed.
