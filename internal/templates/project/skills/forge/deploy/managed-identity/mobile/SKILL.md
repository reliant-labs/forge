---
name: deploy/managed-identity/mobile
description: A mobile (Expo `rn`) app signing in as its own managed-identity client, in the dev loop only (forge.IdentityOnHost) — the kind-native IdentityApplication derived for every rn frontend, the callback read from app.json's expo.scheme, only a static app.json is read, the EXPO_PUBLIC_FORGE_OIDC_* env vars `forge env up` injects and their exact precedence against shell, KCL, .env.<env> and Expo's own .env, opting a frontend out (native_sign_in = False), declaring callbacks yourself, why hosted identity refuses kind native for now, and why --device-host does not move the issuer.
---

# A mobile app signs in as its own client

**Dev loop only today.** An `rn` (Expo) frontend in an env whose identity is `forge.IdentityOnHost` gets a `kind = "native"` application **derived**, named `<env>:<frontend>`: nothing to declare. It is a public PKCE client at the IdP's own hosted pages (no broker, no secret, no `login_uri`); its tokens are accepted unchanged by the env's services (same issuer and audience). `forge env up` registers it and injects `EXPO_PUBLIC_FORGE_OIDC_{ISSUER,CLIENT_ID,SCOPES,REDIRECT_URI,POST_LOGOUT_REDIRECT_URI}` into the frontend. The issuer is localhost, so it reaches the iOS simulator; `--device-host` does not move it (Zitadel resolves its instance from the Host header), and an Android emulator needs `adb reverse`.

**A hosted identity refuses `kind = "native"`** ("native identity applications are not yet supported on hosted identity"), in KCL and in the shape: the control plane cannot register a native client yet, and a declaration it never converged would read as converged. A control plane is never sent native applications meanwhile (`forge env build`/`deploy` print a note when one is left out). The refusal lifts together with hosted registration.

## Where the callback comes from: `expo.scheme`

The callback is `<scheme>:/oauthredirect` (post-logout `<scheme>:/signedout`), RFC 8252 7.1. `<scheme>` is the entry of `app.json`'s `expo.scheme` that is a valid private-use scheme: **lowercase letters, digits and `-`, joined by at least one `.`** (`com.myapp.mobile`). It is not the bundle id. A bundle id may differ on purpose (iOS and Android allow uppercase and `_`; a project named `MyApp` or `my_app` is valid), and `expo.scheme` is also what registers the link with the OS, so the scheme the app answers and the scheme the IdP trusts are one fact. `expo.scheme` is a string or a list; with a list the first valid entry wins (a plain `mobile` link scheme beside it is ignored).

```json
{ "expo": { "scheme": "com.myapp.mobile", "ios": { "bundleIdentifier": "com.MyApp.mobile" } } }
```

If no entry qualifies, **no client is registered** and the app runs on its mock provider; `forge env up` says so loudly, with the exact `app.json` edit.

**Only a static `app.json` is read.** forge does not run Node to evaluate `app.config.js` / `app.config.ts`. A project that has only those gets a warning and no client; declare the callback instead (below) and keep the same scheme in `expo.scheme`.

## Env var precedence

Highest first, per variable (each OIDC variable is resolved on its own):

1. **KCL `env_vars`** of the frontend (and the typed `config`), unless the key is listed in `FORGE_ENV_OVERRIDE`
2. **The frontend's `.env.<env>`** (or its `env_file`), unless listed in `FORGE_ENV_OVERRIDE`
3. **Your shell** (`EXPO_PUBLIC_FORGE_OIDC_ISSUER=… forge env up dev`)
4. **What forge injects** from the converged client
5. **Expo's own `.env` / `.env.local` / `.env.development`**: lowest, because Expo's loader never replaces a variable already present in the dev server's environment, and every layer above is already there

So a value written in KCL or `.env.<env>` always beats the injected one, and Expo's own dotenv files never do. `FORGE_ENV_OVERRIDE=A,B` hands those keys back to the shell, above KCL and `.env.<env>`. Only `EXPO_PUBLIC_` names are inlined into the bundle, as static `process.env.X` reads.

## Opting a frontend out

```python
forge.Frontend {name = "kiosk", type = "rn", native_sign_in = False, ...}
```

Registers no derived client and injects no `EXPO_PUBLIC_FORGE_OIDC_*` for that frontend. `rn` frontends only. It suppresses the *derived* client; an application you declare is honoured.

## Declaring one yourself

To pin callbacks or add a universal link, or when the project uses `app.config.js`:

```python
forge.IdentityApplication {
    name = "dev:mobile", kind = "native", frontend = "mobile"
    # ORDERED: the first is what the app signs in with.
    redirect_uris = ["com.acme.mobile:/oauthredirect", "https://acme.example/oauthredirect"]
    logout_uris = ["com.acme.mobile:/signedout", "https://acme.example/signedout"]
}
```

A callback is a private-use reverse-DNS scheme with one slash (RFC 8252 7.1) or an `https` URL; `http` is the dev loop's alone; no wildcards. If the identity provider itself refuses the client, `forge env up` warns, leaves the app on its mock provider and still publishes the web app's sign-in.
