---
name: observability/release-probes
description: Release probes — steady synthetic traffic the platform sends a hosted env's frontends and workloads so a release can be judged when real traffic is too thin. `forge.Probe` in KCL (`release_probe` on a forge.Frontend or fw.Workload — not `probes`, which is liveness/readiness), the default probe every hosted static site gets at its root, opting a service in (GET/HEAD paths only), the bounds forge refuses at render, and what lands in the release shape (`release_probes`).
---

# Release probes

`forge obs check` judges a release on the traffic it served. A site or a
service with few visitors has too few requests to judge (`min_requests`), so
its verdict stays INCONCLUSIVE however healthy it is. A release probe gives
the judge evidence that does not depend on visitors: the platform requests a
few declared paths, from outside the platform cluster and through the public
hostname (DNS, TLS, the CDN, the proxy), a few times a minute, and records
every outcome in the env's telemetry.

Probe requests are signed and judged apart from real traffic: they never count
toward a service's request rate, error rate or latency. Any probe failure after
a promotion fails the release (`probe_failed`, in `fail_on` by default), and a
release with too little real traffic can pass on probes alone; the verdict then
says so (`judged_by = "probe"`).

The platform turns probing on per org and env. Declaring a probe, or getting the
default one, records it in the release shape; until probing is on for the env,
nothing is requested and verdicts are unchanged.

## What you get with no declaration

| thing | probe |
|---|---|
| `forge.OnHosted` frontend | `GET <base_path>/` (`/` with no `base_path`), 4 a minute, 10s timeout, 2xx or 3xx is success |
| `forge.OnHosted` workload | nothing: a workload is probed only when it declares one |
| any other runtime | nothing |

Only an env the control plane probes renders probes: one with a
`control_plane` that is not `lifecycle = "local"` or `"ephemeral"`. Everywhere
else a declaration is ignored, not refused, so one base declaration in
`workloads.k` serves every env.

## Declaring one

```python
# a service: opt in, GET/HEAD paths that are safe to request forever
wl.api | {release_probe = forge.Probe {paths = ["/healthz", "/flow-health"]}}

# a frontend: replace its default probe
forge.Frontend {
    name = "web"
    # ...
    release_probe = forge.Probe {paths = ["/", "/pricing"]}
}

# a frontend: turn its default probe off
release_probe = forge.Probe {enabled = False}
```

| field | default | bounds |
|---|---|---|
| `enabled` | `True` | `False` renders nothing for the target |
| `paths` | `["/"]` | 1 to 5, each starting with `/`, at most 256 characters, no query, fragment, `..`, `//`, `.` segment or percent-encoding, only RFC 3986 path characters (`A-Z a-z 0-9 -._~!$&'()*+,;=:@/`); requested in turn, in this order. A frontend's `base_path` is not prepended |
| `method` | `GET` | `GET` or `HEAD` only |
| `rate_per_min` | `4` | 1 to 12 requests a minute across all paths. The control plane may lower it (its per-target cap, 4 today); the verdict reports the rate it used |
| `timeout` | `10s` | 1s to 30s, and shorter than one interval (60s / `rate_per_min`) |
| `expect_status` | 2xx and 3xx | statuses (`"200"`) or classes (`"2xx"`). Any other status is a failure, except 429, which is throttling and never a failure |

A probe carries no body, cookie or credential, so declare only public paths a
stranger could request many times a minute without changing anything. A
workload must expose a port (`expose = True`) to be probed: the platform has
no hostname for one that does not, and a declaration on such a workload fails
the render, naming it.

**`release_probe`, not `probes`.** `fw.Workload.probes` is the workload's
liveness and readiness (`fw.Probes`), run inside the cluster. The two types
differ, so a mix-up fails the render.

## What the control plane receives

The render resolves every probe into the release shape's `release_probes`,
sorted by target, every default written out (`expect_status` left out when it
is the default):

```json
{"target": "web", "kind": "frontend", "paths": ["/"], "method": "GET",
 "rate_per_min": 4, "timeout_seconds": 10, "source": "default"}
```

`source` is `default` for a frontend's derived probe and `user` for a declared
one. `forge env render <env>` shows it under `output.release_probes`.

A shape recorded before release probes existed carries none, and the control
plane never derives one for it: a project starts being probed when it deploys
with a forge that renders the field. A control plane that predates the field is
sent the shape without it, and the deploy succeeds unprobed.

## Related

- `observability/release-health` — how `forge obs check` judges a release, and
  `fail_on` (`probe_failed`, `probe_latency`).
- `observability/uptime` — outside-in checks that alert while an env is down;
  release probes are the evidence a release verdict is judged on.
