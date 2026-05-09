---
name: pack-development
description: Write a forge pack — when to use packs vs. scaffold code, the manifest schema, the install lifecycle, conventions.
---

# Pack Development

Forge packs are pre-built integrations (auth, payments, audit, SMS) that ship code into your project at install time. The code lives in your tree under `pkg/<subpath>/` and is yours to customize. The pack manifest, templates, and install lifecycle are what make this skill.

If you're consuming an existing pack, see `packs`. This skill is for *writing* new ones.

## When does pack shape make sense?

Three shapes; pick the right one:

| Shape | Use when |
|-------|----------|
| **Pack** | A reusable integration with config, dependencies, possibly migrations or proto. Multiple projects will install it. The user expects to compose it (`provider: jwt` vs `provider: clerk`). |
| **Scaffold template** | A core pattern every forge project needs at scaffold time (e.g. `pkg/middleware/audit.go` baseline). Lives in `internal/templates/project/`. |
| **Library** | A pure Go package importable as a dep, with no project-side template files or config. No install ceremony. |

Symptoms that you want a pack: it has config knobs in `forge.yaml`, it adds Go deps, it touches multiple files (validator + store + interceptor), or its template content depends on the project's module path. If none of those apply, it's a library.

Symptoms that you want a scaffold template, not a pack: every forge project needs this code at `forge new` time, with no opt-in. There is no install command — it's just there.

## Pack layout

```
internal/packs/<pack-name>/
  pack.yaml             # the manifest
  README.md             # user-facing docs surfaced by `forge pack info`
  templates/
    <file>.go.tmpl      # rendered into the consumer project
    <migration>.up.sql.tmpl
    <migration>.down.sql.tmpl
```

Pack name is hyphenated (`jwt-auth`, `audit-log`, `api-key`). Paths inside the consumer project are snake or whatever the language wants — forge handles the translation.

## `pack.yaml` schema

```yaml
name: jwt-auth                          # hyphenated id, must match dirname
version: 1.0.0                           # semver
description: |
  Production-ready JWT authentication with JWKS, dev-mode bypass,
  and multi-provider support. Code is installed into
  pkg/middleware/auth/jwtauth/ — wire it in pkg/app/setup.go.
subpath: middleware/auth/jwtauth         # under pkg/, where pack code lands

# What the pack adds to forge.yaml on install.
config:
  section: auth
  defaults:
    provider: jwt
    jwt:
      signing_method: RS256
      jwks_url: ""
      issuer: ""
      audience: ""
    dev_mode: true

# Files rendered at install time.
files:
  - template: jwt_validator.go.tmpl
    output:   pkg/middleware/auth/jwtauth/validator.go
    overwrite: always                    # regenerated every `forge generate`

  - template: dev_auth.go.tmpl
    output:   pkg/middleware/auth/jwtauth/dev_auth.go
    overwrite: once                      # written on install only — yours to edit

# Go modules added to go.mod (resolved by `go mod tidy`).
dependencies:
  - github.com/golang-jwt/jwt/v5
  - github.com/MicahParks/keyfunc/v3@v3.8.0

# Files rendered every `forge generate`, not just at install. Use for
# generated wrappers that have to track proto/config changes.
generate:
  - template: auth_gen_override.go.tmpl
    output:   pkg/middleware/auth/jwtauth/auth_gen.go
    description: "JWKS-based authentication interceptor for the jwt-auth pack"

# DB migrations — IDs are allocated at install time so they don't collide
# with the scaffold's 00001_init or with other packs' migrations.
migrations:
  - name: api_keys
    up:   api_key_migration.sql.tmpl
    down: api_key_migration_down.sql.tmpl
    description: "API keys table"
```

Field-by-field:

