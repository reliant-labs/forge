// Scaffolded by forge. Yours to own — forge will not overwrite this file.
// Shared in-memory fakes for the auth tests (not a test file: jest only runs *.test.ts).
//
// jest-expo auto-mocks every expo native module so its functions resolve
// `undefined` — a test over those mocks passes vacuously. These fakes behave
// like the real thing instead: Node's crypto for real SHA-256 / randomness, a
// Map-backed keystore, a scriptable identity provider and system browser.
import { base64UrlEncode } from "../src/lib/auth/oidc";
import type { OidcConfig } from "../src/lib/auth/config";
import type { CryptoLike, FetchLike, HttpResponse } from "../src/lib/auth/oidc";
import type { BrowserLike, BrowserResult } from "../src/lib/auth/oidc-provider";
import type { LaunchMarker, SecureStoreLike } from "../src/lib/auth/token-store";

export const ISSUER = "https://idp.example.com";
export const CLIENT_ID = "native-client";
export const REDIRECT = "com.acme.app:/oauthredirect";
export const POST_LOGOUT = "com.acme.app:/signedout";

export const config: OidcConfig = {
  issuer: ISSUER,
  clientId: CLIENT_ID,
  scopes: ["openid", "profile", "email", "offline_access"],
  redirectUri: REDIRECT,
  postLogoutRedirectUri: POST_LOGOUT,
};

// A real SHA-256 (FIPS 180-4) in plain TypeScript, so the tests need no Node
// typings; auth-oidc.test.ts proves it against the RFC 7636 Appendix B vector.
const K = Uint32Array.from(
  "428a2f98 71374491 b5c0fbcf e9b5dba5 3956c25b 59f111f1 923f82a4 ab1c5ed5 d807aa98 12835b01 243185be 550c7dc3 72be5d74 80deb1fe 9bdc06a7 c19bf174 e49b69c1 efbe4786 0fc19dc6 240ca1cc 2de92c6f 4a7484aa 5cb0a9dc 76f988da 983e5152 a831c66d b00327c8 bf597fc7 c6e00bf3 d5a79147 06ca6351 14292967 27b70a85 2e1b2138 4d2c6dfc 53380d13 650a7354 766a0abb 81c2c92e 92722c85 a2bfe8a1 a81a664b c24b8b70 c76c51a3 d192e819 d6990624 f40e3585 106aa070 19a4c116 1e376c08 2748774c 34b0bcb5 391c0cb3 4ed8aa4a 5b9cca4f 682e6ff3 748f82ee 78a5636f 84c87814 8cc70208 90befffa a4506ceb bef9a3f7 c67178f2"
    .split(" ")
    .map((h) => parseInt(h, 16)),
);

