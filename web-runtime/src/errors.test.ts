// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// stripServerFraming and userMessage used to exist TWICE: once here and once
// in the scaffold's src/lib/format-utils.ts, each carrying a comment
// promising to keep the other in sync. The scaffold copy was written once and
// never overwritten, so any correction to the framing regex could only ever
// reach projects generated after it. These tests pin the single surviving
// implementation.
//
// The framing it strips is the transport's, not the server's: connect-es
// prefixes `ConnectError.message` with "[code] ". Everything after that
// prefix is the message the backend deliberately wrote, and it is shown
// verbatim — this function used to also delete a trailing category tag the
// backend appended to every message, which is a defect that was fixed on the
// backend instead (forge/pkg/svcerr).
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import {
  ConnectClientError,
  FORGE_ERROR_REASON_HEADER,
  normalizeError,
  stripServerFraming,
  userMessage,
} from "./errors.js";

/**
 * A ConnectClientError from a SECOND COPY of this package: same shape, same
 * `name`, different class object. npm produces exactly this whenever two
 * dependencies want different versions of @reliantlabs/forge-web-runtime (it
 * nests the loser), and a bundler produces it whenever one package is pulled
 * in as both ESM and CJS. Declared here rather than imported so the class
 * identity is genuinely different, which is the whole point.
 */
class ForeignConnectClientError extends Error {
  readonly code: string;
  readonly connectCode?: number;
  readonly status: number;
  readonly retryable: boolean;
  readonly reason: string | null;

  constructor(args: {
    message: string;
    code: string;
    connectCode?: number;
    status: number;
    retryable: boolean;
    reason?: string | null;
  }) {
    super(args.message);
    this.name = "ConnectClientError";
    this.code = args.code;
    this.connectCode = args.connectCode;
    this.status = args.status;
    this.retryable = args.retryable;
    this.reason = args.reason ?? null;
  }
}

/** The same idea for ConnectError — a second copy of @connectrpc/connect. */
class ForeignConnectError extends Error {
  readonly code: number;
  readonly metadata: Headers;
  readonly rawMessage: string;
  details: unknown[] = [];

  constructor(message: string, code: number, metadata?: HeadersInit) {
    super(`[code_${code}] ${message}`);
    this.name = "ConnectError";
    this.rawMessage = message;
    this.code = code;
    this.metadata = new Headers(metadata ?? {});
    this.cause = undefined;
  }
}

describe("stripServerFraming", () => {
  it("drops the leading gRPC code prefix", () => {
    expect(stripServerFraming("[not_found] no such task")).toBe("no such task");
  });

  it("leaves the server's own message intact — every word of it is display copy", () => {
    expect(
      stripServerFraming("[invalid_argument] expires_at must follow issued_at"),
    ).toBe("expires_at must follow issued_at");
  });

  it("is idempotent, so double-stripping a normalized message is safe", () => {
    const once = stripServerFraming("[invalid_argument] name is required");
    expect(stripServerFraming(once)).toBe(once);
    expect(once).toBe("name is required");
  });

  it("leaves an unframed message alone", () => {
    expect(stripServerFraming("plain message")).toBe("plain message");
  });
});

describe("userMessage", () => {
  const FALLBACK = "Something went wrong. Please try again.";

  it("prefers a ConnectError's rawMessage", () => {
    const err = new ConnectError("no such task", Code.NotFound);
    expect(userMessage(err)).toBe("no such task");
  });

  it("strips framing from a plain Error", () => {
    expect(userMessage(new Error("[internal] boom"))).toBe("boom");
  });

  it("falls back when the error carries no presentable text", () => {
    expect(userMessage(new Error(""))).toBe(FALLBACK);
    expect(userMessage(null)).toBe(FALLBACK);
    expect(userMessage(undefined)).toBe(FALLBACK);
  });

  it("stringifies a non-Error throw", () => {
    expect(userMessage("[internal] bare string")).toBe("bare string");
  });

  it("takes a caller-supplied fallback", () => {
    expect(userMessage(null, "Could not save.")).toBe("Could not save.");
  });

  it("agrees with normalizeError on the message it produces", () => {
    const err = new ConnectError("[not_found] gone", Code.NotFound);
    expect(userMessage(err)).toBe(normalizeError(err).message);
  });
});

describe("normalizeError across a package boundary", () => {
  // The observed failure: EVERY error in the app arrived as
  // code:"unknown", connectCode:undefined, status:500, reason:null.
  //
  // normalizeError's pass-through guard used `err instanceof
  // ConnectClientError`, which compares CLASS IDENTITY, and class identity is
  // per module instance rather than per package. With two copies of this
  // package in one bundle the guard is false for a genuinely-normalized
  // error, and — because a normalized error's `name` is "ConnectClientError",
  // not "ConnectError" — it misses the ConnectError branch too and lands in
  // the generic Error branch, which has no code, no connectCode and no reason
  // to give. Silent, total, and on every error.
  it("passes through an already-normalized error from a second copy of this package", () => {
    const foreign = new ForeignConnectClientError({
      message: "no such patient",
      code: "not_found",
      connectCode: Code.NotFound,
      status: 404,
      retryable: false,
      reason: "patient_not_found",
    });

    const out = normalizeError(foreign);

    expect(out.code).toBe("not_found");
    expect(out.connectCode).toBe(Code.NotFound);
    expect(out.status).toBe(404);
    expect(out.reason).toBe("patient_not_found");
    expect(out.retryable).toBe(false);
    // Pass-through, not a re-wrap: the interceptor chain must never double-wrap.
    expect(out).toBe(foreign);
  });

  it("recognises a second copy's ConnectClientError via instanceof", () => {
    const foreign = new ForeignConnectClientError({
      message: "boom",
      code: "internal",
      status: 500,
      retryable: false,
    });
    expect(foreign instanceof ConnectClientError).toBe(true);
  });

  it("does not mistake an unrelated Error for a normalized one", () => {
    expect(new Error("plain") instanceof ConnectClientError).toBe(false);
    expect(
      new ConnectError("x", Code.Internal) instanceof ConnectClientError,
    ).toBe(false);
    // Right name, no payload — not a ConnectClientError.
    const impostor = new Error("x");
    impostor.name = "ConnectClientError";
    expect(impostor instanceof ConnectClientError).toBe(false);
  });

  // A pin on a DEPENDENCY guarantee we now rely on deliberately:
  // @connectrpc/connect defines `static [Symbol.hasInstance]` on ConnectError
  // that matches on shape, so `err instanceof ConnectError` already survives
  // two copies of @connectrpc/connect. That is why normalizeError does not
  // carry a second, competing duck-type of its own for that branch. If a
  // future connect release drops the shim, this test fails first and tells us
  // to add one.
  it("still extracts a ConnectError raised by a second copy of @connectrpc/connect", () => {
    const metadata = { [FORGE_ERROR_REASON_HEADER]: "patient_not_found" };
    const raw = "no such patient";

    const foreign = normalizeError(
      new ForeignConnectError(raw, Code.NotFound, metadata),
    );
    const native = normalizeError(
      new ConnectError(raw, Code.NotFound, metadata),
    );

    // Field-for-field identical to the same error from our own copy — the
    // property under test, without blessing a particular `code` spelling.
    expect(foreign.code).toBe(native.code);
    expect(foreign.connectCode).toBe(Code.NotFound);
    expect(foreign.status).toBe(404);
    expect(foreign.reason).toBe("patient_not_found");
    expect(foreign.message).toBe("no such patient");
  });
});
