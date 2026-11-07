// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// Dev-only native log forwarding — the device half of `/__forge/log`.
//
// A phone or simulator console is a dead end for anyone who is not holding the
// device, which includes every LLM agent debugging this app. This module posts
// console output, uncaught errors and unhandled promise rejections to the Metro
// dev server (metro.config.js serves the receiving `POST /__forge/log`), which
// prints them to ITS stdout as `[browser:<level>] ...`. `forge env up` already
// tees that stdout into `.forge/logs/<env>/frontend_<name>.log`, so the same
// grep works as on the web scaffolds:
//
//   grep '\[browser:error\]' .forge/logs/dev/*.log
//
// The wire protocol is the web runtime's v2 batch, unchanged:
//
//   {"entries":[{"level":"warn","msg":"…"}]}
//
// Why this is not @reliantlabs/forge-web-runtime's installDevLogging(): that
// client is written against `window`, `document`, `navigator.sendBeacon` and
// `Blob`, none of which a native runtime has, and its origin is a page-relative
// URL. This is the same design — buffered, single request in flight, errors
// flush promptly, nothing dropped silently — reduced to what a device needs.
//
// What a device needs that a browser does not:
//
//   - The Metro ORIGIN. A page posts to its own origin; a device must be told
//     where the dev server is, and it is NOT the API origin (EXPO_PUBLIC_API_URL
//     points at the backend). The origin comes from expo-constants' PUBLIC API
//     only: expoConfig.hostUri (set by the Expo CLI during development, the
//     address it advertises for the dev server), then the exp:// / exps://
//     experienceUrl and linkingUri of an Expo Go or dev-client session. No
//     internal `expo/internal/*` module is imported. A plain Debug build has
//     none of these, so EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN (injected by `forge env
//     up`) takes precedence over them; `origin` passed to
//     installNativeDevLogging() beats everything (a custom proxy). When nothing
//     resolves, one console.warn names the fix.
//   - A circuit breaker. A dead dev server (Metro stopped, device off the LAN)
//     must not turn every console call into a failing request. After a failure
//     the forwarder goes quiet for a cool-down, counts what it skipped, and
//     reports the count on the first batch that gets through. It never retries
//     a batch.
//   - Redaction. Console output on a device ends up in a log file on disk, so
//     obvious credentials (Authorization headers, bearer/JWT tokens, token /
//     password / secret / api-key pairs) are masked before they leave.
//
// DEV ONLY, structurally: the call site in app/_layout.tsx is
// `if (__DEV__) require(...)`, which Metro folds away in a production bundle —
// this module is not even included there. installNativeDevLogging() also no-ops
// unless __DEV__ is true.
//
// To turn it off: delete the `__DEV__` line in app/_layout.tsx. Nothing else
// depends on this file; metro.config.js's receiver is inert without a sender.

import Constants from "expo-constants";
import { AppState, Platform } from "react-native";

/** Console methods this module mirrors — the web runtime's levels. */
const LEVELS = ["log", "info", "warn", "error", "debug"] as const;

type Level = (typeof LEVELS)[number];

export interface NativeDevLogPayload {
  level: Level;
  msg: string;
}

export interface NativeDevLoggingOptions {
  /**
   * Metro origin to post to, e.g. "http://192.168.1.20:8081". Default:
   * EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN, then derived from expo-constants (see
   * deriveMetroOrigin).
   */
  origin?: string;
  /** Also keep writing to the real console. Default true. */
  mirrorToConsole?: boolean;
}

export const NATIVE_DEV_LOG_ENDPOINT = "/__forge/log";

/** Response header the receiver sets. Its absence means something else answered. */
const PROTOCOL_HEADER = "X-Forge-Devlog";

/** Cap a single line so a runaway loop cannot fill the disk. Matches the receiver. */
const MAX_LINE = 8_000;

/** How long a line may wait for company before it is sent. */
const FLUSH_INTERVAL_MS = 250;

