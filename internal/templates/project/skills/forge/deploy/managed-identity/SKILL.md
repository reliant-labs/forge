---
name: deploy/managed-identity
description: forge.ManagedIdentity, an env's users declared in KCL — roles, the applications that sign users in, the workloads that read tokens, share_with, SMTP, seed users — and how forge fills each consumer's typed config (jwt_issuer, oidc_client_id, ...) with no app code change, what it delivers per runtime, what is refused, and what is not built yet (hosted login, social).
---

# Managed identity

`Bundle.identity = forge.ManagedIdentity {...}` declares an env's users. The control plane converges an identity-provider instance to it; forge records the **declaration** in the release shape (`shape.identity`, names only) and fills every consumer's typed config from the instance's outputs. You write no `config.k` line and no app code for it, and copy nothing between files.

```python
# deploy/kcl/identity.k, imported by every env
IDENTITY = forge.ManagedIdentity {
    roles = [
        forge.IdentityRole {key = "admin", display_name = "Administrators"}
        forge.IdentityRole {key = "member"}
    ]
}

# deploy/kcl/prod/main.k
identity = idn.IDENTITY                                   # runs on the control plane (hosted)

# deploy/kcl/dev/main.k: the same declaration on the env's host-run Zitadel
identity = idn.IDENTITY | {runtime = forge.IdentityOnHost {infra = "idp"}}

# a preview env reusing its project's staging users
identity = forge.ManagedIdentity {share_with = "staging"}
```

## What you declare

| field | meaning |
|---|---|
| `runtime` | `forge.OnHosted {}` (default; needs `control_plane`) or `forge.IdentityOnHost {infra}` (a `HostInfra` with `engine = "zitadel"`) |
| `login.mode` | `native` (default): the app's own form plus the server-side login broker. `hosted` is **phase 2**: accepted and recorded, nothing converges it yet |
| `roles` | `key`, `display_name`, `group`; `members_from = "console"` (default) or `"declared"` with `members = [emails]`, a set converged from KCL and read-only in the console |
| `applications` | derived when empty: one `native_broker` named `<env>:api` for the workload `api`, else one per service workload. `redirect_uris`, `logout_uris`, `login_uri` are literal overrides |
| `consumers` | derived when empty: every service workload on a runtime that can reach the instance. `fields = forge.IdentityFields {...}` remaps an output to a non-standard config field (an app validating two issuers); `deliver_only = True` is the general "deliver, I will read the Secret myself" escape hatch: the control plane still writes `forge-identity-<name>` (+ `-jwks`) to the workload's cluster, but forge projects nothing into the workload (no env, no volumes) and does not refuse hand-set values. Use it for a workload that reads the Secret itself, or while adopting an identity whose workload still reads legacy config; flip it off to let forge fill the typed config |
| `machine_users` | `name`, `roles`, `key_secret` + `cluster`: where its key is delivered (hosted instance); a host-run one keeps the key in the env's secret store |
| `smtp` | `host`, `port`, `user`, `sender`, `password = forge.ManagedSecret {name = ...}`: a **name**, never the password. Omit it and mail goes through the platform relay |
| `seed_users` | only in an env that declares `lifecycle = "local"` or `"ephemeral"` |
| `domain` | your own issuer domain; needs `public = True` (or hosted login) |
| `public` | ask for a public route. **Private by default** |
| `project_name` | the identity project's name (default: `Bundle.project`); settable so an adopted instance keeps its legacy name |
| `share_with` | a bare env name **of the same project**. The owner must declare an identity and not itself be a sharer: no chains. A sharer declares only applications and consumers |
| `social` | **reserved**; refused at render |

## The issuer

`https://<slug>.reliantidentity.com`. The control plane assigns the slug **once**, when the instance is first provisioned, stores it, and never recomputes it, so renaming an env or project cannot move an issuer. The rule it assigns by is `release.IdentitySlug`:

```
<project>-<env>-<hash>      label = lower-case [a-z0-9], other runs folded to "-", at most 24 characters
hash  = first N hex digits of sha256(envID), N = 6 and grown (to 12) only to resolve a collision
```

`envID` is the control plane's immutable id for the env, never its name. The control plane's unique index, not the hash, guarantees uniqueness. A slug is at most 62 characters.

## What reaches a consumer

The consumer's typed config fields are filled; the env var is the upper-cased field name.

