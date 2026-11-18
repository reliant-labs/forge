---
name: v1.8.0
description: Upgrade a project to forge v1.8.0. Frontends move to zod 4 for GHSA-mrhx-c7xc-m3w5 (high; zod below 4.6.6, with no 3.x fix). A frontend scaffolded since v1.8.0 pins zod ^4.6.6, @hookform/resolvers ^7.0.7 and react-hook-form ^7.55.0, and its generated create/edit forms use the zod 4 API (z.enum over a TypeScript enum, z.email(), and a form typed by z.input and z.output). Nothing breaks on upgrade: forge reads each frontend's package.json and keeps writing pages for the zod it declares, but the advisory stays until you bump. Use when a frontend's package.json declares zod 3 or an @hookform/resolvers below 5.
version: v1.8.0
detection: forge-detect zod3-frontend
---

# Upgrading to v1.8.0

## 1. Frontends move to zod 4 (GHSA-mrhx-c7xc-m3w5)

**Use when** a frontend's `package.json` (`frontends/<name>/package.json`, or
the root `package.json` of a site) declares `zod` 3 or `@hookform/resolvers`
below 5, in `dependencies` or `devDependencies`. A range that admits one
major is that major (`^3.23.0`, `~3.22`, `3.x`, `v3.23.8`, `>=3.0.0 <4`,
`npm:zod@^3.22`); one that admits several (`>=3.0.0`, `*`) or is not a
range (`catalog:`) is the version installed — `node_modules`, then
`package-lock.json` or `pnpm-lock.yaml`. `forge generate` reads it the same
way.

### What changed

[GHSA-mrhx-c7xc-m3w5](https://github.com/advisories/GHSA-mrhx-c7xc-m3w5)
(CVE-2023-54404, high) covers every zod below 4.6.6. There is no 3.x release
with the fix. A frontend scaffolded since v1.8.0 declares:

```json
"@hookform/resolvers": "^7.0.7",
"react-hook-form": "^7.55.0",
"zod": "^4.6.6"
```

(an Expo frontend declares `zod` only), and the create and edit pages forge
generates for an entity use the zod 4 API:

| Was (zod 3) | Now (zod 4) |
|---|---|
| `z.coerce.number().pipe(z.nativeEnum(OrderStatus))` | `z.coerce.number().pipe(z.enum(OrderStatus))` |
| `z.string().min(1, "Required").email("Enter a valid email address")` | `z.string().min(1, "Required").pipe(z.email("Enter a valid email address"))` |
| `type FormValues = z.infer<typeof schema>;` | `type FormInput = z.input<typeof schema>;`<br>`type FormValues = z.output<typeof schema>;` |
| `useForm<FormValues>({ resolver: zodResolver(schema) })` | `useForm<FormInput, unknown, FormValues>({ resolver: zodResolver(schema) })` |

The form is typed twice because `zodResolver` from @hookform/resolvers 5 on
states both types: what the inputs hold (`z.input`, which is `unknown` for a
coerced number) and what a submit receives (`z.output`). A form typed by
`z.infer` alone fails `tsc` against it.

Upgrading forge changes none of your files. The pages are yours, and so is
`package.json`. `forge generate` reads the `zod` and `@hookform/resolvers`
majors each frontend declares and writes any page it adds later (a new
entity's list, create and edit pages) in that frontend's dialect, so a
frontend still on zod 3 keeps building. It stays exposed to the advisory
until you bump it.

### What to do

For each frontend that declares zod 3:

1. **Bump the three packages** in its `package.json` to the versions above,
   then install with the frontend's own package manager (`pnpm install` with
   a `pnpm-lock.yaml`, else `npm install`). Check what resolved:
   `pnpm why zod` / `npm ls zod` should list nothing below 4.6.6. A library
   that pins zod 3 as a dependency of its own needs its own upgrade, or an
   override.
2. **Port the generated pages.** In each create and edit page
   (`src/app/<slug>/new/page.tsx` and `src/app/<slug>/edit/page.tsx` in
   Next.js, `src/pages/<slug>/Create.tsx` and `Edit.tsx` in Vite), make the
   four edits in the table. In a page you have not restructured the first,
   third and fourth are mechanical:

   ```bash
   cd frontends/<name>
   files=$(grep -rlE 'zodResolver' src --include='*.tsx')
   sed -i \
     -e 's/z\.nativeEnum(/z.enum(/g' \
     -e 's/^type FormValues = z\.infer<typeof schema>;$/type FormInput = z.input<typeof schema>;\ntype FormValues = z.output<typeof schema>;/' \
     -e 's/useForm<FormValues>(/useForm<FormInput, unknown, FormValues>(/' \
     $files
   ```

   (GNU sed; on macOS, `brew install gnu-sed` and run `gsed`). An `.email("…")` still works in zod 4 (it is
   deprecated, not removed). To port it, pipe the string into `z.email` and
   move any rule that followed it inside the pipe, so the messages keep their
   order: `.max(64, "…").email("…").regex(re, "…")` becomes
   `.max(64, "…").pipe(z.email("…").regex(re, "…"))`.
3. **Port your own zod code.** zod 4's changes that most often break a build
   (the full list: https://zod.dev/v4/changelog):
   - Error customization takes `error`: `{ message: "…" }` still works but is
     deprecated, and `errorMap`, `invalid_type_error` and `required_error`
     are gone; use `{ error: (issue) => "…" }`.
   - `z.string().email()`, `.url()`, `.uuid()` and the other formats are
     top-level schemas: `z.email()`, `z.url()`, `z.uuid()`.
   - `z.nativeEnum(E)` is `z.enum(E)`.
   - `.default(v)` now takes and returns the OUTPUT type and skips parsing
     when the input is `undefined`; `.prefault(v)` is the zod 3 behavior.
   - `z.record(valueSchema)` needs its key schema: `z.record(z.string(), valueSchema)`.
   - `.strict()` and `.passthrough()` are `z.strictObject({…})` and
     `z.looseObject({…})`.
   - `ZodError.errors` is `.issues`; `error.flatten()` / `.format()` are
     `z.flattenError(error)` / `z.treeifyError(error)`.
   - `z.ZodTypeAny` is `z.ZodType`.
4. **Verify:** `npx tsc --noEmit` and `npm run build` (pnpm: `pnpm exec tsc
   --noEmit`, `pnpm build`) in the frontend, then submit a create and an edit
   form once: an empty required field, a bad email address and an enum
   select should all still show their messages.

From then on, pages `forge generate` adds to that frontend use the zod 4 API.
