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

resolves to `<registry_host>/<organization>/<project>/api`, composed from the
env's own `forge.ControlPlane` declaration. A cluster or compose workload still
must name its host: no platform owns that registry, so there is nothing to
compose from.

A host-BEARING reference on a hosted workload is used verbatim — an author who
named one meant it — and `forge env render` / `forge lint` report it when it
sits outside the subtree the platform will admit.

An env that pushes to the platform registry AND one of your own logs in to both
in one run: ours from the control-plane credential, yours from the flags.

## A denied push (realm 401 / `DENIED`)

Almost never a credential problem, and forge appends a hint naming your declared
org. In order:

1. **`organization` on `forge.ControlPlane` is not the org your token belongs
   to.** The realm scopes every token to its own org's subtree and refuses the
   rest. Fix the declaration — forge cannot guess it, and a wrong one fails here
   rather than silently pushing somewhere unexpected.
2. **It is still the scaffolded `REPLACE_ME_ORG_ID`.** `forge lint` gates on
   this.
3. **The credential expired or was revoked.** `forge login` again, or refresh
   the CI secret.

**Re-running a manual `docker login` fixes none of those.** If a push is being
refused, the question is which subtree your token may write to, not whether a
credential reached the keychain.
