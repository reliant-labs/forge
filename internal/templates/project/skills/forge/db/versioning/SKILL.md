---
name: db/versioning
description: Migration version numbers — why they are UTC timestamps, the two lint rules that enforce it, and what to do when a migration is refused because the schema already passed its version.
---

# Migration versioning

`forge db migration new <name>` names the file `<YYYYMMDDHHMMSS>_<name>.up.sql`, using UTC. **Never hand-type a version number.**

## Why timestamps and not max+1

A sequential allocator picks `max+1` by reading the migrations directory. That is collision-free on one checkout and collision-prone across branches: two branches cut from the same commit both read the same highest number and both pick the same next one, and neither sees the other until merge.

Measured on one repository: **ten version numbers were each claimed by two different migrations**, and one number was claimed on eight separate branches. Every one had to be renumbered by hand.

The damage is worse than a merge conflict, because a version is the schema's only identity. golang-migrate records one integer — "the schema is at 91" cannot say *which* 91. So whichever file reached a shared database first left the other permanently unapplied, while every later `up` reported the schema current. The first symptom is a query for a column that does not exist.

A UTC timestamp removes the shared counter: two branches allocating at different instants cannot pick the same value without coordinating. Versions become sparse, which nothing depends on — the migrator orders by version and never assumes `version+1` exists.

## Existing sequential migrations are never renamed

A 14-digit timestamp exceeds any 5-digit number by nine orders of magnitude, so **every new migration sorts after every old one** with no renaming, no backfill and no flag day. A project mid-adoption holds both spellings and orders correctly. Adopting this costs nothing.

## The two lint rules

Both are errors in `forge lint`:

| Rule | Fires when |
|---|---|
| `duplicate-migration-version` | two files claim one version. Reported on *every* claimant, so a reviewer reading one file learns it is contested and by which file. Needs no git. |
| `non-timestamp-migration-version` | a **new** migration is not a 14-digit UTC timestamp. |

"New" means *added since the merge-base with your default branch* — exactly the set a pull request can still fix — so pre-existing sequential migrations are never flagged and nobody is asked to renumber history. This rule is what catches an out-of-order version at review time, before a deploy can refuse.

Without git (a shallow clone, a tarball, no default branch) newness is unknowable, and the rule narrows to what is wrong regardless: a 14-or-more-digit version that is not a real calendar instant. `99999999999999` cannot be a sequence number and is not a date, so it is hand-typed and would sort ahead of every genuine timestamp forever.

The timestamp check is a **calendar** check, not a digit count, for that same reason.

## When a migration is refused because the schema passed it

golang-migrate applies only migrations **above** the recorded version, so one numbered below it can never run. With timestamps that happens exactly one way: branches A and B are cut, A allocates the earlier timestamp, and **B merges and deploys first**. Main now holds A's migration at a version the database has already passed.

The migrator refuses with `*migratekit.MissingMigrationError` and applies nothing. **The fix is to re-version the file** — it has not run anywhere, so renaming it to a fresh timestamp is safe, and the error text says so.

```bash
forge db migration rebase db/migrations/20260101120000_add_users.up.sql
forge db migration rebase --all-pending   # every migration added on this branch
```

Rebase keeps the name stem, allocates a version above both the directory's max and the default branch's (so a squashed-away migration still on main is accounted for), `git mv`s tracked files, and prints `old → new`.

It **refuses** a migration that is already on the default branch, with no override. That one has been recorded as applied under its current filename by every database that ran it, so renaming it would leave those databases with a recorded version whose file does not exist — worse than the problem, and unrecoverable without hand-editing `schema_migrations`. A migration that has merged keeps its version; repair it with a new forward migration.

Forge deliberately does not apply it out of order. That would make the schema depend on merge order, so A-then-B and B-then-A would produce different databases from the same commit with nothing reporting which one you got — a `DROP COLUMN` landing after the migration that reads the column is a different schema from the reverse. For the least reversible thing that ships, a loud refusal beats a quiet guess.

See `db/deploy-migrations` for the full record model (`schema_migrations_applied`, the mismatch check, and the one-time bootstrap that lets an existing database adopt the check without refusing).
