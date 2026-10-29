---
name: v1.0.7
description: Upgrade a project to forge v1.0.7. (1) pkg/dbquery.Options and pkg/dbquery.Event are no longer comparable — both gained an Args slice for bound query parameters. Use when Go code compares or map-keys a dbquery.Options or dbquery.Event value.
version: v1.0.7
detection: grep -rqsE --include='*.go' --exclude-dir=gen --exclude-dir=vendor --exclude-dir=node_modules 'dbquery\.(Options|Event)\b' . && exit 0; exit 1
---

# Upgrading to v1.0.7

Each section below is independent: apply the ones whose "Use when" matches.

## 1. `dbquery.Options` and `dbquery.Event` are no longer comparable

**Use when** Go code compares a `dbquery.Options` or `dbquery.Event` with `==`/`!=`, or uses one as a map key.

**What changed.** `Options.Args` carries the values bound to `$1..$n` of the statement, and `Event.Args` records them in the audit event. A slice field makes the struct non-comparable, so the compiler now rejects those comparisons. Building and passing an `Options{}` or reading an `Event` is unaffected.

**What to do.** Compare the fields you mean (`a.MaxRows == b.MaxRows`), or use `reflect.DeepEqual` / `cmp.Equal` in tests. To key a map by options, key by the fields that matter. A comparison of the whole value is rare enough that no tool can pick the intent, so there is no codemod.