/** Per-request ceiling; the receiver accepts 1 MiB. */
const MAX_BATCH_BYTES = 32 * 1024;

/** Buffer ceiling. Past it entries are dropped and counted, never queued forever. */
const MAX_BUFFER_ENTRIES = 1_000;

/** Cool-down after a failed request, doubling per consecutive failure. */
const BACKOFF_START_MS = 2_000;
const BACKOFF_MAX_MS = 30_000;

/** Give a stalled request up to this long before counting it lost. */
const REQUEST_TIMEOUT_MS = 5_000;

/** Marker for hot reload: a re-evaluated module finds and removes the old install. */
const INSTALLED_KEY = "__forgeDevLogUninstall";

type ConsoleFn = (...args: unknown[]) => void;
type GlobalHandler = (error: unknown, isFatal?: boolean) => void;

interface ErrorUtilsLike {
  getGlobalHandler?: () => GlobalHandler;
  setGlobalHandler?: (handler: GlobalHandler) => void;
}

const REDACTED = "[REDACTED]";

// What names a credential. Matches by SUFFIX, so camelCase (`authToken`),
// snake_case and kebab-case (`api_key`, `x-api-key`) are all caught; a bare
// word like "keyboard" is not, because the key must be followed by `:` or `=`
// (optionally through the key's own closing quote). The cost of matching by
// suffix is over-masking a prose "monkey: x" — the cheap side.
//
// `*key` is the exception: it names a credential only behind a secret-ish
// prefix (apiKey, privateKey, sessionKey, ...). `queryKey`, `cacheKey` and
// `primaryKey` are identifiers, and masking them mangles React Query's own
// diagnostics. A bare `key` (`?key=AIza...`) still masks, except as the tail of
// an identifier phrase ("query key: [...]", `cache-key=`; see IDENTIFIER_KEY).
//
// `credential` is the other: as an identifier's suffix (`awsCredentials:`,
// `client_credential=`) it always masks, but bare it is also an English word,
// and "No credential: this bundle..." is the scaffold's own auth warning. So a
// bare `credential(s)` masks only as a quoted key (`"credentials":`) or with
// `=`, never as prose ending in a colon.
// Group 1 is an Authorization header, 2 a cookie header, 3 a bare `key`,
// 4 a bare `credential(s)`.
const KEY_PREFIX =
  "api|private|secret|access|signing|sign|encryption|encrypt|decryption|session|auth|master|service|client|license|hmac|ssh";
const CREDENTIAL_KEY = new RegExp(
  String.raw`\b(?:((?:proxy-)?authorization)|((?:set-)?cookie)|[\w-]*(?:token|secret|password|passwd|pwd|(?:${KEY_PREFIX})[_-]?key)|[\w-]+credentials?|(key)|(credentials?))(?:\\?["']?)\s*[:=]\s*`,
  "gi",
);

// What a bare `key` must not follow. Checked by hand rather than with a regex
// lookbehind, which not every JS engine a device runs supports.
const IDENTIFIER_KEY =
  /(?:^|[^\w-])(?:query|mutation|cache|primary|foreign|sort|react|map|object)[\s-]+$/i;

