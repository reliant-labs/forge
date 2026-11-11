---
name: deploy/managed-identity/mobile
description: A mobile (Expo `rn`) app signing in as its own managed-identity client, in the dev loop only (forge.IdentityOnHost) — the kind-native IdentityApplication derived for every rn frontend, the callback read from app.json's expo.scheme, only a static app.json is read, the EXPO_PUBLIC_FORGE_OIDC_* env vars `forge env up` injects and their exact precedence against shell, KCL, .env.<env> and Expo's own .env, opting a frontend out (native_sign_in = False), declaring callbacks yourself, why hosted identity refuses kind native for now, and how --device-host serves a physical device (the dev IdP answers at the LAN IPv4 address, the issuer moves there for the run, the API follows), and its limits (IPv4 only, social sign-in, networks, LAN exposure), and https universal links / App Links as the sign-in callback (associated_domains, apple_team_id, android_cert_fingerprints, the rendered apple-app-site-association and assetlinks.json and exactly how each target must serve them, the with-forge-links Expo plugin, the client's https-or-fallback choice, iOS 17.4, the dev loop, what is not yet proven).
---

# A mobile app signs in as its own client

**Dev loop only today.** An `rn` (Expo) frontend in an env whose identity is `forge.IdentityOnHost` gets a `kind = "native"` application **derived**, named `<env>:<frontend>`: nothing to declare. It is a public PKCE client at the IdP's own hosted pages (no broker, no secret, no `login_uri`); its tokens are accepted unchanged by the env's services (same issuer and audience). `forge env up` registers it and injects `EXPO_PUBLIC_FORGE_OIDC_{ISSUER,CLIENT_ID,SCOPES,REDIRECT_URI,POST_LOGOUT_REDIRECT_URI}` into the frontend. The issuer is `http://localhost:<idp port>`, which reaches the iOS simulator; a physical device or an emulator needs `--device-host` (next section).

**A hosted identity refuses `kind = "native"`** ("native identity applications are not yet supported on hosted identity"), in KCL and in the shape: the control plane cannot register a native client yet, and a declaration it never converged would read as converged. A control plane is never sent native applications meanwhile (`forge env build`/`deploy` print a note when one is left out). The refusal lifts together with hosted registration.

**Scaffolding the first frontend edits the dev env only.** It declares the dev identity provider and `identity` in `deploy/kcl/dev/main.k`, and leaves staging and prod alone (a hosted identity is a managed, billable instance, so it is your call) and prints the exact edit instead. To give a hosted env sign-in, in `deploy/kcl/<env>/main.k` add `import ..identity as idn` after `import ..workloads as wl`, and `identity = idn.IDENTITY` in the `forge.Bundle` before `secret_provider =`, then `forge env deploy <env>`.

## A physical device: `--device-host`

```
forge env up dev --device-host 192.168.1.20      # this machine's LAN address; or FORGE_DEV_DEVICE_HOST
```

Zitadel decides which instance serves a request, and the `iss` it mints, from the request's **Host header**. A device cannot reach `localhost`, and the API validates one exact `iss`, so the device origin and the API's `jwt_issuer` have to be the same string. forge makes them so:

- The dev IdP serves **both** `localhost` and the device host as two domains of **one instance**. Zitadel's setup adds a domain when `ZITADEL_EXTERNALDOMAIN` changes between boots and never removes one, so forge cold-starts under `localhost` and, when the instance does not yet answer at the device host, restarts it **once** with the device host. Users, keys, the PAT and every registered app stay; there is no data reset, in either direction.
- The identity converge publishes `http://<device host>:<idp port>` as the issuer for the run, so the API's `jwt_issuer`/`idp_base`, the login broker, `.forge/identity/<name>.json` and `EXPO_PUBLIC_FORGE_OIDC_ISSUER` all carry **one** issuer. The API is not made to accept two.
- Web sign-in on this machine keeps working: the browser only talks to the app, and the app's broker dials the IdP at the published origin (this machine's own LAN address). The iOS simulator keeps working too, but it signs in at the device origin for that run, because the app is handed one issuer.
- Running again **without** `--device-host` publishes `localhost` again and every output follows; nothing is wiped. The device origin keeps answering (Zitadel cannot drop a domain) but nobody is pinned to it.
- The RN template's OIDC config accepts an `http` issuer on loopback **or a private-network address** (RFC 1918, `.local`, link-local, ULA) in development only; `https` is unchanged and a public host over `http` is still refused. Existing apps own `config.ts`: copy `isDevNetworkHost` and the `isSecureEndpoint` change (`migrations/v1.4.0/native-auth`).
- **The API's userinfo lookup (what fills `Claims.Email`) needs a dev opt-in to reach the LAN-address issuer.** Plain HTTP to anything but loopback is refused by default, in every environment. In a `--device-host` run `forge env up` sets `JWT_USERINFO_ALLOW_LOCAL_HTTP=true` on the host workloads that consume the identity, and the scaffolded `SetupAuth` turns it into `userInfo.AllowLocalPlainHTTP()`. The opt-in applies only to an issuer that is plain HTTP at an IPv4 literal; the userinfo URL must have the issuer's own origin; and every request goes out through a transport that ignores `HTTP_PROXY`/`HTTPS_PROXY` and refuses at **connect time**, against this machine's interfaces as they are then, any address that is not loopback or one of them. The JWKS and OIDC discovery fetches are confined the same way: `auth.JWTConfig.HTTPClient`, set from `auth.LocalIssuerHTTPClient(cfg.JwtIssuer)` (nil unless the issuer is plain HTTP at this machine's IPv4 address), and passed to `oauth2.Discover` as its doer. The login broker's IdP client (`devidp.Client`) gets the same transport when the scaffolded broker sets `LocalOnly: cfg.JwtUserinfoAllowLocalHttp` (an explicit signal from this same knob, never a look at the interfaces) and its `idp_base` is plain HTTP to a non-loopback host. forge sets the variable unless the workload declares it itself, in which case the declared value stands. Never set it in a deployed environment. **Existing apps own `internal/app/auth.go`, `internal/app/login_broker.go` and `pkg/config`:** add `bool jwt_userinfo_allow_local_http` (any free field number; 51 in new scaffolds; `env_var: "JWT_USERINFO_ALLOW_LOCAL_HTTP"`, default `false`) to the config proto, regenerate, add `if cfg.JwtUserinfoAllowLocalHttp { userInfo.AllowLocalPlainHTTP() }` between building `auth.NewUserInfo(...)` and `authenticator.UseUserInfo(userInfo)`, add `LocalOnly: cfg.JwtUserinfoAllowLocalHttp,` to the broker's `devidp.Client{...}`, and add the `issuerClient`/`discoveryDoer` helpers with `HTTPClient: httpClient` on `auth.JWTConfig` for the JWKS and discovery fetches (all in `migrations/v1.4.0/native-auth`, step 2 of its API half). Without it a device-host run still signs in; `Claims.Email` just stays empty (an invitation-by-address first sign-in does not bind).

