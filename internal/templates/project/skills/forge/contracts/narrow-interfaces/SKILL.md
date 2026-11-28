---
name: contracts/narrow-interfaces
description: Narrowing wide repository facades at port time — per-aggregate contracts, consumer seams and transitional query-generator exclusions.
---

# Prefer narrow per-aggregate interfaces over wide facades

The contract pattern works best when `Service` is **focused** — one cohesive responsibility, methods that belong together by domain. A 100+ method `Repository` interface (one struct exposing every DB operation in the system) defeats the pattern:

- Every consumer depends on every method, even the ones it doesn't use.
- Mocks become unusable in tests — you can't easily fake just the 3 methods a handler calls.
- `interfacebloat` will fire, correctly.

The remedy depends on your shape:

| Source shape | Port-time fix |
|---|---|
| Hand-written wide `Repository` interface + a concrete `*PostgresRepository` that satisfies it | Drop the wide `Repository`. Define narrow per-aggregate interfaces (`UserRepository`, `OrgRepository`, `BillingRepository`). The same concrete `*PostgresRepository` satisfies all of them structurally — Go does that for free. Each handler depends on the 10-method interface it actually needs. |
| Multi-aggregate package with wide internal surface | Split into per-aggregate packages: `internal/db/user/`, `internal/db/org/`, `internal/db/billing/`. Each gets its own narrow `contract.go`. Larger refactor; cleanest long-term. |
| A query generator's `Queries` struct carried over from the source (e.g. sqlc: one method per query, all in one struct) | Transitional only, since forge does not run sqlc. Port the queries onto the generated `<Entity>Store` and `internal/db/<entity>_repo_ext.go` (see `db/generated-orm`). Until each aggregate moves, path-exclude the generated dir with a note naming that port. It is the one case where `interfacebloat` is a false positive: the generator's output shape is not yours to split. |
| Wide interface but only one or two callers | Either narrow it inline (delete the methods nobody calls) or split per-caller. Don't keep a 50-method interface that two functions use. |

Design for the *callers*, not the implementation. Have the split-or-narrow conversation at port time, not after lint failures pile up.

See `contracts` for declaring the resulting contracts and generating mocks.
