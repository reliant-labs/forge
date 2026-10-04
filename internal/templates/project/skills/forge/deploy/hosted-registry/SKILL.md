---
name: hosted-registry
description: The platform container registry — ONE TOKEN (your control-plane credential, no registry password), why hosted pushes log themselves in, what a bare hosted image resolves to, and how to triage a denied push (realm 401) without reaching for docker login.
---

# The hosted registry: ONE TOKEN

A hosted env's images go to the PLATFORM registry, and you authenticate it with
the credential you already have. There is no registry password to mint, no
second CI secret, and nothing to rotate.

```
forge registry login <env>        # no --username, no --password-*
```

forge resolves the same `rlat_` it reaches the control plane with — `--token`,
then the env's declared `token_env` (default `$FORGE_CONTROL_PLANE_TOKEN`), then
what `forge login` stored. It presents that on **stdin** as username `forge`;
the realm authenticates on the credential alone, so the username is a label in
a log rather than an identity.

**Passing `--username` / `--password-*` for the platform host is REFUSED.** Not
pedantry: a hand-passed credential that happened to work would teach you this
registry has a password of its own, and the next pipeline you wrote would carry
one — until it expired, in a job nobody remembered had two secrets.

## You usually run no login at all

`forge env build <env> --push`, `forge env deploy <env>` and `forge env up` log
themselves in before their first push, once per host per process. So:

- the scaffolded `release.yml` has **no login step**;
- `build-images.yml` has one only for a non-hosted build env;
- a missing credential fails **before anything is built**, rather than after
  minutes of compiling, with the three remedies named in the order forge checks
  them.

That is also what lets the in-app Deploy button push from a cloud daemon: the
daemon's token is deposited where forge already looks, so no part of the flow
needs a registry password it has no way to obtain.

One `docker login` is enough for everything, because all three of forge's push
paths read the docker credential store — `docker push` for images, oras for the
config bundle, go-containerregistry for a static site release.

## A bare image is the normal hosted shape

```
image = "api"      # on a forge.OnHosted workload
```

resolves to `<registry_host>/<org>/<project>/api`: the host from the env's
`forge.ControlPlane` (defaulting to Reliant's registry), `<org>` from the
credential — forge asks the control plane which organization your token acts
for, so there is no `organization` to declare and none to get wrong. A cluster or compose workload still
must name its host: no platform owns that registry, so there is nothing to
compose from.

A host-BEARING reference on a hosted workload is used verbatim — an author who
named one meant it — and `forge env render` / `forge lint` report it when it
sits outside the subtree the platform will admit.

An env that pushes to the platform registry AND one of your own logs in to both
in one run: ours from the control-plane credential, yours from the flags.

## A denied push (realm 401 / `DENIED`)

forge composes the push address from the org your credential acts for, so a
denial is never a mis-declared org. In order:

1. **The credential expired or was revoked.** `forge login` again, or refresh
   the CI secret.
2. **The token holds `deploy:read` but not `deploy:write`.** A push needs both
   the org's subtree and write access; check the token's scopes with
   `forge cloud token list`.

**Re-running a manual `docker login` fixes neither.** If a push is being
refused, the question is what your token may write, not whether a credential
reached the keychain.

## When forge cannot learn your org

A build, deploy, release cut or `forge registry login` for a hosted env asks the
control plane which organization the credential acts for (one call per
command). With no credential it stops at the top and says so — authenticate and
re-run. `forge env render` and `forge lint` need no credential: with none they
compose no base, and report a host-bearing image as the weaker, unverified fact.
