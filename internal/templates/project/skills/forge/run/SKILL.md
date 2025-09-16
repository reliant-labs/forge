---
name: forge/run
description: Run the full dev stack with hot reload — infra, services, and frontends.
when_to_use:
  - You're starting a development session and need the stack up locally
  - You want hot reload on Go services and Next.js frontends simultaneously
  - You need to exercise the system manually in a browser or with curl
  - You're about to run e2e tests (they require the stack to be running)
---

# forge/run

`forge run` brings up the entire local development environment: docker-compose infrastructure (postgres, etc.), the Go binary under Air (hot reload), and every Next.js frontend via `npm run dev`. Logs are color-coded per service.

## Core commands

```
forge run                           # full dev stack: infra + services + frontends
forge run --debug                   # same, but Delve attaches on :2345
forge run --no-infra                # skip docker-compose (assume infra already up)
forge run --service <name>          # only run one service (still brings up infra)
```

Press **Ctrl+C once** to shut down all child processes gracefully. Do not `kill -9` — it leaves orphaned docker containers.

**Note on `--env`**: the flag exists but currently only prints the environment name at startup. It does not load a different config. For environment-specific runs, use `forge deploy <env>` against k3d instead.

## Workflow

1. First time in a fresh clone:
   ```
   go mod download
   forge generate      # populate gen/ — it's not committed
   forge run
   ```
2. Normal dev loop: leave `forge run` in one terminal. Edit code in another. Air rebuilds the Go binary on save; Next.js hot-reloads frontends.
3. Debugging a crash on startup: add `--service <name>` to isolate one service's logs.
4. Running alongside e2e tests: start `forge run` in one terminal, then run `forge test e2e` in another. The e2e suite expects the stack already up.

## Rules

- One `forge run` at a time per project — ports conflict otherwise.
- Don't restart on every code change. Air handles hot reload. If hot reload breaks, investigate `.air.toml` rather than restarting forge.
- `--no-infra` is only for the case where you're running postgres (or whatever) outside docker-compose deliberately. If in doubt, omit it.
- If a service crash-loops, stop `forge run` and attach a debugger with `forge debug start <svc>` to get a clean crash without log interleaving.

## When this skill is not enough

- You need to interactively debug a specific service → `forge run --debug` or `forge/debug`.
- You need to run against a real cluster → `forge/deploy` to dev / staging / prod.
- You need to run a one-off script → use the project's `Taskfile.yml`, not `forge run`.
