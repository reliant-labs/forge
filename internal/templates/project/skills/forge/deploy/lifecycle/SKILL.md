---
name: deploy/lifecycle
description: Stopping, starting and deleting a HOSTED environment from the CLI — `forge env stop|start|delete`, --target and --wait, billing refusals, data retention, and why `forge env down` is not the same thing; plus the domain commands and scope errors.
---

# Hosted env lifecycle

Everything the Reliant UI does to a hosted env is available with a plain
`forge login`:

```
forge env stop prod [--target api] [--wait]    # suspend; never refused
forge env start prod [--target api] [--wait]   # resume; refused with the billing reason if not entitled
forge env delete preview [--yes]               # tear the env down
```

- `--target` takes a workload name (resolved to its deployment id); without it
  the whole environment changes. After the call forge prints each deployment's
  DECLARED state; the platform converges asynchronously.
- `--wait` polls until the platform OBSERVES the new state (`--timeout`,
  default 5m) and exits non-zero on timeout.
- A refused `start` prints the server's reason and the fix; nothing is started.
- `delete` prompts you to type the env name. With no TTY or `CI=1` it fails
  fast unless you pass `--yes`. It removes the env, its deployments, Flux
  objects, custom resources and namespaces. A ManagedDatabase's data is
  RETAINED but orphaned: a re-deploy creates a NEW env and does not reattach it.
- All three refuse an env that is not hosted. `forge env down` only stops local
  host processes and never touches a control plane.

## Domains

`forge domain add|ls|show|verify|bind|unbind|rm --env <env>` — see
`deploy/domains`.

## Missing-scope rejections

A 403 naming a scope (e.g. `domain:read`): re-run `forge login` to pick it up.
If the new token still lacks it, your role has no grant — ask an org admin, who
can mint `forge cloud token create --env <env> --name <n> --scopes <scope>`;
export it as the env's declared token variable.
