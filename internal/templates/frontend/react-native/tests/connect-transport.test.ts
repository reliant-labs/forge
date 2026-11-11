// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { afterEach, describe, expect, it, jest } from "@jest/globals";
import { createFileRegistry, fromJson, type DescService } from "@bufbuild/protobuf";
import { FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import {
  createTransport,
  setAuthTokenGetter,
  setTokenRejectedHandler,
  transport,
} from "../src/lib/connect";
import { getEventBus, initEventBus } from "../src/lib/events";
import { currentEpoch, session } from "../src/lib/auth/session";

// The production default must be expo/fetch, not the global. The suite-wide
// setup (tests/setup/expo-fetch.ts) maps `expo/fetch` onto globalThis.fetch so
// ordinary app tests can stub the network; THIS file opts out of that mapping
// with its own factory, so the exported `transport` is observable as using
// expo/fetch and never the global.
const mockExpoFetch = jest.fn();
jest.mock("expo/fetch", () => ({
  fetch: (...args: unknown[]) => mockExpoFetch(...args),
}));

// Drives the app's real transport (src/lib/connect) with a fake network that
// hands back a chunked ReadableStream. It proves the Connect framing,
// incremental server-stream delivery, cancellation and the interceptor stack
// in Jest. It does NOT exercise expo/fetch's native networking: that needs the
// Hermes runtime on a device or simulator.
const file = fromJson(FileDescriptorProtoSchema, {
  name: "echo.proto",
  package: "test.v1",
  syntax: "proto3",
  messageType: ["EchoRequest", "EchoReply"].map((name) => ({
    name,
    field: [
      { name: "text", number: 1, type: "TYPE_STRING", label: "LABEL_OPTIONAL", jsonName: "text" },
    ],
  })),
  service: [
    {
      name: "EchoService",
      method: [
        // No idempotency declared: a write, never replayed after a 401.
        { name: "Say", inputType: ".test.v1.EchoRequest", outputType: ".test.v1.EchoReply" },
        {
          name: "Read",
          inputType: ".test.v1.EchoRequest",
          outputType: ".test.v1.EchoReply",
          options: { idempotencyLevel: "NO_SIDE_EFFECTS" },
        },
        {
          name: "Put",
          inputType: ".test.v1.EchoRequest",
          outputType: ".test.v1.EchoReply",
          options: { idempotencyLevel: "IDEMPOTENT" },
        },
        {
          name: "Watch",
          inputType: ".test.v1.EchoRequest",
          outputType: ".test.v1.EchoReply",
          serverStreaming: true,
        },
      ],
    },
  ],
});
const service: DescService = createFileRegistry(file, () => undefined).getService(
  "test.v1.EchoService",
)!;

interface Echo {
  say(req: { text: string }, opts?: { signal?: AbortSignal }): Promise<{ text: string }>;
  read(req: { text: string }, opts?: { signal?: AbortSignal }): Promise<{ text: string }>;
  put(req: { text: string }, opts?: { signal?: AbortSignal }): Promise<{ text: string }>;
  watch(req: { text: string }, opts?: { signal?: AbortSignal }): AsyncIterable<{ text: string }>;
}

interface Call {
  url: string;
  method: string;
  headers: Headers;
  body: string;
  signal: AbortSignal | undefined;
}

type FetchLike = (url: string, init: RequestInit) => Promise<unknown>;

// connect-web only reads status, headers, body, json() and arrayBuffer().
function fakeFetch(handler: (call: Call) => Promise<unknown>): typeof globalThis.fetch {
  const fn: FetchLike = async (url, init) =>
    handler({
      url,
      method: String(init.method),
      headers: new Headers(init.headers),
      body: new TextDecoder().decode(init.body as Uint8Array),
      signal: init.signal ?? undefined,
    });
  return fn as unknown as typeof globalThis.fetch;
}

function jsonResponse(value: unknown) {
  return {
    status: 200,
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => value,
    arrayBuffer: async () => new TextEncoder().encode(JSON.stringify(value)).buffer,
    body: null,
  };
}

function envelope(flags: number, payload: unknown): Uint8Array {
  const bytes = new TextEncoder().encode(JSON.stringify(payload));
  const out = new Uint8Array(5 + bytes.length);
  out[0] = flags;
  new DataView(out.buffer).setUint32(1, bytes.length);
  out.set(bytes, 5);
  return out;
}

const message = (text: string) => envelope(0, { text });
const endStream = () => envelope(2, {});

// A response whose body the test feeds chunk by chunk. Like expo/fetch, it
// errors the body with the abort reason when the signal fires.
function streamingResponse(signal: AbortSignal | undefined) {
  let controller!: ReadableStreamDefaultController<Uint8Array>;
  const state = { cancelled: false };
  const body = new ReadableStream<Uint8Array>({
    start: (c) => {
      controller = c;
    },
    cancel: () => {
      state.cancelled = true;
    },
  });
  signal?.addEventListener("abort", () => {
    try {
      controller.error(signal.reason);
    } catch {
      // already closed
    }
  });
  return {
    state,
    push: (chunk: Uint8Array) => controller.enqueue(chunk),
    close: () => controller.close(),
    response: {
      status: 200,
      headers: new Headers({ "content-type": "application/connect+json" }),
      body,
    },
  };
}

function concat(...parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let offset = 0;
  for (const p of parts) {
    out.set(p, offset);
    offset += p.length;
  }
  return out;
}

function client(fetchImpl: typeof globalThis.fetch): Echo {
  return createClient(service, createTransport(fetchImpl)) as unknown as Echo;
}

afterEach(() => {
  setAuthTokenGetter(async () => null);
  setTokenRejectedHandler(null);
});

describe("app transport over an injected fetch", () => {
  it("sends unary JSON through the interceptor stack and decodes the reply", async () => {
    setAuthTokenGetter(async () => "tok-1");
    const calls: Call[] = [];
    const echo = client(
      fakeFetch(async (call) => {
        calls.push(call);
        return jsonResponse({ text: "pong" });
      }),
    );

    await expect(echo.say({ text: "ping" })).resolves.toMatchObject({ text: "pong" });

    expect(calls).toHaveLength(1);
    expect(new URL(calls[0].url).pathname).toBe("/test.v1.EchoService/Say");
    expect(calls[0].method).toBe("POST");
    expect(calls[0].headers.get("content-type")).toBe("application/json");
    expect(JSON.parse(calls[0].body)).toEqual({ text: "ping" });
    expect(calls[0].headers.get("authorization")).toBe("Bearer tok-1");
    expect(calls[0].headers.get("traceparent")).toMatch(
      /^00-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$/,
    );
  });

  it("delivers server-stream messages as chunks arrive, across chunk and UTF-8 boundaries", async () => {
    const wire = concat(message("héllo 🌍"), message("second"));
    // Cut mid-envelope-header, then inside the 4-byte emoji, then inside a
    // multi-byte "é": no chunk holds a whole message or a whole character.
    const emoji = wire.indexOf(0xf0);
    const accent = wire.indexOf(0xc3);
    const cuts = [3, accent + 1, emoji + 2];
    const firstEnd = message("héllo 🌍").length;

    let stream!: ReturnType<typeof streamingResponse>;
    const echo = client(
      fakeFetch(async (call) => {
        stream = streamingResponse(call.signal);
        return stream.response;
      }),
    );

    const iterator = echo.watch({ text: "go" })[Symbol.asyncIterator]();
    const first = iterator.next();
    // The request is issued lazily on the first read.
    await new Promise((r) => setTimeout(r, 0));
    stream.push(wire.slice(0, cuts[0]));
    stream.push(wire.slice(cuts[0], cuts[1]));
    stream.push(wire.slice(cuts[1], cuts[2]));
    stream.push(wire.slice(cuts[2], firstEnd));

    // The first message resolves while the stream is still open and the
    // second message has not been sent: delivery is incremental, not buffered.
    const a = await first;
    expect(a.done).toBe(false);
    expect(a.value).toMatchObject({ text: "héllo 🌍" });

    const pending = iterator.next();
    stream.push(wire.slice(firstEnd, firstEnd + 7));
    stream.push(wire.slice(firstEnd + 7));
    stream.push(endStream());
    stream.close();
    const b = await pending;
    expect(b.value).toMatchObject({ text: "second" });
    expect((await iterator.next()).done).toBe(true);
  });

  it("aborts an in-flight stream with Canceled and releases the response", async () => {
    let stream!: ReturnType<typeof streamingResponse>;
    let seen: AbortSignal | undefined;
    const echo = client(
      fakeFetch(async (call) => {
        seen = call.signal;
        stream = streamingResponse(call.signal);
        return stream.response;
      }),
    );
    const controller = new AbortController();
    const iterator = echo
      .watch({ text: "go" }, { signal: controller.signal })
      [Symbol.asyncIterator]();
    const first = iterator.next();
    await new Promise((r) => setTimeout(r, 0));
    stream.push(message("one"));
    expect((await first).value).toMatchObject({ text: "one" });

    const next = iterator.next();
    controller.abort();

    const error = await next.then(
      () => null,
      (e: unknown) => e,
    );
    expect(seen?.aborted).toBe(true);
    expect(error).not.toBeNull();
    expect(ConnectError.from(error).code).toBe(Code.Canceled);
  });

  it("aborts a pending unary call with Canceled", async () => {
    const echo = client(
      fakeFetch(
        (call) =>
          new Promise((_, reject) => {
            call.signal?.addEventListener("abort", () => reject(call.signal?.reason));
          }),
      ),
    );
    const controller = new AbortController();
    const result = echo.say({ text: "ping" }, { signal: controller.signal }).then(
      () => null,
      (e: unknown) => e,
    );
    await new Promise((r) => setTimeout(r, 0));
    controller.abort();
    expect(ConnectError.from(await result).code).toBe(Code.Canceled);
  });

  it("uses expo/fetch, not the global fetch, for the exported production transport", async () => {
    const globalFetch = jest.spyOn(globalThis, "fetch");
    mockExpoFetch.mockImplementation(async () => jsonResponse({ text: "native" }));
    try {
      const echo = createClient(service, transport) as unknown as Echo;
      await expect(echo.say({ text: "ping" })).resolves.toMatchObject({ text: "native" });
      expect(mockExpoFetch).toHaveBeenCalledTimes(1);
      expect(globalFetch).not.toHaveBeenCalled();
    } finally {
      globalFetch.mockRestore();
    }
  });
});

function errorResponse(status: number, code: string) {
  const body = { code, message: `${code} from the server` };
  return {
    status,
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => body,
    arrayBuffer: async () => new TextEncoder().encode(JSON.stringify(body)).buffer,
    body: null,
  };
}

const unauthenticated = () => errorResponse(401, "unauthenticated");

async function rejection(p: Promise<unknown>): Promise<ConnectError> {
  const e = await p.then(
    () => null,
    (err: unknown) => err,
  );
  expect(e).not.toBeNull();
  return ConnectError.from(e);
}

const tick = () => new Promise((r) => setTimeout(r, 0));

function listenForAuthExpired() {
  initEventBus();
  const seen: number[] = [];
  const off = getEventBus().on("auth:expired", () => seen.push(1));
  return { seen, off };
}

describe("auth:expired", () => {
  it("is emitted when a call ends Unauthenticated, as the browser transports do", async () => {
    const { seen, off } = listenForAuthExpired();
    try {
      const echo = client(fakeFetch(async () => unauthenticated()));
      expect((await rejection(echo.say({ text: "x" }))).code).toBe(Code.Unauthenticated);
      expect(seen).toHaveLength(1);
    } finally {
      off();
    }
  });

  it("is not emitted for other failures", async () => {
    const { seen, off } = listenForAuthExpired();
    try {
      const echo = client(fakeFetch(async () => errorResponse(503, "unavailable")));
      expect((await rejection(echo.say({ text: "x" }))).code).toBe(Code.Unavailable);
      expect(seen).toHaveLength(0);
    } finally {
      off();
    }
  });
});

describe("401 -> renew the token once -> replay once", () => {
  it("replays a unary call with the renewed token, passing the refused token to the handler", async () => {
    let token = "stale";
    setAuthTokenGetter(async () => token);
    const handler = jest.fn(async (rejected: string | null) => {
      expect(rejected).toBe("stale");
      token = "fresh";
      return true;
    });
    setTokenRejectedHandler(handler);
    const auths: (string | null)[] = [];
    const echo = client(
      fakeFetch(async (call) => {
        auths.push(call.headers.get("authorization"));
        return call.headers.get("authorization") === "Bearer fresh"
          ? jsonResponse({ text: "ok" })
          : unauthenticated();
      }),
    );

    await expect(echo.read({ text: "x" })).resolves.toMatchObject({ text: "ok" });
    expect(auths).toEqual(["Bearer stale", "Bearer fresh"]);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("does not replay, and still emits auth:expired, when the handler says no", async () => {
    const { seen, off } = listenForAuthExpired();
    setAuthTokenGetter(async () => "stale");
    setTokenRejectedHandler(async () => false);
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    try {
      expect((await rejection(echo.read({ text: "x" }))).code).toBe(Code.Unauthenticated);
      expect(calls).toBe(1);
      expect(seen).toHaveLength(1);
    } finally {
      off();
    }
  });

  it("replays at most once: a second 401 is final", async () => {
    setAuthTokenGetter(async () => "t");
    const handler = jest.fn(async () => true);
    setTokenRejectedHandler(handler);
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    expect((await rejection(echo.read({ text: "x" }))).code).toBe(Code.Unauthenticated);
    expect(calls).toBe(2);
    expect(handler).toHaveBeenCalledTimes(1);
  });

  it("keeps the original 401 when the handler itself fails", async () => {
    setTokenRejectedHandler(async () => {
      throw new Error("offline");
    });
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    expect((await rejection(echo.read({ text: "x" }))).code).toBe(Code.Unauthenticated);
    expect(calls).toBe(1);
  });

  it("replays an IDEMPOTENT method too", async () => {
    let token = "stale";
    setAuthTokenGetter(async () => token);
    setTokenRejectedHandler(async () => {
      token = "fresh";
      return true;
    });
    const auths: (string | null)[] = [];
    const echo = client(
      fakeFetch(async (call) => {
        auths.push(call.headers.get("authorization"));
        return call.headers.get("authorization") === "Bearer fresh"
          ? jsonResponse({ text: "ok" })
          : unauthenticated();
      }),
    );
    await expect(echo.put({ text: "x" })).resolves.toMatchObject({ text: "ok" });
    expect(auths).toEqual(["Bearer stale", "Bearer fresh"]);
  });

  it("never replays a non-idempotent write: the token is renewed, the 401 stands, and auth:expired fires", async () => {
    // forge's authn answers a bare Unauthenticated with no distinguishing
    // x-forge-error-reason, so a 401 cannot prove the handler never ran.
    const { seen, off } = listenForAuthExpired();
    let token = "stale";
    setAuthTokenGetter(async () => token);
    const handler = jest.fn(async () => {
      token = "fresh";
      return true;
    });
    setTokenRejectedHandler(handler);
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    try {
      expect((await rejection(echo.say({ text: "write" }))).code).toBe(Code.Unauthenticated);
      expect(calls).toBe(1);
      expect(handler).toHaveBeenCalledTimes(1); // renewed for the NEXT call
      expect(seen).toHaveLength(1);
      // ...and the next call carries the renewed token.
      const auths: (string | null)[] = [];
      const next = client(
        fakeFetch(async (call) => {
          auths.push(call.headers.get("authorization"));
          return jsonResponse({ text: "ok" });
        }),
      );
      await next.say({ text: "again" });
      expect(auths).toEqual(["Bearer fresh"]);
    } finally {
      off();
    }
  });

  it("never replays when no handler is wired (the scaffold default)", async () => {
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    expect((await rejection(echo.read({ text: "x" }))).code).toBe(Code.Unauthenticated);
    expect(calls).toBe(1);
  });

  it("never replays a server stream", async () => {
    const handler = jest.fn(async () => true);
    setTokenRejectedHandler(handler);
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    const iterator = echo.watch({ text: "go" })[Symbol.asyncIterator]();
    await rejection(iterator.next());
    expect(calls).toBe(1);
    expect(handler).not.toHaveBeenCalled();
  });

  it("does not replay when the identity changed while it was renewing, and does not emit", async () => {
    const { seen, off } = listenForAuthExpired();
    setTokenRejectedHandler(async () => {
      session.bumpEpoch(); // the user signed out while the renewal ran
      return true;
    });
    let calls = 0;
    const echo = client(
      fakeFetch(async () => {
        calls++;
        return unauthenticated();
      }),
    );
    try {
      expect((await rejection(echo.read({ text: "x" }))).code).toBe(Code.Canceled);
      expect(calls).toBe(1);
      expect(seen).toHaveLength(0);
    } finally {
      off();
    }
  });
});

describe("identity epoch", () => {
  it("aborts an in-flight unary call when the epoch bumps", async () => {
    let seen: AbortSignal | undefined;
    const echo = client(
      fakeFetch(
        (call) =>
          new Promise((_, reject) => {
            seen = call.signal;
            call.signal?.addEventListener("abort", () => reject(call.signal?.reason));
          }),
      ),
    );
    const result = rejection(echo.say({ text: "write" }));
    await tick();
    session.bumpEpoch();
    expect((await result).code).toBe(Code.Canceled);
    expect(seen?.aborted).toBe(true);
  });

  it("drops a response that lands after the epoch moved, even from a fetch that ignores abort", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    const echo = client(
      fakeFetch(async () => {
        await gate; // does not look at the signal
        return jsonResponse({ text: "previous account's data" });
      }),
    );
    const result = rejection(echo.say({ text: "x" }));
    await tick();
    const before = currentEpoch();
    session.bumpEpoch();
    expect(currentEpoch()).toBe(before + 1);
    release();
    expect((await result).code).toBe(Code.Canceled);
  });

  it("lets a request started under the new epoch through", async () => {
    session.bumpEpoch();
    const echo = client(fakeFetch(async () => jsonResponse({ text: "mine" })));
    await expect(echo.say({ text: "x" })).resolves.toMatchObject({ text: "mine" });
  });

  it("stops delivering stream messages once the epoch moves", async () => {
    let stream!: ReturnType<typeof streamingResponse>;
    const echo = client(
      fakeFetch(async () => {
        stream = streamingResponse(undefined); // ignores abort: the guard must still hold
        return stream.response;
      }),
    );
    const iterator = echo.watch({ text: "go" })[Symbol.asyncIterator]();
    const first = iterator.next();
    await tick();
    stream.push(message("one"));
    expect((await first).value).toMatchObject({ text: "one" });

    session.bumpEpoch();
    const next = iterator.next();
    stream.push(message("previous account's second message"));
    expect((await rejection(next)).code).toBe(Code.Canceled);
  });

  it("still honours the caller's own signal, and a caller abort is not an identity change", async () => {
    const echo = client(
      fakeFetch(
        (call) =>
          new Promise((_, reject) => {
            call.signal?.addEventListener("abort", () => reject(call.signal?.reason));
          }),
      ),
    );
    const controller = new AbortController();
    const result = rejection(echo.say({ text: "x" }, { signal: controller.signal }));
    await tick();
    const epoch = currentEpoch();
    controller.abort();
    expect((await result).code).toBe(Code.Canceled);
    expect(currentEpoch()).toBe(epoch);
  });
});
