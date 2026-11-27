---
name: observability/edge
description: Edge request logs of a hosted static site — every request Reliant hosting's CDN served (cache hits, crawlers and AI agents the browser SDK never sees), in the tenant tables edge_requests and edge_requests_1d. `forge obs traffic` (totals, top pages, --by page|status|agent|referrer|country, --class, --site, --json forge.obs.traffic/v1), the frontend dashboard's Traffic and Bots & agents groups, querying the tables with `forge obs query`, the limits and the opt-out.
---

# Edge request logs

A frontend bound to Reliant hosting (`forge.OnHosted`) sits behind the
platform's CDN, and the control plane records every request the CDN served,
cache hits and AI agents included, into the env's telemetry: the tenant tables
`edge_requests` (one row per request) and `edge_requests_1d` (its daily
rollup). The browser SDK never sees these requests: a cache hit never reaches
the origin and an agent runs no JavaScript.

- `forge obs traffic prod` — totals per site (requests, cache-hit ratio, 4xx,
  5xx, bytes) and the top pages split into humans, agents and crawlers.
  `--by agent --class ai` lists each AI agent with its operator, whether the
  request came from the operator's published IP ranges (verified), and its
  `.md` and `llms.txt` fetches. `--json` writes `forge.obs.traffic/v1`.
- The frontend dashboard gains **Traffic** and **Bots & agents** groups when an
  env has a hosted static frontend.
- `forge obs query prod --sql "… FROM edge_requests …"` for anything else:
  filter `EnvId` with `{env_ids:Array(String)}` and count `sum(SampleWeight)`
  (`sum(Requests)` on the rollup), so counts stay true when ingest is sampled
  under a quota. Columns include `Path`, `Status`, `CacheStatus`, `Bytes`,
  `RefererHost`, `UaClass`, `Agent`, `AgentOperator`, `AgentVerified`,
  `ContentKind` (page, markdown, llms, robots, sitemap, asset, other),
  `ClientNet` (a /24 or /48, never a full IP) and `Country`.

Limits: edge logs exist only on Reliant hosting; `forge env up` has no CDN, so
a local env answers "could not look". They carry no visitor identity: unique
visitors stay the browser SDK's number. An env opts out with
`forge.Observability { signals = forge.ObservabilitySignals { edge = False } }`
(the KCL field is arriving in a coming forge release).

