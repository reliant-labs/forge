---
name: pages
description: Cookbook — scaffold a CRUD page with React Query hooks + Zustand state + forge component library + Tailwind v4.
---

# Frontend Pages Cookbook

End-to-end recipe for adding a typed CRUD page to a forge frontend. Follow it linearly; every step has a "see also" pointing at deeper material.

The example walks through adding `app/users/page.tsx` for a hypothetical `UserService`. Substitute your domain — the recipe is mechanical.

## Step 1 — define the proto entity + RPCs

In `proto/services/users/v1/users.proto`:

```proto
syntax = "proto3";
package myproject.services.users.v1;

import "forge/v1/forge.proto";
import "google/protobuf/timestamp.proto";

message User {
  option (forge.v1.entity) = { table_name: "users", timestamps: true };

  string id = 1 [(forge.v1.field) = { pk: true }];
  string org_id = 2 [(forge.v1.field) = { tenant: true }];
  string email = 3 [(forge.v1.field) = { store: true, unique: true }];
  string name = 4 [(forge.v1.field) = { store: true }];
  google.protobuf.Timestamp created_at = 100 [(forge.v1.field) = { store: true }];
}

service UserService {
  option (forge.v1.service) = { name: "users" version: "v1" };

  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse) {
    option (forge.v1.method) = { auth_required: true };
  }
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse) {
    option (forge.v1.method) = { auth_required: true };
  }
  rpc GetUser(GetUserRequest)   returns (GetUserResponse)   { option (forge.v1.method) = { auth_required: true }; }
  rpc UpdateUser(UpdateUserRequest) returns (UpdateUserResponse) { option (forge.v1.method) = { auth_required: true }; }
  rpc DeleteUser(DeleteUserRequest) returns (DeleteUserResponse) { option (forge.v1.method) = { auth_required: true }; }
}

message CreateUserRequest  { string email = 1; string name = 2; }
message CreateUserResponse { User user = 1; }
message ListUsersRequest   { int32 page_size = 1; string page_token = 2; optional string search = 3; }
message ListUsersResponse  { repeated User users = 1; string next_page_token = 2; }
message GetUserRequest     { string id = 1; }
message GetUserResponse    { User user = 1; }
message UpdateUserRequest  { string id = 1; string name = 2; }
message UpdateUserResponse { User user = 1; }
message DeleteUserRequest  { string id = 1; }
message DeleteUserResponse {}
```

See `proto` for annotation rules and the full reference. `forge lint --conventions` enforces the structural rules.

## Step 2 — generate

```bash
forge generate
```

This produces:

- Go service stubs and CRUD handler implementations (`handlers_crud_gen.go` for matching method names — see `api`).
- Generated React Query hooks: `useListUsers`, `useGetUser`, `useCreateUser`, `useUpdateUser`, `useDeleteUser` in `frontends/<name>/src/hooks/users-hooks.ts`.
- Generated Connect transport in `src/lib/connect.ts`.

Never hand-edit `*-hooks.ts` — overwritten on next `forge generate`. Add custom hooks in a separate file (`src/hooks/custom-hooks.ts`).

## Step 3 — build the page

```tsx
// frontends/web/src/app/users/page.tsx
"use client";

import { create } from "@bufbuild/protobuf";
import { useState } from "react";

import {
  useListUsers,
  useCreateUser,
  useDeleteUser,
} from "@/hooks";
import { CreateUserRequestSchema } from "@/../../gen/myproject/services/users/v1/users_pb";

import DataTable from "@/components/ui/data_table";
import Button from "@/components/ui/button";
import Card from "@/components/ui/card";
import Input from "@/components/ui/input";
import { useUiStore } from "@/stores/ui-store";

export default function UsersPage() {
  const [search, setSearch] = useState("");
  const showToast = useUiStore((s) => s.showToast);

  const { data, isLoading, error } = useListUsers({
    pageSize: 20,
    search: search || undefined,
  });
  const createUser = useCreateUser();
  const deleteUser = useDeleteUser();

  if (isLoading) return <div className="p-8">Loading…</div>;
  if (error)    return <div className="p-8 text-red-600">Error: {error.message}</div>;

  return (
    <div className="p-8 space-y-6">
      <header className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Users</h1>
        <Button
          onClick={async () => {
            try {
              await createUser.mutateAsync(
                create(CreateUserRequestSchema, {
                  email: "new@example.com",
                  name:  "New User",
                }),
              );
              showToast({ kind: "success", message: "User created" });
            } catch (err) {
              showToast({ kind: "error", message: (err as Error).message });
            }
          }}
        >
          New user
        </Button>
      </header>

      <Card className="p-4">
        <Input
          placeholder="Search by name…"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
      </Card>

      <DataTable
        columns={[
          { key: "name",  header: "Name" },
          { key: "email", header: "Email" },
          {
            key: "actions",
            header: "",
            render: (u) => (
              <Button
                variant="ghost"
                onClick={() => deleteUser.mutate({ id: u.id })}
              >
                Delete
              </Button>
            ),
          },
        ]}
        data={data?.users ?? []}
      />
    </div>
  );
}
```

