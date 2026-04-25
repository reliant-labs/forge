---
name: reproduce
description: Reproduce bugs — diagnostic logging, runtime evidence collection, and e2e reproduction.
---

# Reproduce Bugs

## When to Use

The bug requires runtime evidence — you can't determine the cause from code reading alone.

## System Discovery

1. Check `forge run` output for service ports and startup logs
2. Find API routes in `proto/` definitions
3. Identify the service and handler involved in the bug

## Diagnostic Logging

Mark all debug logging clearly so it can be cleaned up:

```go
// DEBUG-REPRO: Remove after fixing [description]
```

What to log, in order of placement:
1. **Inputs** at function entry
2. **State** before and after mutations
3. **Branches** — which decision path was taken
4. **Errors** with full context (wrapped, not bare)
5. **Exit** values and final state

## Triggering the Bug

- curl API endpoints directly
- Exercise via Connect client
- Query DB state before and after the operation
- Use `forge debug start <svc>` to attach a debugger if logging isn't enough

## E2E Reproduction

Write a minimal test under `e2e/` that triggers the bug against the live stack:

```
forge test e2e
```

The test should assert on the **wrong** behavior first (red), then flip to expected after fix.

## Evidence Format

Collect and present:
- Environment (OS, Go version, service versions)
- Exact trigger command or request
- Observed vs expected behavior
- Relevant log lines
- State changes (DB rows, cache entries)

## User Handoff

If you can't reproduce programmatically, provide exact manual steps and specify what logs to collect.

## Cleanup

Always grep for `DEBUG-REPRO` markers and remove them after the bug is fixed.
