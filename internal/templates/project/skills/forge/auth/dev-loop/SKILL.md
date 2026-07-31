---
name: dev-loop
description: Getting a token locally — mint one against a shared secret, or run the opt-in dev IdP container for a real browser sign-in; the two-hostname issuer trap and the opaque-vs-JWT access token trap.
---

# Authenticating locally

Dev relaxes only NON-security ergonomics (permissive CORS, verbose errors). **Authentication is enforced in every mode**: `SetupAuth` builds a real validator whether dev or prod, and no environment variable turns it off. So a local call to a protected RPC needs a real token. Three ways, cheapest first.

## 1. Mint one against a shared secret

No IdP, no container. Set `jwt_signing_method: HS256` and put a `jwt_secret` in the env's secret provider (`.env.dev`), then sign a short-lived token with the same secret.

Fast, and the reason it is not enough on its own: the token has whatever claims you typed, so it never exercises the claim shape your real issuer produces, and it cannot test the sign-in flow at all.

## 2. A real browser sign-in against the dev IdP

`docker-compose.yml` ships an identity-provider container behind an **opt-in compose profile** — the same `profiles:` mechanism that gates `app-debug`. It is OFF by default, deliberately: it is a large image and a second web server, and a project authenticating with API keys, a worker, or a service with no browser must not pay for an IdP it never calls.

```sh
docker compose --profile <idp-profile> up -d --wait
```

Then register a client in its admin console (type: **single-page application** — a PUBLIC client, no secret) and hand the app `jwt_issuer`, `jwt_jwks_url` and `jwt_audience`. **Read the comments in `docker-compose.yml`** for this project's actual image version, ports, admin URL and env: they are pinned there and this document will not track a bump.

The app's identity env is EMPTY by default rather than pre-pointed at that container, and that is not an oversight. A configured-but-unreachable issuer makes the server refuse to start, so a default pointing at a container nobody started would break `docker compose up` for every project that never wanted an IdP. Empty means no key material: closed, and bootable.

## 3. Your real staging IdP

Point `jwt_issuer` at it. Nothing local is needed beyond network reachability, and the claim shape is exactly production's.

## Two traps that are not your wiring

**A containerized IdP has TWO hostnames.** The browser reaches it at `localhost:<port>`, so that is the issuer it mints tokens under and the `iss` your server enforces; your server reaches the same process at the compose service name. Set BOTH: `jwt_issuer` to the browser URL, `jwt_jwks_url` to the in-network URL.

Discovery cannot bridge that gap — OIDC Discovery §4.3 requires a document's `issuer` to equal the URL it was fetched from, so a document fetched in-network either declares the in-network name (and fails the issuer check against the browser-issued tokens) or declares the browser name (and fails the discovery check). This is exactly why `jwt_jwks_url` takes PRECEDENCE over discovery when both are set: the explicit pair is the only consistent answer for a two-hostname deployment.

**Some IdPs issue OPAQUE access tokens by default.** An opaque token is a random string, not a JWT, and will never validate against a JWKS however correct everything else is. If sign-in succeeds, a token comes back, and every API call still 401s with a malformed-token error, check that the client requested a **registered API resource / audience** — that is usually what switches the issuer to JWT format — and that `jwt_audience` matches the indicator you registered.

## Reading the failures

| symptom | cause |
| --- | --- |
| Server refuses to start, naming a URL | The issuer or JWKS endpoint is unreachable or wrong. Intended: it fails loudly instead of 401ing every request. |
| Every call 401s, token looks like a random string | Opaque access token — request an API resource (above). |
| Every call 401s, `alg` complaint | `jwt_signing_method` disagrees with the key material (HS\* secret vs RS\*/ES\* JWKS). |
| `iss` mismatch | Two-hostname trap (above), or `jwt_issuer` has a stray trailing slash. |
| Public RPC works, authenticated one 401s with a valid token | The token is fine; check `jwt_audience` — a token minted for another API of the same issuer is correctly rejected. |