Rules and limits:

- **The device host must be this machine's own LAN IPv4 address** (an IPv4 literal on one of its interfaces). A hostname (`my-mac.local`), an IPv6 literal, `10.0.2.2` (an Android emulator's alias for the host), a tunnel or another machine is none of those: forge prints one notice saying why and to pass the LAN IPv4 address, and keeps the localhost issuer. The reason is the API and the broker send tokens and credentials to the issuer over plain HTTP and accept that only for an IPv4 address they can confirm is this machine's at connect time; a name resolves to whatever the network says. For an emulator pass the machine's LAN IPv4, or keep localhost and `adb reverse tcp:<idp port> tcp:<idp port>`.
- Only a run that targets a native frontend **with a native application** puts the IdP on the device host. A frontend with `native_sign_in = False` does not.
- **If the IdP cannot be made to serve the device host** (the restart that adds the domain fails, or it does not answer there within 15 s), forge puts the IdP back under localhost, prints why, and the run continues on the localhost issuer: devices cannot sign in, and nothing else is lost.
- **Social sign-in (Google, GitHub) on this machine does not work during a `--device-host` run.** The IdP's social callback is `<issuer>/idps/callback`, so it moves to `http://<LAN IPv4>:<idp port>/idps/callback` for the run; Google refuses plain-HTTP private-IP redirect URIs and an OAuth app registered for the localhost callback no longer matches. forge prints a warning when the identity declares social providers. Password sign-in is unaffected. Run without `--device-host` to test social sign-in.
- **Security: the dev IdP listens on every interface, on every dev run, with or without `--device-host`** (Zitadel has no listen-address setting; forge cannot bind it to loopback). It is plain HTTP and holds the committed dev credentials from `idp-steps.yaml`. forge says so on the IdP's start line every time (`listens on all interfaces: reachable from your LAN over plain HTTP with the dev credentials`), because a developer who never passes `--device-host` is exposed all the same and that line is the only place they would learn it, and prints a fuller notice on a `--device-host` run. Use a network you trust. Do not use it on shared Wi-Fi.
- **Stop the stack before changing networks** (`forge env down`, then `forge env up --device-host <new address>`). The address is the issuer. A network change leaves the IdP, the API and the app on the old address, the userinfo and broker connections refuse it at connect time (it is no longer this machine's), and a session minted under the old issuer signs in again.
- iOS App Transport Security: a Debug build allows the `http` issuer; a Release build needs an exception.

## Where the callback comes from: `expo.scheme`

The callback is `<scheme>:/oauthredirect` (post-logout `<scheme>:/signedout`), RFC 8252 7.1. `<scheme>` is the entry of `app.json`'s `expo.scheme` that is a valid private-use scheme: **lowercase letters, digits and `-`, joined by at least one `.`** (`com.myapp.mobile`). It is not the bundle id: forge reads **only** `expo.scheme`, never `ios.bundleIdentifier` or `android.package`. New scaffolds set `expo.scheme` to the sanitised reverse-DNS id `com.<project>.<frontend>` (lowercase, no separators), one string, and use the same spelling as the bundle id and package. An existing app keeps whatever bundle id it shipped with: **never change `bundleIdentifier` or `package` to match the scheme**, because that renames a published app to the OS and the stores. Set `expo.scheme` alone. It is also what registers the link with the OS, so the scheme the app answers and the scheme the IdP trusts are one fact. `expo.scheme` is a string or a list; with a list the first valid entry wins (a plain `mobile` link scheme beside it is ignored by forge, but expo-linking warns on every launch when more than one is registered, so a single string is the scaffold's shape).

