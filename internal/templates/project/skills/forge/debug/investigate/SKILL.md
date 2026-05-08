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