A few patterns to copy:

- **`"use client"` only when you need it.** This page uses hooks and event handlers, so client.
- **`create(Schema, {...})` for proto messages.** Never `new MessageType(...)` — that's protobuf-es v1; we're on v2.
- **`useListUsers({ pageSize, search })` direct.** The hook takes a plain object; pass the search filter as `undefined` (not empty string) when not set.
- **Three states: loading / error / success.** Always.
- **Forge component library first.** Use `component_library(action="search", query="...")` before hand-rolling. See `frontend`.

## Step 4 — Zustand for client state, if needed

For state that lives only on the client (sidebar collapse, modal open, toast queue), extend an existing store or create a domain store in `src/stores/`:

```ts
// src/stores/users-store.ts
import { create } from "zustand";

interface UsersUiState {
  selectedIds: string[];
  toggleSelect: (id: string) => void;
  clearSelection: () => void;
}

export const useUsersUiStore = create<UsersUiState>((set) => ({
  selectedIds: [],
  toggleSelect: (id) =>
    set((s) => ({
      selectedIds: s.selectedIds.includes(id)
        ? s.selectedIds.filter((x) => x !== id)
        : [...s.selectedIds, id],
    })),
  clearSelection: () => set({ selectedIds: [] }),
}));
```

In the component, **subscribe to slices, not the whole store**, so re-renders stay tight:

```ts
const selectedIds  = useUsersUiStore((s) => s.selectedIds);
const toggleSelect = useUsersUiStore((s) => s.toggleSelect);
```

Server data does NOT belong in Zustand. The `data` from `useListUsers` is already cached by React Query — copying it into a store creates a stale-data bug waiting to happen. See `frontend/state` for the full ownership table.

## Step 5 — styling with Tailwind v4

Use utility classes directly. The project ships Tailwind v4, configured via `@theme` in `src/app/globals.css` — no `tailwind.config.js`. See `frontend` for v4 specifics, including the `@import "tailwindcss"` rule and `@tailwindcss/postcss` plugin.

For the tokens you'll repeat — colors, spacing, radii — extend the `@theme` block. Do NOT hard-code one-off colors across multiple components.

## Step 6 — wire the route

Next.js App Router auto-routes anything under `src/app/`. Adding `src/app/users/page.tsx` creates `/users`. To gate the page behind auth, the auth provider in `src/lib/auth/` already runs at layout level — components can read `useAuth()` for current user / token.

For navigation links, use the project's existing nav component (likely in `src/components/layout/`).

## Step 7 — verify visually

```bash
forge run
```

Open the page in a browser. Use Chrome DevTools' MCP integration:

```
take_snapshot()      # element tree, accessibility
take_screenshot()    # actual rendered pixels
```

**Both.** Snapshots miss CSS bugs (overflow, z-index, broken responsive). Screenshots miss accessibility regressions. Run both at every breakpoint you support.

## Common mistakes

1. **`new CreateUserRequest({...})`** — protobuf-es v1 syntax. Use `create(Schema, {...})`.
2. **Copying server data into Zustand.** React Query already caches it. Subscribe to the query, don't duplicate it.
3. **Using `useState` for form state on a non-trivial form.** Use `react-hook-form` + Zod schema. See `frontend/patterns`.
4. **Forgetting `"use client"`** on a page that uses hooks. Build error.
5. **Hand-editing `src/hooks/users-hooks.ts`.** Overwritten on next `forge generate`.
6. **Skipping screenshots.** Snapshots compile, tests pass, layout is broken in the browser.
7. **`mutate(...)`** without an error handler. Wrap in try/catch (or `mutateAsync` + try/catch) and surface failures via the event bus or a toast.

## Rules

- Generated hooks (`use<Method>`) come from `forge generate`. Never hand-edit `*-hooks.ts`.
- Always `create(Schema, {...})` for proto messages.
- Three states (loading, error, success) on every data-fetching page.
- Server data lives in React Query; client UI state lives in Zustand. Never mix.
- Subscribe to Zustand slices, not the whole store.
- Component library before custom UI. `component_library(action="search", ...)`.
- Verify visually with both `take_snapshot` and `take_screenshot` before declaring done.
- Tailwind v4 only — no `tailwind.config.js`, no `@tailwind base/components/utilities`.

## When this skill is not enough

- **State management decision tree** (URL vs Zustand vs query vs ref) — see `frontend/state`.
- **Component composition / container-presentational patterns** — see `frontend/patterns`.
- **The proto and codegen side** of the recipe — see `proto` and `api`.
- **Auth wiring on the frontend** (Auth0, Clerk, Supabase) — see `auth` and `packs`.
- **Mobile (React Native / Expo)** — see `frontend`. The hook layer is shared; the UI patterns differ.
