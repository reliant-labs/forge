---
name: investigate
description: Investigate bugs — hypothesis formation, code tracing, and common Forge failure patterns.
---

# Investigate Bugs

## When to Use

You have a bug report with no obvious code path — you need to form and rank hypotheses before diving in.

## Investigation Workflow

1. **Understand the bug**: expected vs actual, frequency, environment, recent changes
2. **Trace code paths**: grep for the affected feature, follow the call chain from API endpoint → handler → service → DB
3. **Check recent changes**: `git log` on affected files, look for related commits
4. **Form 2–3 ranked hypotheses** with confidence percentage
5. **Verify each**: read code carefully, check edge cases, look for the patterns below

## Frontend bugs that depend on backend state

If the bug only reproduces "when the user has X done" (signed in, GitHub connected, daemon running, …), don't drive through the real flow manually. Reach for a **scenario**: a typed RPC handler overlay you select via `?scenario=name` in the URL. Chrome DevTools MCP can teleport into any scenario in one navigate; no human in the loop.

- If a scenario already exists for the state you need, navigate to it.
- If not, run `forge add scenario <name>`, edit the generated handler to return the right response shape, and navigate.

See the `frontend/scenarios` sub-skill for the API and rules. The most important one: **mock the RPC response, not the React Query cache** — state-seeding skips the parser layer that's the most common source of "the wire said yes but the UI shows no" bugs.

## Common Forge Bug Patterns

**Stale generated code**
Forgot `forge generate` after a proto change. Run it and retest:
```
forge generate
forge test
```

**Nil proto fields**
Optional proto fields accessed without nil checks. Look for bare field access on messages that could be empty.

**Wrong Connect error codes**
Returning `Internal` when it should be `InvalidArgument`, `NotFound`, or `PermissionDenied`. Check handler return paths.

**Race conditions in handlers**
Shared state modified without synchronization. Look for package-level vars, map writes, or shared slices accessed from handlers.

**Migration drift**
Code expects a column or table that hasn't been migrated yet. Compare struct fields against the latest migration.

**Missing error checks on Connect calls**
Frontend calling Connect endpoints without handling error responses. Check for unchecked `.error` on client responses.

## Output Format

Present findings as:
- **Hypothesis 1** (confidence %): description, file:line evidence
- **Hypothesis 2** (confidence %): description, file:line evidence
- **Suggested fix approach** for handoff to implementer

Do not fix the bug in investigation mode — hand off with clear evidence.