```json
{ "expo": { "scheme": "com.myapp.mobile", "ios": { "bundleIdentifier": "com.MyApp.mobile" } } }
```

If no entry qualifies, **no client is registered** and the app runs on its mock provider; `forge env up` says so loudly, with the exact `app.json` edit. Three ways out, each leaving the bundle id and package as they are: set `"scheme": "<reverse.dns.id>"` (one string); or declare `redirect_uris` on a `kind = "native"` application (below); or set `native_sign_in = False` on the frontend.

**Only a static `app.json` is read.** forge does not run Node to evaluate `app.config.js` / `app.config.ts`. A project that has only those gets a warning and no client; declare the callback instead (below) and keep the same scheme in `expo.scheme`.

## Env var precedence

Highest first, per variable (each OIDC variable is resolved on its own):

1. **KCL `env_vars`** of the frontend (and the typed `config`), unless the key is listed in `FORGE_ENV_OVERRIDE`
2. **The frontend's `.env.<env>`** (or its `env_file`), unless listed in `FORGE_ENV_OVERRIDE`
3. **Your shell** (`EXPO_PUBLIC_FORGE_OIDC_ISSUER=… forge env up dev`)
4. **What forge injects** from the converged client
5. **Expo's own `.env` / `.env.local` / `.env.development`**: lowest, because Expo's loader never replaces a variable already present in the dev server's environment, and every layer above is already there

So a value written in KCL or `.env.<env>` always beats the injected one, and Expo's own dotenv files never do. `FORGE_ENV_OVERRIDE=A,B` hands those keys back to the shell, above KCL and `.env.<env>`. Only `EXPO_PUBLIC_` names are inlined into the bundle, as static `process.env.X` reads.

**The five are one unit.** Their scopes and redirects belong to the client forge registered, so when any layer sets `EXPO_PUBLIC_FORGE_OIDC_ISSUER` or `_CLIENT_ID` forge injects **none** of the five and prints one line saying why (the app is then pointed at another provider; set the rest yourself). Otherwise it fills each variable no layer set, so one can still be overridden alone.

**`EXPO_PUBLIC_API_URL`** (the API origin) follows the same layers for a native frontend: a value in KCL, or in the frontend's `.env.<env>`, beats forge's injected one, and a value exported in your shell beats the injected one too (the origin is a per-device choice: LAN address, emulator alias, tunnel). Declared values still outrank the shell, as above; `FORGE_ENV_OVERRIDE=EXPO_PUBLIC_API_URL` hands the key back to the shell. Web frontends are unchanged: forge's origin wins.

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

## https universal links / App Links as the callback

Any app on a device can claim a private-use scheme such as `com.acme.mobile:/oauthredirect` (RFC 8252 §8.6). An https callback on a host you own is bound to **your signed app** by two association files that host serves. Opt in per app; nothing changes until you declare a domain.

```python
forge.IdentityApplication {
    name = "dev:mobile", kind = "native", frontend = "mobile"
    associated_domains = ["app.acme.example"]        # bare lowercase hostnames, in order
}
forge.Frontend {
    name = "mobile", type = "rn", path = "frontends/mobile", ...
    apple_team_id = "23P64LQTZD"                      # the team that signs the iOS build
    android_cert_fingerprints = {
        dev = ["debug"],                              # this machine's android/app/debug.keystore
        prod = ["AA:BB:CC:...:99"],                   # the Play App Signing key (not the upload key)
    }
    associated_site = "web"                           # optional: the static site that serves the host
}
```