/** Where a value starting at `start` ends, honouring quotes. */
function valueEnd(msg: string, start: number, kind: "bare" | "authorization" | "cookie"): number {
  const eol = (from: number): number => {
    const i = msg.indexOf("\n", from);
    return i === -1 ? msg.length : i;
  };
  // Quoted, possibly inside an already-JSON-escaped string (`\"abc\"`): mask to
  // the CLOSING quote — spaces included. Unterminated: to end of line.
  const escaped = msg.startsWith('\\"', start);
  const quote = escaped ? '\\"' : msg[start];
  if (escaped || quote === '"' || quote === "'") {
    let i = start + quote.length;
    const lineEnd = eol(i);
    while (i < lineEnd) {
      if (!escaped && msg[i] === "\\") {
        i += 2;
        continue;
      }
      if (msg.startsWith(quote, i)) return i + quote.length;
      i += 1;
    }
    return lineEnd;
  }
  // `Cookie: a=1; b=2` — everything after the colon is a credential.
  if (kind === "cookie") return eol(start);
  // An array/object value is masked whole; the bare-value scan below would
  // stop at its first quote and leave the tail of the structure behind.
  if (msg[start] === "[" || msg[start] === "{") {
    const lineEnd = eol(start);
    let depth = 0;
    let inString: string | null = null;
    for (let j = start; j < lineEnd; j += 1) {
      const c = msg[j] as string;
      if (inString) {
        if (c === "\\") j += 1;
        else if (c === inString) inString = null;
      } else if (c === '"' || c === "'") inString = c;
      else if (c === "[" || c === "{") depth += 1;
      else if ((c === "]" || c === "}") && --depth === 0) return j + 1;
    }
    return lineEnd;
  }
  let i = start;
  if (kind === "authorization") {
    const scheme = /^(?:Bearer|Basic|Token)\s+/i.exec(msg.slice(start, start + 16));
    if (scheme) i += scheme[0].length;
  }
  const stop = kind === "authorization" ? /[\s"',;}]/ : /[\s"',;&}]/;
  while (i < msg.length && !stop.test(msg[i] as string)) i += 1;
  return i;
}

/** Mask obvious credentials in one rendered log line. */
export function redact(msg: string): string {
  let out = "";
  let cursor = 0;
  CREDENTIAL_KEY.lastIndex = 0;
  for (let m = CREDENTIAL_KEY.exec(msg); m !== null; m = CREDENTIAL_KEY.exec(msg)) {
    // A bare `key` right after an identifier word ("query key", "cache-key") is not a credential.
    if (m[3] && IDENTIFIER_KEY.test(msg.slice(Math.max(0, m.index - 24), m.index))) continue;
    // A bare `credential:` with no quote and no `=` is prose ("No credential: ...").
    if (m[4] && !/["'=]/.test(m[0].slice(m[4].length))) continue;
    const start = m.index + m[0].length;
    const kind = m[1] ? "authorization" : m[2] ? "cookie" : "bare";
    const end = valueEnd(msg, start, kind);
    const value = msg.slice(start, end);
    // Nothing to mask, or already masked on a previous pass.
    if (end === start || value.startsWith(REDACTED)) continue;
    const quote = /^(\\"|"|')/.exec(value)?.[1] ?? "";
    // Keep the closing quote only if the value had one (an unterminated value
    // ran to end of line).
    const closed = quote !== "" && value.length >= quote.length * 2 && value.endsWith(quote);
    out += msg.slice(cursor, start) + quote + REDACTED + (closed ? quote : "");
    cursor = end;
    CREDENTIAL_KEY.lastIndex = end;
  }
  out += msg.slice(cursor);
  return out
    .replace(/\b(Bearer|Basic)\s+[A-Za-z0-9\-._~+/]{6,}=*/g, `$1 ${REDACTED}`)
    .replace(/\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*/g, REDACTED);
}

/**
 * Render one console argument as a string. Circular structures and BigInt are
 * the two cases a plain JSON.stringify throws on; the replacer keeps the object
 * and names the cycle instead of collapsing it to "[object Object]".
 */
function render(value: unknown): string {
  if (typeof value === "string") return value;
  if (value instanceof Error) {
    return `${value.name}: ${value.message}${value.stack ? `\n${value.stack}` : ""}`;
  }
  try {
    const seen = new WeakSet<object>();
    return (
      JSON.stringify(value, (_key, val: unknown) => {
        if (typeof val === "bigint") return `${val}n`;
        if (typeof val === "object" && val !== null) {
          if (seen.has(val)) return "[Circular]";
          seen.add(val);
        }
        return val;
      }) ?? String(value)
    );
  } catch {
    // Getters that throw, hostile toString — logging must never throw.
    try {
      return String(value);
    } catch {
      return "[unprintable]";
    }
  }
}

/**
 * The 32-hex trace id a failed RPC's error carries (ConnectClientError.traceId),
 * for the value or anything on its `cause` chain. Read structurally, as the web
 * runtime does, so one grep of the id joins this line to the server's logs.
 */
function traceIdOf(value: unknown): string | null {
  let v: unknown = value;
  for (let depth = 0; depth < 5 && v !== null && typeof v === "object"; depth++) {
    const id = (v as { traceId?: unknown }).traceId;
    if (typeof id === "string" && /^[0-9a-f]{32}$/.test(id)) return id;
    v = (v as { cause?: unknown }).cause;
  }
  return null;
}

function truncate(msg: string): string {
  return msg.length > MAX_LINE ? `${msg.slice(0, MAX_LINE)}… (truncated)` : msg;
}

function entryBytes(entry: NativeDevLogPayload): number {
  return JSON.stringify(entry).length;
}

export interface MetroOriginSources {
  hostUri?: string | null;
  experienceUrl?: string | null;
  linkingUri?: string | null;
}

const SCHEME_AUTHORITY = /^([a-z][a-z0-9+.-]*):\/\/([^/?#\s]+)/i;

function originOfUrl(value: string): string | null {
  const m = SCHEME_AUTHORITY.exec(value.trim());
  if (!m) return null;
  const scheme = (m[1] as string).toLowerCase();
  // exp:// and exps:// are Expo's dev-server schemes for http and https.
  if (scheme === "http" || scheme === "exp") return `http://${m[2]}`;
  if (scheme === "https" || scheme === "exps") return `https://${m[2]}`;
  // An app deep-link scheme ("myapp://") names no dev server.
  return null;
}

// A dev client opens as `exp+<scheme>://expo-development-client/?url=<metro url>`.
function originOfDevClientUrl(value: string): string | null {
  const m = /^exp\+[a-z0-9+.-]+:\/\/[^?#]*\?(?:[^#]*&)?url=([^&#]+)/i.exec(value.trim());
  if (!m) return null;
  try {
    return originOfUrl(decodeURIComponent(m[1] as string));
  } catch {
    return null;
  }
}

/**
 * Metro origin from expo-constants' public fields, first match wins:
 *
 *   1. hostUri       "192.168.1.20:8081" — dev only, no scheme, so http.
 *   2. experienceUrl exp(s)://host:port (Expo Go) or the `url=` of a dev-client link.
 *   3. linkingUri    exp(s)://host:port/--/ in Expo Go.
 *
 * An app deep-link scheme names no dev server and is skipped. Returns null when
 * nothing applies (a bundle with no dev server behind it).
 */
export function deriveMetroOrigin(sources: MetroOriginSources): string | null {
  const host = sources.hostUri?.trim();
  if (host && /^[^/?#\s:]+(?::\d+)?$/.test(host.split("/")[0] as string)) {
    return `http://${host.split("/")[0]}`;
  }
  for (const url of [sources.experienceUrl, sources.linkingUri]) {
    if (!url) continue;
    const origin = originOfDevClientUrl(url) ?? originOfUrl(url);
    if (origin) return origin;
  }
  return null;
}

/**
 * Origin from EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN. A plain Debug build (no dev
 * client) has no Expo manifest, so `forge env up` injects the dev server's
 * origin through the environment instead. The access below MUST stay a static
 * `process.env.NAME` property read: Expo's Babel preset inlines exactly that
 * shape at bundle time, and a dynamic lookup would see an empty object.
 */
function envOrigin(): { origin: string | null; invalid: boolean } {
  const raw = process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN?.trim();
  if (!raw) return { origin: null, invalid: false };
  const m = SCHEME_AUTHORITY.exec(raw);
  const scheme = m?.[1]?.toLowerCase();
  if (m && (scheme === "http" || scheme === "https")) {
    return { origin: `${scheme}://${m[2]}`, invalid: false };
  }
  return { origin: null, invalid: true };
}

const NO_ORIGIN_FIX =
  "run the app through `forge env up`, set EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN to the Metro origin " +
  "(e.g. http://192.168.1.20:8081), or pass { origin } to installNativeDevLogging()";

/** Resolve the Metro origin the device can actually reach, or null. */
function resolveOrigin(env: { origin: string | null }): string | null {
  if (env.origin) return env.origin;
  try {
    return deriveMetroOrigin({
      hostUri: Constants.expoConfig?.hostUri,
      experienceUrl: Constants.experienceUrl,
      linkingUri: Constants.linkingUri,
    });
  } catch {
    // no manifest in this runtime
    return null;
  }
}

class DevLogger {
  private original: Partial<Record<Level, ConsoleFn>> = {};
  private buffer: NativeDevLogPayload[] = [];
  private bufferedBytes = 0;
  private flushTimer: ReturnType<typeof setTimeout> | undefined;
  private requestInFlight = false;
  private errorFlushQueued = false;
  private droppedCount = 0;
  private droppedReason = "";
  private suspendedUntil = 0;
  private failures = 0;
  // Re-entry guards. `forwarding` covers the fetch() call (a polyfill that logs
  // does so synchronously from inside it); `capturing` covers rendering (a
  // hostile toString that logs). A line logged under either is still mirrored
  // to the real console — only forwarding is suppressed.
  private forwarding = false;
  private capturing = false;
  // True while the previous global handler runs. RN's own handler reports the
  // very error we just captured through console.error; capturing that too
  // would print every uncaught error twice.
  private inGlobalHandler = false;
  private previousHandler: GlobalHandler | undefined;
  private appStateSub: { remove(): void } | undefined;
  private url = "";

  constructor(
    private readonly origin: string,
    private readonly mirrorToConsole: boolean,
  ) {
    this.url = `${origin.replace(/\/+$/, "")}${NATIVE_DEV_LOG_ENDPOINT}`;
  }

  install(): void {
    for (const level of LEVELS) {
      const previous = console[level] as ConsoleFn;
      this.original[level] = previous;
      console[level] = (...args: unknown[]) => {
        if (this.mirrorToConsole) previous.apply(console, args);
        if (level === "error" && this.inGlobalHandler) return;
        this.capture(level, args);
      };
    }

    const errorUtils = (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils;
    if (errorUtils?.getGlobalHandler && errorUtils.setGlobalHandler) {
      const previous = errorUtils.getGlobalHandler();
      this.previousHandler = previous;
      errorUtils.setGlobalHandler((error, isFatal) => {
        this.capture("error", isFatal ? ["[fatal]", error] : [error]);
        this.inGlobalHandler = true;
        try {
          previous(error, isFatal);
        } finally {
          this.inGlobalHandler = false;
        }
      });
    }

    // Backgrounding is the native unload: the OS may suspend the JS thread
    // before the 250ms timer fires.
    try {
      this.appStateSub = AppState.addEventListener("change", (state) => {
        if (state !== "active") this.flush();
      });
    } catch {
      // No AppState in this runtime; the timer still flushes.
    }

    // The one line that proves the whole path end to end: if this is missing
    // from the log file, nothing from this device will arrive.
    this.capture("info", [
      `[forge-devlog] native logging attached (${Platform.OS}) -> ${this.origin}`,
    ]);
  }

  uninstall(): void {
    this.clearFlushTimer();
    this.flush();
    for (const level of LEVELS) {
      const previous = this.original[level];
      if (previous) console[level] = previous;
    }
    this.original = {};
    const errorUtils = (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils;
    if (this.previousHandler && errorUtils?.setGlobalHandler) {
      errorUtils.setGlobalHandler(this.previousHandler);
    }
    this.previousHandler = undefined;
    this.appStateSub?.remove();
    this.appStateSub = undefined;
  }

  private countDropped(count: number, reason: string): void {
    this.droppedCount += count;
    this.droppedReason = reason;
  }

  private capture(level: Level, args: unknown[]): void {
    if (this.forwarding || this.capturing) return;

    // A suspended or full forwarder drops WITHOUT rendering: rendering is the
    // expensive part, and a dead sink is exactly when output is heaviest.
    if (Date.now() < this.suspendedUntil) {
      this.countDropped(1, "dev server unreachable");
      return;
    }
    if (this.buffer.length >= MAX_BUFFER_ENTRIES) {
      this.countDropped(1, "buffer full");
      return;
    }

    this.capturing = true;
    let entry: NativeDevLogPayload;
    try {
      // Rendered NOW, not at flush time: a console argument is a live object
      // that may be mutated before the batch leaves.
      let msg = args.map(render).join(" ");
      const traceId = args.map(traceIdOf).find((id) => id !== null);
      if (traceId && !msg.includes(traceId)) msg += ` trace_id=${traceId}`;
      // Truncate FIRST: it bounds the scan on a runaway line, and a secret cut
      // mid-value still runs to the end of the line, so it is masked either way.
      entry = { level, msg: redact(truncate(msg)) };
    } finally {
      this.capturing = false;
    }

    const wasEmpty = this.buffer.length === 0;
    this.buffer.push(entry);
    this.bufferedBytes += entryBytes(entry);

    if (this.bufferedBytes >= MAX_BATCH_BYTES) {
      this.flush();
      return;
    }
    if (level === "error") {
      // An error is what someone is waiting for; it must not sit in a timer.
      // The microtask lets a same-tick burst still leave as one request.
      if (!this.errorFlushQueued) {
        this.errorFlushQueued = true;
        queueMicrotask(() => {
          this.errorFlushQueued = false;
          this.flush();
        });
      }
      return;
    }
    if (wasEmpty) this.armTimer();
  }

  private armTimer(): void {
    if (this.flushTimer !== undefined) return;
    this.flushTimer = setTimeout(() => {
      this.flushTimer = undefined;
      this.flush();
    }, FLUSH_INTERVAL_MS);
  }

  private clearFlushTimer(): void {
    if (this.flushTimer === undefined) return;
    clearTimeout(this.flushTimer);
    this.flushTimer = undefined;
  }

  private takeBatch(): NativeDevLogPayload[] {
    const batch: NativeDevLogPayload[] = [];
    let size = '{"entries":[]}'.length;
    while (this.buffer.length > 0) {
      const entry = this.buffer[0] as NativeDevLogPayload;
      const cost = entryBytes(entry) + (batch.length > 0 ? 1 : 0);
      // Always take at least one: an oversized line must still leave or it
      // stalls everything behind it.
      if (batch.length > 0 && size + cost > MAX_BATCH_BYTES) break;
      this.buffer.shift();
      this.bufferedBytes -= entryBytes(entry);
      batch.push(entry);
      size += cost;
    }
    if (this.buffer.length === 0) this.bufferedBytes = 0;
    return batch;
  }

  // The "we lost lines" notice rides the next batch rather than needing a
  // request of its own — a dead sink that looks like working code is the
  // failure this exists to prevent.
  private drainDroppedNotice(): void {
    if (this.droppedCount === 0) return;
    const notice: NativeDevLogPayload = {
      level: "warn",
      msg: `[forge-devlog] dropped ${this.droppedCount} lines (${this.droppedReason})`,
    };
    this.buffer.unshift(notice);
    this.bufferedBytes += entryBytes(notice);
    this.droppedCount = 0;
    this.droppedReason = "";
  }

  private onFailure(batchSize: number, reason: string): void {
    this.requestInFlight = false;
    this.failures += 1;
    this.countDropped(batchSize, reason);
    const cooldown = Math.min(BACKOFF_START_MS * 2 ** (this.failures - 1), BACKOFF_MAX_MS);
    this.suspendedUntil = Date.now() + cooldown;
    // What was already queued behind the failed batch is dropped too: it
    // would only fail the same way, and keeping it grows the pile.
    if (this.buffer.length > 0) {
      this.countDropped(this.buffer.length, reason);
      this.buffer = [];
      this.bufferedBytes = 0;
    }
  }

  flush(): void {
    if (this.requestInFlight) return;
    if (Date.now() < this.suspendedUntil) return;
    this.drainDroppedNotice();
    if (this.buffer.length === 0) return;
    this.clearFlushTimer();

    const batch = this.takeBatch();
    if (batch.length === 0) return;
    const body = JSON.stringify({ entries: batch });

    this.requestInFlight = true;
    let settled = false;
    const settle = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timeout);
      fn();
    };
    const controller = new AbortController();
    const timeout = setTimeout(() => {
      settle(() => this.onFailure(batch.length, "request timed out"));
      // Release the socket: a stalled request must not outlive its failure.
      controller.abort();
    }, REQUEST_TIMEOUT_MS);

    // The guard wraps the CALL, not the promise: a fetch implementation that
    // logs does so synchronously from inside fetch().
    let pending: Promise<Response>;
    this.forwarding = true;
    try {
      pending = fetch(this.url, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body,
        signal: controller.signal,
      });
    } catch {
      this.forwarding = false;
      settle(() => this.onFailure(batch.length, "request failed"));
      return;
    }
    this.forwarding = false;

    pending.then(
      (response) => {
        settle(() => {
          this.requestInFlight = false;
          if (!response.ok) {
            this.onFailure(batch.length, `endpoint returned ${response.status}`);
          } else if (!response.headers.get(PROTOCOL_HEADER)) {
            // 2xx without the header: Metro is not the one that answered —
            // typically the origin is the API, or an older receiver.
            this.onFailure(batch.length, "endpoint is not the forge dev-log receiver");
          } else {
            this.failures = 0;
            if (this.buffer.length > 0 || this.droppedCount > 0) this.flush();
          }
        });
      },
      () => settle(() => this.onFailure(batch.length, "request failed")),
    );
  }
}

/**
 * Mirror console output, uncaught errors and unhandled rejections to Metro,
 * which prints them into .forge/logs/<env>/frontend_<name>.log.
 *
 * Unhandled rejections need no listener of their own: in development React
 * Native's promise rejection tracker (Hermes, or the `promise` polyfill) reports
 * each one through ExceptionsManager, which calls console.error — captured here.
 *
 * Safe to call more than once: a second call replaces the first.
 */
export function installNativeDevLogging(options: NativeDevLoggingOptions = {}): void {
  if (!__DEV__) return;
  const registry = globalThis as unknown as Record<string, unknown>;
  const previous = registry[INSTALLED_KEY];
  if (typeof previous === "function") (previous as () => void)();

  const env = options.origin ? { origin: null, invalid: false } : envOrigin();
  const origin = options.origin ?? resolveOrigin(env);
  if (!origin) {
    // Not installed yet, so this is the real console.warn: it can neither be
    // forwarded nor recurse.
    console.warn(
      `[forge-devlog] no dev server origin found; device logs will not reach .forge/logs. ` +
        (env.invalid ? "EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN is not an http(s) URL. " : "") +
        `To fix: ${NO_ORIGIN_FIX}.`,
    );
    return;
  }

  const logger = new DevLogger(origin, options.mirrorToConsole ?? true);
  logger.install();
  registry[INSTALLED_KEY] = () => {
    logger.uninstall();
    delete registry[INSTALLED_KEY];
  };
}

/** Restore the original console methods and global error handler. */
export function uninstallNativeDevLogging(): void {
  const registry = globalThis as unknown as Record<string, unknown>;
  const uninstall = registry[INSTALLED_KEY];
  if (typeof uninstall === "function") (uninstall as () => void)();
}
