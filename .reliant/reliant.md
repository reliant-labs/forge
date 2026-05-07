# forge

## IMPORTANT NOTICES

This project has not launched yet. There are no users and no backwards compatibility requirements.

- Do NOT add fallbacks that mask bugs — fix the underlying issue instead
- Do NOT maintain backwards compatibility — opt for clean new implementations
- Do NOT add defensive code for scenarios that can't happen
- Do NOT keep old code paths "just in case" — delete what's not needed
- When something breaks, patch the root cause, don't paper over it

## Architecture

This is a Forge-generated Go + Next.js application with:
- **Backend**: Go with Connect RPC, JWT auth, RBAC, PostgreSQL + ORM
- **Frontend**: Next.js with TypeScript, Tailwind CSS, Connect RPC client
- **Code Generation**: Proto files drive handler, ORM, migration, and frontend generation

## Key Conventions

- Entity protos go in `proto/services/<service>/v1/`
- Generated code lives in `gen/` and `*_gen.go` files — do NOT edit these
- Custom business logic goes in handler files (not `*_gen.go`) and `pkg/app/setup.go`
- Frontend hooks in `frontends/*/src/hooks/` are generated — use them, don't write manual fetch calls
- Run `forge generate` after proto changes, then `forge build` to verify

## Frontend Architecture

- **Server state**: Generated React Query hooks (`src/hooks/*-hooks.ts`) — do not copy into Zustand
- **Client UI state**: Zustand stores in `src/stores/` — subscribe to slices, not whole stores
- **Auth**: DI'd via `AuthProvider` interface — swap providers without changing components
- **Events**: Typed event bus (`src/lib/events.ts`) for imperative actions (toasts, navigation) — events are moments, not truth
- **Forms**: react-hook-form + Zod schemas — do not hand-roll with useState
- **URL state**: App Router params for anything that should survive refresh/bookmarking
- **Context**: Only for stable, broad dependencies (auth, theme, event bus) — not high-frequency data
- **Derive, don't effect**: Compute values during render, never useEffect to set derived state

## Do NOT Edit
- `gen/` directory
- `*_gen.go` files
- `cmd/server.go`, `cmd/otel.go`
- `pkg/app/bootstrap.go`
- `frontends/*/src/hooks/*-hooks.ts`
- `frontends/*/src/gen/`