**What forge derives.** With `associated_domains` and no `redirect_uris`, the registered redirects are `[https://<first domain>/oauthredirect, <scheme>:/oauthredirect]` and the post-logout URIs the `/signedout` beside each. The IdP accepts either entry and the client picks by platform (below). A `redirect_uris` you declare keeps your order. **Validation** (a render error, in KCL and in the shape): every domain is a bare lowercase hostname (no scheme, port, path, wildcard or IP: a wildcard cannot serve `webcredentials`), none repeats, the host of **every https redirect must be one of the domains**, and domains with an explicit redirect list need at least one https entry. `apple_team_id` is ten upper-case letters and digits; each fingerprint is a SHA-256 colon-hex (32 bytes) or `"debug"`.

**The association files** are facts about the binary, so they are per env and rendered at `forge env deploy`, never baked into the build. They are a `/.well-known/` overlay on the site that serves the host (a promoted release stays byte-identical; each env serves its own fingerprints):

- `/.well-known/apple-app-site-association`: `applinks` (components `/oauthredirect`, `/signedout`) and `webcredentials`, listing `<apple_team_id>.<ios.bundleIdentifier>`. Both services are declared: `webcredentials` is what the auth-session callback is checked against; `applinks` makes the link a universal link outside the session too.
- `/.well-known/assetlinks.json`: `delegate_permission/common.handle_all_urls` for `android.package` with every fingerprint listed for the **current env** (an iOS-only env renders only the AASA, an Android-only env only assetlinks). `"debug"` is resolved with the real `keytool` against `frontends/<name>/android/app/debug.keystore` (per checkout; run `expo prebuild` once so it exists); forge reads no keystore itself.

**Serving requirements, on every target** (both platforms fetch from the host root, over https with a valid certificate): **`Content-Type: application/json`**, **no redirect** (no apex/www, trailing-slash or `.html` rewrite), the AASA with **no extension**, at most 128 KiB. The extensionless file means a host cannot infer the type, so each target sets it: *hosted* serves the StaticSite spec's `wellKnown` that way (the platform must; see the control-plane requirement in the evidence doc); *OnFirebase* pins it in `firebase.json` `headers`; *OnBucket* uploads the objects `application/json` and `no-cache`; for your own CDN or server, set it in **your** header config. forge never emulates another tool's header file. The site must be on that host: name it with `associated_site` when the env has more than one static site; an env with none refuses the deploy.

**The Expo plugin** `plugins/with-forge-links.js` (in `expo.plugins` of new scaffolds) adds `ios.associatedDomains` (`applinks:<host>` and `webcredentials:<host>`) and `android.intentFilters` (VIEW, `autoVerify`, https `<host>`, `/oauthredirect` and `/signedout`) at prebuild. It reads `FORGE_ASSOCIATED_DOMAINS` and `FORGE_APP_LINK_MODE` from the environment, else from `.forge-app-links.json`, which `forge env up` writes beside the app (prebuild is your command, not forge's). With no domain it does nothing, so a domain-less app ships no claim it cannot verify. `FORGE_APP_LINK_MODE=developer` appends `?mode=developer` to the entitlements so a dev-signed build fetches the file from the host and not from Apple's CDN (which can lag by hours); forge sets it for a **local** env only and never for a deployed one. Entitlements and intent filters are native config: rebuild the dev client after changing them. On iOS, enable **Associated Domains** on the App ID (Xcode automatic signing for the team does).

**The client** (`src/lib/auth/redirect.ts`, `oidc-provider.ts`, `native.ts`) chooses **once per attempt**: https when `EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI` is https **and** the device is Android or iOS 17.4+, otherwise the private-use `EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI` (forge injects the second registered entry; it belongs to the OIDC unit above, so another provider's issuer withholds it too). The chosen URI is used for the authorize `redirect_uri`, the callback match and the token exchange (RFC 6749 §4.1.3), and `preferUniversalLinks: true` is passed to `openAuthSessionAsync` for the https link (iOS 17.4+ then builds `ASWebAuthenticationSession` with `.https(host:path:)`; Android ignores it and a verified App Link is delivered to the app). **iOS below 17.4 keeps the private-use fallback**: the session cannot intercept an https callback there, so an https redirect would never complete. Logout uses the post-logout URI beside the redirect it chose. One `app/oauthredirect.tsx` route still serves both forms for cold-start deliveries.

**Dev loop.** Both platforms need a public host with a CA-trusted certificate (not localhost, a LAN IP or a self-signed certificate): a Reliant-hosted dev static site, with the files served per the requirements above. The IdP can stay the local dev Zitadel; it only needs the https redirect registered. Run `forge env up dev`, `expo prebuild`, then a dev build signed by your team.

**Not proven here.** The files, the plugin, the redirect choice and the deploy serving are unit-tested; signing in through the https link on a signed iOS build and an Android emulator is pending device acceptance (`docs/proposals/mobile-conformance-evidence.md`). Existing apps opt in with `forge skill load migrations/v1.4.0/native-auth` ("Opt in to https universal links").
