// An anonymous call must work. A signed-out browser is not a broken one.
//
// forge's server-side auth is fail-closed per RPC, but a PUBLIC RPC still has
// to serve a caller who offers no credential — that is what "public" means,
// and an anonymous browse of a marketing page or a public listing is the
// normal first visit for every app. The failure this guards against is a
// transport that treats "no token" as an error, or that attaches a literal
// `Authorization: Bearer null`, which a server correctly rejects with a 401
// on an endpoint that never needed a credential at all.
//
// The assertion derives from the HEADERS THE INTERCEPTOR ACTUALLY SET, using
// the REAL authInterceptor from @reliant-labs/web-runtime — the same one
// src/lib/connect.ts installs. A reimplementation here would prove nothing
// about what ships.

import { authInterceptor } from "@reliant-labs/web-runtime";
import { describe, expect, it } from "vitest";

import { clearSession, getAccessToken, setSession } from "./oidc-storage";

/**
 * Runs one request through the real auth interceptor and reports the
 * Authorization header it ended up with, plus whether the call completed.
 *
 * The request/response shapes are the minimum the interceptor touches
 * (`req.header`, and passing through to next), which is why this needs no
 * server, no service descriptor and no protobuf.
 */
async function callThrough(
  getToken: () => Promise<string | null>,
): Promise<{ completed: boolean; authorization: string | null }> {
  const interceptor = authInterceptor({ getToken });
  const header = new Headers();
  let reached = false;

  const next = async (req: unknown) => {
    reached = true;
    return { ...(req as object), message: {} };
  };

  // The interceptor only reads `header`; the rest of the UnaryRequest surface
  // is irrelevant to it, so the cast keeps this test free of protobuf setup.
  await (interceptor as unknown as (n: typeof next) => (r: unknown) => Promise<unknown>)(
    next,
  )({ header });

  return { completed: reached, authorization: header.get("Authorization") };
}

describe("an anonymous call with getToken() === null", () => {
  it("reaches the server with NO Authorization header at all", async () => {
    clearSession();
    // This is exactly what the AuthProvider returns when signed out.
    expect(getAccessToken()).toBeNull();

    const result = await callThrough(async () => getAccessToken());

    // The call is not blocked...
    expect(result.completed).toBe(true);
    // ...and no header is invented. Not "Bearer null", not "Bearer ", absent.
    expect(result.authorization).toBeNull();
  });

  it("never sends the string 'null' or 'undefined' as a credential", async () => {
    for (const token of [null, undefined] as (string | null | undefined)[]) {
      const result = await callThrough(async () => token ?? null);
      expect(result.completed).toBe(true);
      expect(result.authorization).toBeNull();
    }
  });

  it("treats an expired session as anonymous rather than as an error", async () => {
    // The in-memory store stops OFFERING an expired token, so this path is
    // reached in a real app whenever a session outlives its expiry.
    setSession({ accessToken: "expired-token", expiresIn: -1 });
    const result = await callThrough(async () => getAccessToken());
    expect(result.completed).toBe(true);
    expect(result.authorization).toBeNull();
    clearSession();
  });

  it("attaches the bearer token once a session exists", async () => {
    // The positive control: without this, the three assertions above could
    // all pass on an interceptor that never attaches anything.
    setSession({ accessToken: "at-live-1", expiresIn: 3600 });
    const result = await callThrough(async () => getAccessToken());
    expect(result.completed).toBe(true);
    expect(result.authorization).toBe("Bearer at-live-1");
    clearSession();
  });
});