- **`name`** — must match the directory name. Hyphenated.
- **`version`** — semver, bumped when the manifest or templates change. `forge pack list` shows it.
- **`subpath`** — relative to `pkg/`. Pack code lives in this nested per-pack subpackage. Auth providers nest under `middleware/auth/<provider>/`; clients under `clients/<service>/`; audit at `middleware/audit/<name>/`. **Always nest** so multiple packs of the same family can coexist.
- **`config.section`** / **`config.defaults`** — block of YAML merged into the consumer's `forge.yaml`. Users override values after install.
- **`files`** — emitted on install. Each entry has `template`, `output`, and `overwrite`.
- **`dependencies`** — Go module paths. Optional `@version` pin. Run through `go get`.
- **`generate`** — emitted on every `forge generate`, not just install. Use for wrappers that depend on proto/config the consumer can change.
- **`migrations`** — SQL templates rendered into the consumer's migrations directory at install time, with sequential IDs allocated so they don't collide.

## Overwrite policies

| Policy | Behavior | Use for |
|--------|----------|---------|
| `always` | Re-rendered on every `forge generate`. Hand-edits are clobbered. | Generated wrappers, validators driven by config — anything that has to track proto / config changes. |
| `once` | Written at install. Never touched again. | Files the user is expected to customize — store implementations, dev-only helpers, config-driven defaults the user will edit. |

Document the policy in the file header so users don't lose work:

```go
// Code generated by `forge pack install jwt-auth`. DO NOT EDIT.
// To customize: see pkg/middleware/auth/jwtauth/dev_auth.go (overwrite: once).
```

## Template variables

Templates are rendered via Go's `text/template`. Available variables (set by the install lifecycle):

| Variable | What it is |
|----------|-----------|
| `{{ .Module }}` | The consumer project's Go module path (e.g. `github.com/example/proj`). Use for import paths. |
| `{{ .ProjectName }}` | Display name from `forge.yaml`. |
| `{{ .ProjectKind }}` | `service` / `cli` / `library`. |
| `{{ .Pack.Subpath }}` | The pack's `subpath` field, for self-references. |
| `{{ .Config.<key> }}` | Anything from the pack's `config.defaults` block. |

Common usage:

```go
// templates/jwt_validator.go.tmpl
package jwtauth

import (
    "context"
    "{{ .Module }}/pkg/middleware/auth"
    "github.com/golang-jwt/jwt/v5"
)
```

## Install lifecycle

`forge pack install <name>` does, in order:

1. Resolve pack from `internal/packs/<name>/pack.yaml`.
2. Refuse if pack is already installed (must `forge pack remove` first).
3. Render every `files` entry into the consumer tree (respecting `overwrite`).
4. Render every `migrations` entry into the consumer's migration directory with allocated sequential IDs.
5. Merge `config.defaults` into `forge.yaml` under `config.section`.
6. Append the pack name to `forge.yaml`'s `packs:` list.
7. `go get` each dep, then `go mod tidy`.
8. Run `forge generate` to fire the `generate:` entries plus any other codegen the new pack participates in.

