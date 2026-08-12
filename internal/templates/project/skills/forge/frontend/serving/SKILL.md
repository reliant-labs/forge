---
name: serving
description: How a forge frontend is built and served — the three output shapes (standalone/static/server), fenced build dirs, and mounting under a URL prefix with base_path and joinBasePath.
---

# Frontend build & serving shapes

## Production build shape (`output:`)

`forge scaffold frontend` emits a `next.config.ts` captured in `forge.yaml` as
`output:`. Opt in at scaffold time (`forge scaffold frontend dashboard --output
static`); three values:

| `output:`    | Production shape | Use when |
| ------------ | ---------------- | -------- |
| `standalone` | Node sidecar (`output: "standalone"`) | **The default.** Pairs with the shipped Dockerfile; supports the generated dynamic CRUD routes, server components/actions, request-time `redirect()`/`cookies()`. |
| `static`     | Static export (`output: "export"`) | Pure UI shell with NO dynamic routes — drop `out/` on a CDN. |
| `server`     | Full Next.js (no `output:`) | Custom server, ISR, managed host (Vercel). |

**FOOTGUN: `static` is incompatible with generated CRUD pages.** `output:
"export"` requires `generateStaticParams()` on every dynamic segment, and the
generated detail/edit pages (`/<entity>/[id]`) are dynamic client routes whose
ids only exist at runtime, so `npm run build` fails on any project with a CRUD
entity. Server-runtime APIs (`redirect()` from `next/navigation`, `cookies()`,
server actions) also require `standalone` or `server`; for a root redirect under
static export use a client component with `useRouter().replace()` in a
`useEffect`.

**Build dirs are fenced.** In `standalone`/`server`, production builds write to
`.next-prod` while `next dev` keeps `.next`, so `npm run build` during a live
`forge env up` session can't clobber the dev cache; `next start` and the
Dockerfile read `.next-prod`. `static` keeps Next.js defaults, so avoid
production builds during a live dev session in that mode. `output:` takes effect
only at scaffold time (`next.config.ts` is yours to edit after).

## Serving under a path prefix (`base_path`)

To mount a frontend under a URL prefix, declare `base_path: /admin` in
`forge.yaml` (or `--base-path /admin`; must start with `/`, no trailing `/`).
What it drives:

- `next.config.ts` sets **both `basePath` AND `assetPrefix`** — `assetPrefix` is
  required or some RSC chunk URLs skip the prefix and React never hydrates.
- **ONE env var**: `NEXT_PUBLIC_BASE_PATH`. A second variant
  (`ADMIN_WEB_BASE_PATH` etc.) is silently ignored.
- `src/lib/basepath_gen.ts` (regenerated) exports `BASE_PATH` +
  `joinBasePath(path)`.
- Static-export builds **fail loudly** if `NEXT_PUBLIC_BASE_PATH` is emptied
  while `forge.yaml` declares a prefix.

Internal navigation (`<Link href="/tasks">`, `router.push("/tasks")`) keeps
app-relative paths — Next.js prepends the basePath, so do NOT wrap these in
`joinBasePath`. Hand-built URLs Next.js can't see — `window.location.origin`-based
return URLs, OAuth `redirect_uri`, share links, raw `fetch()`/`<a>` paths — MUST
go through it:

```typescript
import { joinBasePath } from "@/lib/basepath_gen";
const successUrl = window.location.origin + joinBasePath("/billing/success");
```

Anti-patterns lint catches: bare `"/admin" + path` literals, bare `/route`
strings in hand-built URLs, and reading any env var other than
`NEXT_PUBLIC_BASE_PATH`. `src/lib/admin-url.ts` (`adminUrl` /
`absoluteAdminUrl`) wraps the same helper for strings handed to an external
system that round-trips back.

See also: `frontend` for the rest of the frontend surface, `deploy` for how the
built artifact ships.