export function sha256(data: Uint8Array): Uint8Array {
  const rotr = (x: number, n: number) => (x >>> n) | (x << (32 - n));
  const bitLen = data.length * 8;
  const padded = new Uint8Array(((data.length + 9 + 63) >> 6) << 6);
  padded.set(data);
  padded[data.length] = 0x80;
  const view = new DataView(padded.buffer);
  view.setUint32(padded.length - 8, Math.floor(bitLen / 2 ** 32));
  view.setUint32(padded.length - 4, bitLen >>> 0);
  const h = Uint32Array.from([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  const w = new Uint32Array(64);
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, hh] = h;
    for (let i = 0; i < 64; i++) {
      const t1 =
        (hh + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + K[i] + w[i]) >>> 0;
      const t2 = ((rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0;
      hh = g;
      g = f;
      f = e;
      e = (d + t1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) >>> 0;
    }
    [a, b, c, d, e, f, g, hh].forEach((v, i) => (h[i] = (h[i] + v) >>> 0));
  }
  const out = new Uint8Array(32);
  const ov = new DataView(out.buffer);
  h.forEach((v, i) => ov.setUint32(i * 4, v));
  return out;
}

export const utf8 = (s: string): Uint8Array => new TextEncoder().encode(s);

export const nodeCrypto: CryptoLike = {
  // Test-only randomness; the app uses expo-crypto's CSPRNG (native.ts).
  getRandomBytes: (n) => Uint8Array.from({ length: n }, () => Math.floor(Math.random() * 256)),
  sha256Base64: async (input) => {
    const b64url = base64UrlEncode(sha256(utf8(input)));
    return b64url.replace(/-/g, "+").replace(/_/g, "/") + "=".repeat((4 - (b64url.length % 4)) % 4);
  },
};

export function jwt(claims: Record<string, unknown>): string {
  const enc = (o: unknown) => base64UrlEncode(utf8(JSON.stringify(o)));
  return `${enc({ alg: "RS256", typ: "JWT" })}.${enc(claims)}.c2lnbmF0dXJl`;
}

export class FakeSecureStore implements SecureStoreLike {
  AFTER_FIRST_UNLOCK_THIS_DEVICE_ONLY = 3;
  readonly items = new Map<string, string>();
  readonly optionsSeen: (number | undefined)[] = [];
  available = true;
  failWith: Error | null = null;
  /** Every call yields this many ms first, as a real keychain round trip does. */
  latencyMs = 0;
  /** Runs inside a write/delete, before it lands: lets a test interleave another caller. */
  beforeWrite: ((op: "set" | "delete", key: string) => void | Promise<void>) | null = null;
  async isAvailableAsync() {
    return this.available;
  }
  private async check(key: string, options?: { keychainAccessible?: number }) {
    if (!/^[A-Za-z0-9._-]+$/.test(key))
      throw new Error(`Invalid key provided to SecureStore: ${key}`);
    this.optionsSeen.push(options?.keychainAccessible);
    if (this.latencyMs) await new Promise((r) => setTimeout(r, this.latencyMs));
    if (this.failWith) throw this.failWith;
  }
  async getItemAsync(key: string, options?: { keychainAccessible?: number }) {
    await this.check(key, options);
    return this.items.get(key) ?? null;
  }
  async setItemAsync(key: string, value: string, options?: { keychainAccessible?: number }) {
    await this.check(key, options);
    await this.beforeWrite?.("set", key);
    this.items.set(key, value);
  }
  async deleteItemAsync(key: string, options?: { keychainAccessible?: number }) {
    await this.check(key, options);
    await this.beforeWrite?.("delete", key);
    this.items.delete(key);
  }
}

export class FakeMarker implements LaunchMarker {
  constructor(public value = true) {}
  isSet() {
    return this.value;
  }
  set() {
    this.value = true;
  }
}

export interface Recorded {
  url: string;
  method: string;
  form: Record<string, string>;
}

/** A scriptable identity provider speaking just enough OIDC. */
export class FakeIdp {
  requests: Recorded[] = [];
  now = () => Date.now();
  issuerInDiscovery = ISSUER;
  offline = false;
  tokenCalls = 0;
  /** Override the next token-endpoint reply. */
  nextTokenReply: (() => Promise<{ status: number; body: unknown }>) | null = null;
  tokenDelayMs = 0;
  expiresIn = 3600;
  revocationEndpoint: string | null = `${ISSUER}/oauth/v2/revoke`;
  endSessionEndpoint: string | null = `${ISSUER}/oidc/v1/end_session`;
  discoveryStatus = 200;
  sub = "user-1";
  idTokenClaims: Record<string, unknown> = {};
  /** Set by the browser fake from the authorize request. */
  lastNonce = "";
  refreshCounter = 0;
  /** Refresh tokens the IdP still honours. */
  validRefresh = new Set<string>();
  revoked: string[] = [];
  /** When set, the next access token minted. */
  accessCounter = 0;

  fetch: FetchLike = async (url, init) => {
    if (this.offline) throw new TypeError("Network request failed");
    if (init.signal?.aborted) throw new Error("aborted");
    const form: Record<string, string> = {};
    if (init.body) for (const [k, v] of new URLSearchParams(init.body)) form[k] = v;
    this.requests.push({ url, method: init.method, form });
    if (url.endsWith("/.well-known/openid-configuration")) {
      return reply(this.discoveryStatus, {
        issuer: this.issuerInDiscovery,
        authorization_endpoint: `${ISSUER}/oauth/v2/authorize`,
        token_endpoint: `${ISSUER}/oauth/v2/token`,
        ...(this.revocationEndpoint ? { revocation_endpoint: this.revocationEndpoint } : {}),
        ...(this.endSessionEndpoint ? { end_session_endpoint: this.endSessionEndpoint } : {}),
      });
    }
    if (url === this.revocationEndpoint) {
      this.revoked.push(form.token);
      return reply(200, {});
    }
    if (url === `${ISSUER}/oauth/v2/token`) {
      this.tokenCalls++;
      if (this.tokenDelayMs) await new Promise((r) => setTimeout(r, this.tokenDelayMs));
      if (this.nextTokenReply) {
        const r = this.nextTokenReply;
        this.nextTokenReply = null;
        const { status, body } = await r();
        return reply(status, body);
      }
      if (form.grant_type === "refresh_token" && !this.validRefresh.has(form.refresh_token)) {
        return reply(400, { error: "invalid_grant", error_description: "refresh token revoked" });
      }
      return reply(200, this.mint(form.grant_type === "authorization_code"));
    }
    return reply(404, {});
  };

  mint(withNonce: boolean) {
    this.accessCounter++;
    this.refreshCounter++;
    const refresh = `refresh-${this.refreshCounter}`;
    this.validRefresh.add(refresh);
    return {
      access_token: `access-${this.accessCounter}`,
      token_type: "Bearer",
      expires_in: this.expiresIn,
      refresh_token: refresh,
      id_token: jwt({
        iss: ISSUER,
        aud: [CLIENT_ID],
        sub: this.sub,
        email: `${this.sub}@example.com`,
        name: `Name ${this.sub}`,
        ...(withNonce ? { nonce: this.lastNonce } : {}),
        ...this.idTokenClaims,
      }),
    };
  }

  tokenRequests() {
    return this.requests.filter((r) => r.url === `${ISSUER}/oauth/v2/token`);
  }
}

function reply(status: number, body: unknown): HttpResponse {
  return { ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(body) };
}

export type BrowserScript = (authorizeUrl: URL) => BrowserResult | Promise<BrowserResult>;

/** A system browser that follows `script` and records what it was asked to open. */
export class FakeBrowser implements BrowserLike {
  opened: { url: URL; redirectUri: string; ephemeral: boolean }[] = [];
  constructor(
    private readonly idp: FakeIdp,
    public script: BrowserScript = successScript,
  ) {}
  async openAuthSession(
    url: string,
    redirectUri: string,
    options: { preferEphemeralSession: boolean },
  ) {
    const u = new URL(url);
    this.idp.lastNonce = u.searchParams.get("nonce") ?? "";
    this.opened.push({ url: u, redirectUri, ephemeral: options.preferEphemeralSession });
    return this.script(u);
  }
}

export const successScript: BrowserScript = (u) => ({
  type: "success",
  url: `${u.searchParams.get("redirect_uri")}?code=the-code&state=${encodeURIComponent(u.searchParams.get("state") ?? "")}`,
});
