---
name: frontend/mock-mode
description: Connect RPC clients in a forge frontend and opt-in mock mode — the generated transport in src/lib/connect.ts, mock_api ("", "true", "hybrid") and the signed-in user each gives, the mock banner, where the mock pipeline lives (fixtures, scenarios), dispatch order, and stubbing an RPC with forge scaffold scenario.
---

# Connect RPC clients + mock mode

Import the generated transport from `src/lib/connect.ts`; for direct calls `createClient(MyService, transport)`. It talks to the REAL backend through `src/lib/api-base.ts` (`frontend/serving`). Mock mode is **opt-in**: set `mock_api` in `deploy/kcl/<env>/config.k`, then `forge generate`.

| `mock_api` | RPCs | Signed-in user |
|---|---|---|
| `""` (default) | Real backend. | `GET /auth/session` |
| `"true"` | Fixtures only; "MOCK DATA" banner. | Fixture `dev@localhost`, no `/auth/session`, so guards pass. |
| `"hybrid"` | `?scenario=` handlers over the real backend. | Same fixture, UI only: forwarded RPCs carry just the real cookie. |

**Never remove the mock banner from the layout** — it stops a working-looking UI masquerading as a working stack.

## Where the mock pipeline lives

The engine is library code (`@reliantlabs/forge-web-runtime/mock-transport`, a subpath so production bundles shake it out). Your project keeps:

- **`src/lib/mock-transport_gen.ts`** (Tier-1) — gathers the per-entity `src/mocks/entities/*_gen.ts`.
- **`src/mocks/<entity>_gen.ts`** — the fixtures, the same rows `forge db seed apply` writes.
- **`src/mocks/scenarios/*.ts`** — your scenarios; forge only regenerates the `index_gen.ts` barrel.

Dispatch: scenario handler → hybrid passthrough → fixtures → `Unimplemented`. A Get miss is `NotFound`. Writes persist until reload. A fixture List mirrors the backend's: each SET `optional` request field filters by equality (enums by value), `search` matches strings and enum names, `order_by`/`descending` sort, `page_size` pages, and `total_count` is the filtered count. Custom RPCs need a scenario.

To stub an RPC, run `forge scaffold scenario <name>` and add a typed handler — do NOT edit `mock-transport_gen.ts`. Activate with `?scenario=<name>`; it is read once at module init, so client-side navigation keeps it active until a full reload.