| output | field | hosted workload | cluster workload | host process (OnHost identity) |
|---|---|---|---|---|
| issuer | `jwt_issuer` | managed secret `RELIANT_IDENTITY_<NAME>_ISSUER` | key `ISSUER` of Secret `forge-identity-<name>` | secret store key `ISSUER` |
| audience | `jwt_audience` | `..._AUDIENCE` | `AUDIENCE` | `AUDIENCE` |
| scopes | `oidc_scopes` | `..._SCOPES` | `SCOPES` | `SCOPES` |
| idp_base | `idp_base` | `..._ISSUER` | `ISSUER` | `ISSUER` |
| client_id, redirect_uri, login_uri | `oidc_client_id`, `oidc_redirect_uri`, `idp_login_uri` | `..._<APP>_<KEY>` | `<APP>_<KEY>` | `<APP>_<KEY>` |
| broker_token | `idp_broker_token` | managed secret | **file** `IDP_BROKER_TOKEN_FILE` (re-read on change) | store key |
| jwks | `jwt_jwks_snapshot` | optional managed secret | **file** `JWT_JWKS_SNAPSHOT_FILE`, optional Secret | store key |
| roles_claim | `jwt_roles_claim` | literal `auto` | literal `auto` | literal `auto` |

`<APP>` is the application name upper-cased with other runs folded to `_` (`prod:api` is `PROD_API`). A cluster consumer waits (`CreateContainerConfigError`) for the first delivery of the `identity` Secret rather than crash-looping on an empty issuer, and starts by itself when it lands. The JWKS snapshot Secret is optional: it is a fallback.

**Escape hatches on a consumer** (`forge.IdentityConsumer`):

- `fields = forge.IdentityFields {...}` remaps an output to a non-standard config field.
- `omit = ["scopes", "roles_claim", "login_uri", "jwks"]` declines outputs the app's config has no field for. Without it every standard output is projected under its standard name, as env nothing reads. Only those four may be omitted: issuer, audience and the native-broker outputs define the identity (use `deliver_only` to project none). An output cannot be both mapped in `fields` and omitted, and one omitted twice is refused. The delivered Secrets still carry every key, and the shape records `omit` (absent when empty, so existing shapes hash unchanged).
- `deliver_only = True` delivers the Secrets and projects nothing, for a workload still on its legacy config.

The control plane is the only writer of `forge-identity-*` and `RELIANT_IDENTITY_*`; nobody sets them, and `forge secret` never asks.

## Derived control-plane secrets

`Bundle.identity` derives `forge.ControlPlaneSecret`s you never declare (declaring an identity kind by hand is refused): kind `identity` (required), `identity_jwks` (optional) per cluster namespace that runs a hosted-identity consumer, and `identity_machine_key` per machine user. They are in `shape.control_plane_secrets` and listed in `shape.identity.deliveries`, so the existing delivery path sees them. A cluster consumer needs a connected cluster (`forge cluster connect`), refused at render otherwise.

## What is refused

- a hosted identity with no `control_plane`; `IdentityOnHost` naming something that is not a zitadel `HostInfra`;
- `seed_users` outside a `local`/`ephemeral` env; `domain` on a private instance;
- `share_with` that is not a bare env name (so never another project), is this env, or sits beside roles/smtp/seed users/machine users/domain/project name; sharing a host-run IdP;
- duplicate roles, applications, consumers; undeclared roles on a machine or seed user; a consumer naming an undeclared application or a non-service workload;
- **a hand-set field the identity fills**: a workload that sets `JWT_ISSUER`, `JWT_AUDIENCE`, `OIDC_CLIENT_ID`, `OIDC_REDIRECT_URI` or `IDP_BASE` (literal or secret ref) beside `Bundle.identity` is refused, naming both. An empty value is the unset default and gives way. To bring your own issuer for an env, declare no identity there.

`share_with` ownership (the owner declares an identity, is not a sharer, same org and project) is judged where both envs are visible: the control plane at admission, since one env's render cannot see another's.

## `forge.IdentityRef`

The primitive the projection is made of; write it yourself for an output the projection does not hand out: `env = {SECOND_ISSUER = forge.IdentityRef {output = "issuer"}}`. Application outputs take `application = "<name>"`.

## Not built yet

`login.mode = "hosted"` (accepted, recorded, unconverged), `social` (refused), the identity reconciler and delivery (control plane), `forge env up` converging the dev instance.
