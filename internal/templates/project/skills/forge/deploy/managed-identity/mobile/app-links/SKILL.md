---
name: deploy/managed-identity/mobile/app-links
description: Opt native sign-in into HTTPS universal links and Android App Links — associated domains, platform identifiers, association-file serving, Expo plugin, ordered callbacks and private-scheme fallback. Signed-device HTTPS acceptance remains pending.
---

# HTTPS universal links / App Links

Native sign-in and env precedence: `forge skill load deploy/managed-identity/mobile`.

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

**Hosted compatibility:** a nonempty `StaticSite.spec.wellKnown` requires the control plane to advertise `GetCapabilities.resourceFeatures: ["static-site-well-known"]`. Older servers fail before publishing or sealing with upgrade guidance; forge never silently removes the files. This resource capability is separate from `bundleDocumentFeatures`.

**The Expo plugin** `plugins/with-forge-links.js` (in `expo.plugins` of new scaffolds) adds `ios.associatedDomains` (`applinks:<host>` and `webcredentials:<host>`) and `android.intentFilters` (VIEW, `autoVerify`, https `<host>`, `/oauthredirect` and `/signedout`) at prebuild. It reads `FORGE_ASSOCIATED_DOMAINS` and `FORGE_APP_LINK_MODE` from the environment, else from `.forge-app-links.json`, which `forge env up` writes beside the app (prebuild is your command, not forge's). With no domain it does nothing, so a domain-less app ships no claim it cannot verify. `FORGE_APP_LINK_MODE=developer` appends `?mode=developer` to the entitlements so a dev-signed build fetches the file from the host and not from Apple's CDN (which can lag by hours); forge sets it for a **local** env only and never for a deployed one. Entitlements and intent filters are native config: rebuild the dev client after changing them. On iOS, enable **Associated Domains** on the App ID (Xcode automatic signing for the team does).

**The client** (`src/lib/auth/redirect.ts`, `oidc-provider.ts`, `native.ts`) chooses **once per attempt**: https when `EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI` is https **and** the device is Android or iOS 17.4+, otherwise the private-use `EXPO_PUBLIC_FORGE_OIDC_FALLBACK_REDIRECT_URI` (forge injects the second registered entry; it belongs to the OIDC unit above, so another provider's issuer withholds it too). The chosen URI is used for the authorize `redirect_uri`, the callback match and the token exchange (RFC 6749 §4.1.3), and `preferUniversalLinks: true` is passed to `openAuthSessionAsync` for the https link (iOS 17.4+ then builds `ASWebAuthenticationSession` with `.https(host:path:)`; Android ignores it and a verified App Link is delivered to the app). **iOS below 17.4 keeps the private-use fallback**: the session cannot intercept an https callback there, so an https redirect would never complete. Logout uses the post-logout URI beside the redirect it chose. One `app/oauthredirect.tsx` route still serves both forms for cold-start deliveries.

**Dev loop.** Both platforms need a public host with a CA-trusted certificate (not localhost, a LAN IP or a self-signed certificate): a Reliant-hosted dev static site, with the files served per the requirements above. The IdP can stay the local dev Zitadel; it only needs the https redirect registered. Run `forge env up dev`, `expo prebuild`, then a dev build signed by your team.

**Not proven here.** The files, the plugin, the redirect choice and the deploy serving are unit-tested; signing in through the https link on a signed iOS build and an Android emulator is pending device acceptance (`docs/proposals/mobile-conformance-evidence.md`). Existing apps opt in with `forge skill load migrations/v1.5.0/native-auth` ("Opt in to https universal links").
