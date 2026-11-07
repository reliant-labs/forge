// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { afterEach, describe, expect, it, jest } from "@jest/globals";
import { createFileRegistry, fromJson, type DescService } from "@bufbuild/protobuf";
import { FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createTransport, setAuthTokenGetter, transport } from "../src/lib/connect";

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
        { name: "Say", inputType: ".test.v1.EchoRequest", outputType: ".test.v1.EchoReply" },
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