Uninstall is the inverse: remove pack files (the manifest tracks them), revert the `forge.yaml` `packs:` entry, and warn (don't auto-run) about migrations and dependencies — you don't want to drop a column that's holding production data.

## Naming conventions

- **Pack name**: hyphenated, lowercase (`jwt-auth`, `api-key`).
- **`subpath`**: nest under the family directory (`middleware/auth/<provider>`, `clients/<service>`, `middleware/audit/<name>`). Never flat.
- **Per-pack subpackage** for the rendered Go code (`package jwtauth`, `package apikeyauth`). The rendered files all share one package.
- **Template filenames**: `<purpose>.go.tmpl`, `<purpose>.sql.tmpl`. Use a suffix-`.tmpl` extension so editors don't try to compile them.
- **Generated symbols**: prefix with the pack's package name where needed to avoid colliding with scaffold-emitted defaults (e.g. `apikeyauth.Validator` not just `Validator`).

## Avoiding scaffold collisions

The scaffold emits some defaults the user expects to find — `pkg/middleware/audit.go`, generic `Authenticator` symbols, etc. If your pack contributes to the same area, **do not** overwrite the scaffold's symbol. Use a per-pack name (`AuditInterceptorWithStore` instead of `AuditInterceptor`) and document the override surface in the pack README. Composing happens in the consumer's `pkg/app/setup.go`.

## Testing your pack

A pack's first test is `forge pack install <name>` against a fresh `forge new` project followed by `go build ./...` and `forge lint`:

```bash
mkdir -p /tmp/packtest && cd /tmp/packtest
forge new probe --mod example.com/probe --kind service --service api
cd probe
forge pack install <your-pack>
forge generate
go build ./...
forge lint
```

If any step fails, the pack is broken. Don't ship a pack you haven't installed end-to-end. Add a CI job that does the above for each pack on every commit.

For richer coverage, write a Go integration test that drives the rendered code (with a real DB if the pack has one) — see `testing/integration`.

## Common pack bugs (the migration session caught these)

- **Hyphenated proto packages.** A pack proto template that did `package {{ .ProjectName }}.db.v1` produced `package my-project.db.v1` — invalid. Use a layout-keyed package name (`db.v1`), not a project-keyed one.
- **Outdated annotation surface.** `forge.options.v1.*` is gone; the current annotation namespace is `forge.v1.*`. Update before publishing.
- **Symbol collisions with scaffold defaults.** Audit-log shipped `audit_gen.go` colliding with scaffold's `pkg/middleware/audit.go`. Nest under a per-pack subpath and use a per-pack package name.
- **Stale dep API.** Packs that pin a transitive dep can break when the dep's API changes (`keyfunc/v3.Keyfunc.Cancel()` was removed mid-line). Track upstream changes; bump the pack's `version` and the pin together.

## Rules

- One pack = one integration. Don't bundle two unrelated subsystems.
- Always nest under a per-pack subpath. Never write to flat `pkg/middleware/`.
- Pick `overwrite: always` vs `once` deliberately. Document it in the file header.
- `version` bumps when the manifest or templates change.
- Test by installing into a fresh `forge new` project end-to-end. Build + lint must pass.
- Avoid scaffold-symbol collisions. Pack symbols use the per-pack package name.
- Update the README with config knobs, wiring example, and customization points.

## Frontend pack conventions

Frontend packs (`kind: frontend`) follow a layered model. Pull the right tool from the right layer instead of reinventing primitives:

| Layer | What it is | Where it lives | Examples |
|-------|-----------|----------------|----------|
| **1. Base library** | Generic, framework-agnostic UI primitives. Installed unconditionally at scaffold time. | `forge/components/components/ui/` (master) → rendered into `frontends/<name>/src/components/ui/` | **Primitives:** `Button`, `Input`, `Label`, `Form`, `Card`, `Avatar`, `Tabs`, `Table`, `Select`, `Chip`. **Higher-level:** `SearchInput`, `Pagination`, `AlertBanner`, `Modal`, `SkeletonLoader`, `Badge`, `ToastNotification`, `KeyValueList`, `PageHeader`, `SidebarLayout`, `LoginForm`. |
| **2. Forge-aware primitives** | Hook-aware components that depend on forge-generated artifacts (e.g. the Connect transport, generated React Query hooks). Always shipped with the scaffold. | `internal/templates/frontend/nextjs/src/components/` | `Nav` (renders entries from `forge.yaml` pages), generated CRUD page templates |
| **3. Domain packs** | Opt-in installs that pair with generated hooks for a specific domain (data tables, auth flows, audit log viewers, billing portals). | `internal/packs/<name>/` (`kind: frontend`) | `data-table`, `auth-ui` |

### Rule (enforced): pack templates MUST import from the base library, not third-party UI, and MUST NOT inline button/input/table/etc. markup

This rule is now enforced — both shipped frontend packs (`data-table`, `auth-ui`) compose the base library directly and the `frontendpacklint` analyzer reports zero warnings against the convention. New packs that don't follow it will surface as warnings under `forge lint --frontend-packs`.

A frontend pack template that imports `@radix-ui/*`, `@headlessui/*`, `@tanstack/react-table` (for JSX), `@mui/*`, `recharts`, etc. **directly** is a convention violation. The right move is one of:

1. **Reuse the base library.** Every forge frontend ships the primitives in the table above under `src/components/ui/`. Use `@/components/ui/button`, `@/components/ui/input`, `@/components/ui/table`, `@/components/ui/form`, `@/components/ui/select`, `@/components/ui/chip`, etc. Don't hand-roll a `<button class="rounded-md bg-blue-600 …">` in a pack template — that's the cue to import `Button` instead.
2. **Add the primitive to the base library.** If you need a primitive the library doesn't have yet, add it under `forge/components/components/ui/<name>.tsx`, register it in `library.go`, and add it to `coreComponents` in `internal/generator/frontend_gen.go`. Then refactor your pack to import it. Every pack benefits — don't reimplement.
3. **Allowlist a headless engine.** Some libraries are genuinely needed (TanStack Table for headless sort/filter state, `recharts` for chart rendering, `@react-google-maps/api` for maps). Declare them in `pack.yaml`:

   ```yaml
   allowed_third_party:
     - "@tanstack/react-table"  # Headless engine; we wrap it with base library components.
   ```

   The trailing-slash form (`@radix-ui/`) allows an entire scope. Utility-only deps (form validators like `react-hook-form` + `zod`, state libs like `zustand`) are not gated by this rule — only the third-party UI prefixes the analyzer recognizes (`@radix-ui/`, `@chakra-ui/`, `@mui/`, `@tanstack/react-table`, `recharts`, etc.) are.

The `frontendpacklint` analyzer (run via `forge lint` or `forge lint --frontend-packs`) flags violations as warnings. It is intentionally non-blocking — you can ship without resolving every warning — but the long-term direction is to wrap, not duplicate. The shipped packs (`data-table`, `auth-ui`) currently produce zero warnings; new packs should aim for the same.

### When to write a frontend pack vs. extend the base library

| Question | Pack | Library |
|----------|------|---------|
| Domain-specific (auth flow, billing portal, CRUD shell)? | ✅ pack | — |
| Generic primitive (Button, Input, Modal, Badge)? | — | ✅ library |
| Needs config knobs in `forge.yaml`? | ✅ pack | — |
| Needs npm dependencies? | usually pack | only if every project should pay for them |
| Hook-aware (consumes generated React Query hooks)? | ✅ pack | — |
| Reusable across many domains as a building block? | — | ✅ library |

Concrete examples:

- `Button`, `SearchInput`, `Modal` → **library** (generic primitives, every app needs them).
- `auth-ui` (login/signup forms wired to Clerk/Auth0) → **pack** (domain-specific, opt-in).
- `data-table` → **borderline pack**: kept as a pack because it pairs with forge's generated `useEntities` hooks and ships TanStack Table as a headless dep. Renders via base library primitives (`AlertBanner`, `SearchInput`, `Pagination`, `SkeletonLoader`).
- A `<Select>` primitive → **library** (generic, currently a backlog gap — see `FORGE_BACKLOG.md`).

### Frontend pack manifest

Frontend packs declare `kind: frontend`, `npm_dependencies:`, and use `{{.FrontendPath}}` / `{{.FrontendName}}` interpolation in `output:` so a single manifest installs into every frontend declared in `forge.yaml`:

```yaml
name: data-table
kind: frontend
version: 1.1.0
subpath: src/components/data-table

config:
  section: data_table
  defaults:
    default_page_size: 25
    page_size_options: [10, 25, 50, 100]

# npm packages installed via `npm install --save` in each frontend
# directory at install time.
npm_dependencies:
  - "@tanstack/react-table@^8.20.0"

# Third-party UI imports the pack templates may reference directly.
# Anything outside this list raises a frontendpacklint warning.
allowed_third_party:
  - "@tanstack/react-table"

files:
  - template: DataTable.tsx.tmpl
    output: "{{.FrontendPath}}/src/components/data-table/DataTable.tsx"
    overwrite: once
```

The installer iterates `cfg.Frontends`, skips `go get`/`go mod tidy`, and runs `npm install --save` per frontend instead. Migrations are rejected for frontend packs — frontend packs don't own DB schema.

## When this skill is not enough

- **Consuming an existing pack** (install, configure, wire) — see `packs`.
- **Auth providers specifically** (JWT, Clerk, Firebase, API key) — read the existing auth packs in `internal/packs/` plus the `auth` skill.
- **DB migration conventions and ORM concerns** — see `db`.
- **Code rendered by `forge generate` for project-level codegen** (vs. pack-level) — see `architecture`.
