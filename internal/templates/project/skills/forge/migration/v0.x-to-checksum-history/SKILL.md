---
name: v0.x-to-checksum-history
description: Migrate `.forge/checksums.json` from the legacy flat shape (path -> hex string) to the structured shape (path -> {hash, history[]}). The new shape lets `forge upgrade` distinguish stale codegen from genuine user edits, eliminating false-positive "user-modified (skipped)" reports on real template upgrades.
---

# Prior-render checksum history

Use this skill when `forge upgrade` previously flagged Tier-2 files as
user-modified even though you didn't touch them — the diff just showed a
template improvement (renamed import, added comment, refactored helper).
That's the failure mode this change eliminates. Most users will not have
to do anything: the migration is transparent on the next `forge upgrade`
or `forge generate`.

## 1. What changed

Forge tracks each generated file's sha256 checksum in
`.forge/checksums.json` so `forge upgrade` can distinguish "forge
generated this; the template moved on" from "the user edited this, leave
it alone." Before this change, each file had a single recorded checksum:

```json
{
  "forge_version": "1.5.0",
  "files": {
    "pkg/middleware/requestid.go": "ab12...c34d"
  }
}
```

When a template was updated between forge versions, the on-disk file
(still the prior render) hashed to a value that matched neither the new
template nor the recorded checksum (because no checksum existed for the
prior render — the entry only ever held the current). `forge upgrade`
took the conservative path and reported the file as user-modified, even
though forge itself had rendered every byte of it. The user then had to
diff the file by hand and decide whether to `--force` overwrite or
hand-merge.

The new shape stores a bounded history of every checksum forge has ever
rendered for a path:

```json
{
  "forge_version": "1.6.0",
  "files": {
    "pkg/middleware/requestid.go": {
      "hash": "ef56...7890",
      "history": ["ab12...c34d", "ef56...7890"]
    }
  }
}
```

`Hash` is the most recent render (mirrors the tail of `history`).
`history` is the deduplicated, bounded list of *every* checksum forge
has rendered for this file. On upgrade, if the on-disk content matches
the current `Hash` *or* any entry in `history`, forge knows it generated
that content and auto-updates cleanly. Only content that matches
nothing in either field is treated as user-modified.

The bound is 20 entries — small enough to keep `.forge/checksums.json`
readable when a human peeks at it, large enough that ordinary
template-churn never falls off the back of the window.

## 2. Detection

```bash
# Old shape: file values are bare hex strings.
jq '.files | to_entries | .[0].value | type' .forge/checksums.json
# Old shape prints: "string"
# New shape prints: "object"
```

## 3. Migration (deterministic part)

There's nothing to do — the migration is transparent.

`forge generate` and `forge upgrade` both call `LoadChecksums`, which
accepts both shapes. A legacy hex string is promoted to a structured
entry with `history` seeded by the same hash. The next time forge
records a checksum for that path, it appends the new hash to history
and writes the structured shape back. After one `forge generate` cycle,
`.forge/checksums.json` is fully migrated.

```bash
# Force a round-trip — re-renders all generated files and rewrites
# checksums.json in the structured shape.
forge generate
```

## 4. Migration (manual part)

Nothing — this change has no user-facing API. The only observable
difference is that `forge upgrade --dry-run` reports cleaner results on
real template upgrades:

- **Before:** `pkg/middleware/requestid.go: user-modified (skipped)` —
  even though you never touched it.
- **After:** `pkg/middleware/requestid.go: would update (clean)` — forge
  recognises the on-disk content as a known prior render.

If you previously had a `--force` step in a CI pipeline to work around
the false-positive flagging, you can drop it. `forge upgrade` (without
`--force`) will now correctly auto-update stale-codegen files and skip
genuinely user-edited ones.

### Edge cases

- **Genuine user edits remain protected.** If the on-disk content
  matches *neither* the current Hash *nor* any history entry, forge
  still reports it as user-modified. The history check only relaxes
  the false-positive case.
- **Files that pre-date checksum tracking** (no entry in `files`) are
  left alone the same way they were before — forge doesn't "own" them.
- **History bound = 20.** Long-running projects that re-render a file
  more than 20 times fall off the back of the window. In practice
  template churn doesn't approach that — most files re-render <5 times
  across a project's lifetime.
- **`--force` still wins.** `forge upgrade --force` overwrites
  user-modified files unconditionally, same as before.

## 5. Verification

```bash
# Inspect the new shape.
cat .forge/checksums.json | head -20
# Each entry should be {"hash": "...", "history": ["...", ...]}.

# `forge upgrade --dry-run` on an unmodified project should report no
# user-modified files.
forge upgrade --dry-run | grep -E "user-modified" || echo "no false positives"

# A genuine user edit is still detected:
echo "// my edit" >> pkg/middleware/requestid.go
forge upgrade --dry-run | grep -E "requestid.*user-modified"
# Expect: pkg/middleware/requestid.go: user-modified (skipped)

# Revert the edit and re-run — should be clean.
git checkout pkg/middleware/requestid.go
forge upgrade --dry-run
```

## 6. Rollback

If the new shape causes problems (we don't expect any — it's strictly
additive), revert by:

```bash
# Pin to an older forge build that predates this change.
forge upgrade --to <prior-version>
```

The on-disk JSON is forward-compatible: an older forge reading the new
structured shape would fail to parse, so plan to also restore
`.forge/checksums.json` from git. The legacy code path is preserved as
the second branch in `internal/checksums/unmarshalChecksums` and the
write path always emits the structured shape, so a true downgrade
needs both a forge revert and a checksum-file revert.

In practice the right rollback is "let forge regenerate everything":

```bash
rm .forge/checksums.json
forge generate
```

This re-renders every tracked file and rewrites the checksum file in
whichever shape the active forge build produces.

## See also

- `migration/upgrade` — the top-level upgrade skill explaining how
  `forge upgrade` chooses between auto-update and skip.
- `internal/checksums/checksums.go` — package docs covering the JSON
  format, the `MatchesAnyKnownRender` helper, and the `historyLimit`
  bound.
- `FORGE_BACKLOG.md` "Drift checksum gap" — the original backlog item
  describing the failure mode this fix eliminates.
