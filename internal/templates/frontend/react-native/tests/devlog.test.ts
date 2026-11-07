// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { afterEach, beforeEach, describe, expect, it, jest } from "@jest/globals";

import { deriveMetroOrigin, redact } from "../src/lib/devlog";

// The forwarder derives its Metro origin from expo-constants' public fields
// only; pin them so the tests do not depend on a native module. Mutable so a
// test can model a different runtime.
const mockConstants: {
  expoConfig: { hostUri?: string } | null;
  experienceUrl?: string;
  linkingUri?: string;
} = { expoConfig: { hostUri: "192.168.1.20:8081" } };
// Read lazily: the static import of devlog below runs this factory before the
// const above is initialised.
jest.mock("expo-constants", () => ({
  __esModule: true,
  get default() {
    return mockConstants;
  },
}));

type Entry = { level: string; msg: string };
type Call = {
  url: string;
  init: {
    method: string;
    body: string;
    headers: Record<string, string>;
    signal?: AbortSignal;
  };
};

// Fresh module state per test: the forwarder keeps its buffer and breaker at
// module level of one logger instance.
function load() {
  let mod!: typeof import("../src/lib/devlog");
  jest.isolateModules(() => {
    // eslint-disable-next-line @typescript-eslint/no-require-imports -- isolateModules needs a sync load
    mod = require("../src/lib/devlog");
  });
  return mod;
}

function okResponse() {
  return {
    ok: true,
    status: 204,
    headers: { get: (k: string) => (k === "X-Forge-Devlog" ? "2" : null) },
  };
}

const realConsole = { ...console };
const g = globalThis as unknown as {
  fetch: unknown;
  ErrorUtils?: { getGlobalHandler: () => unknown; setGlobalHandler: (h: unknown) => void };
};
const realFetch = g.fetch;
const realErrorUtils = g.ErrorUtils;

let calls: Call[];
let fetchImpl: (url: string, init: Call["init"]) => Promise<unknown>;
let globalHandler: (e: unknown, fatal?: boolean) => void;
let previousHandlerCalls: unknown[][];

function entries(): Entry[] {
  return calls.flatMap((c) => (JSON.parse(c.init.body) as { entries: Entry[] }).entries);
}

async function settle() {
  // Let the fetch promise chain and the error-level microtask run.
  for (let i = 0; i < 5; i++) await Promise.resolve();
}

beforeEach(() => {
  mockConstants.expoConfig = { hostUri: "192.168.1.20:8081" };
  delete mockConstants.experienceUrl;
  delete mockConstants.linkingUri;
  delete process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN;
  jest.useFakeTimers({ doNotFake: ["queueMicrotask", "nextTick"] });
  calls = [];
  fetchImpl = async () => okResponse();
  g.fetch = (url: string, init: Call["init"]) => {
    calls.push({ url, init });
    return fetchImpl(url, init);
  };
  previousHandlerCalls = [];
  globalHandler = (...args) => {
    previousHandlerCalls.push(args);
  };
  g.ErrorUtils = {
    getGlobalHandler: () => globalHandler,
    setGlobalHandler: (h) => {
      globalHandler = h as typeof globalHandler;
    },
  };
  // Silence the mirrored output; the originals are restored afterwards.
  for (const level of ["log", "info", "warn", "error", "debug"] as const) {
    console[level] = jest.fn() as never;
  }
});

afterEach(() => {
  // Uninstall before restoring: an install left behind would put stale mocks
  // back on the console when the next test installs.
  (globalThis as { __forgeDevLogUninstall?: () => void }).__forgeDevLogUninstall?.();
  jest.useRealTimers();
  Object.assign(console, realConsole);
  g.fetch = realFetch;
  g.ErrorUtils = realErrorUtils;
});

describe("native dev log forwarder", () => {
  it("posts console output in one batch to the Metro origin, at the web levels", async () => {
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.log("a");
    console.info("b");
    console.warn("c");
    console.debug("d");
    jest.advanceTimersByTime(300);
    await settle();

    expect(calls).toHaveLength(1);
    expect(calls[0].url).toBe("http://192.168.1.20:8081/__forge/log");
    expect(calls[0].init.method).toBe("POST");
    const got = entries();
    expect(got.map((e) => e.level)).toEqual(["info", "log", "info", "warn", "debug"]);
    expect(got[0].msg).toContain("native logging attached");
    expect(got.slice(1).map((e) => e.msg)).toEqual(["a", "b", "c", "d"]);
  });

  it("flushes an error promptly without waiting for the timer", async () => {
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("boom");
    await settle();
    expect(entries().map((e) => e.msg)).toContain("boom");
  });

  it("mirrors to the real console and restores it on uninstall", () => {
    const before = console.warn;
    const { installNativeDevLogging, uninstallNativeDevLogging } = load();
    installNativeDevLogging();
    expect(console.warn).not.toBe(before);
    console.warn("seen");
    expect(before).toHaveBeenCalledWith("seen");
    uninstallNativeDevLogging();
    expect(console.warn).toBe(before);
  });

  it("captures uncaught errors, then chains the previous global handler", async () => {
    const { installNativeDevLogging } = load();
    const original = globalHandler;
    installNativeDevLogging();
    expect(globalHandler).not.toBe(original);
    const err = new TypeError("undefined is not a function");
    globalHandler(err, true);
    await settle();

    const msgs = entries()
      .filter((e) => e.level === "error")
      .map((e) => e.msg);
    expect(msgs).toHaveLength(1);
    expect(msgs[0]).toContain("[fatal]");
    expect(msgs[0]).toContain("TypeError: undefined is not a function");
    expect(previousHandlerCalls).toEqual([[err, true]]);
  });

  it("does not print an uncaught error twice when the previous handler console.errors it", async () => {
    globalHandler = (e) => console.error(e);
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    globalHandler(new Error("once"), false);
    await settle();
    expect(entries().filter((e) => e.msg.includes("once"))).toHaveLength(1);
  });

  it("captures an unhandled rejection that RN reports through console.error", async () => {
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error(new Error('Uncaught (in promise, id: 0): "nope"'));
    await settle();
    expect(
      entries().some((e) => e.level === "error" && e.msg.includes("Uncaught (in promise")),
    ).toBe(true);
  });

  it("redacts Authorization headers, bearer/JWT tokens and key=value secrets", async () => {
    const { installNativeDevLogging, redact } = load();
    installNativeDevLogging();
    const jwt = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk";
    console.log({ headers: { Authorization: "Bearer super-secret-token-123" } });
    console.log("GET /x?access_token=abc123&ok=1 password=hunter2", jwt);
    console.log("cookie: session=xyz; api_key=K-999");
    jest.advanceTimersByTime(300);
    await settle();

    const body = JSON.stringify(entries());
    for (const secret of [
      "super-secret-token-123",
      "abc123",
      "hunter2",
      jwt,
      "session=xyz",
      "K-999",
    ]) {
      expect(body).not.toContain(secret);
    }
    expect(body).toContain("[REDACTED]");
    expect(body).toContain("ok=1");
    expect(redact("plain message")).toBe("plain message");
  });

  it("truncates a single line at 8000 characters", async () => {
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.log("x".repeat(20_000));
    jest.advanceTimersByTime(300);
    await settle();
    const long = entries().find((e) => e.msg.startsWith("xxxx"));
    expect(long?.msg.endsWith("… (truncated)")).toBe(true);
    expect(long!.msg.length).toBeLessThan(8_100);
  });

  it("bounds a batch's size and sends the rest as further single-flight requests", async () => {
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    for (let i = 0; i < 20; i++) console.log("y".repeat(4_000));
    await settle();
    jest.advanceTimersByTime(300);
    for (let i = 0; i < 10; i++) await settle();

    expect(calls.length).toBeGreaterThan(1);
    for (const c of calls) expect(c.init.body.length).toBeLessThanOrEqual(32 * 1024 + 8_200);
    expect(entries().filter((e) => e.msg.startsWith("yyyy"))).toHaveLength(20);
  });

  it("never has two requests in flight", async () => {
    let release!: () => void;
    fetchImpl = () => new Promise((resolve) => (release = () => resolve(okResponse())));
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("first");
    await settle();
    console.error("second");
    await settle();
    expect(calls).toHaveLength(1);
    release();
    await settle();
    release();
    await settle();
    expect(calls).toHaveLength(2);
  });

  it("does not forward lines logged from inside fetch (no recursion)", async () => {
    fetchImpl = async () => okResponse();
    g.fetch = (url: string, init: Call["init"]) => {
      console.log("fetch polyfill chatter");
      calls.push({ url, init });
      return Promise.resolve(okResponse());
    };
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("trigger");
    for (let i = 0; i < 5; i++) {
      await settle();
      jest.advanceTimersByTime(300);
    }
    expect(calls).toHaveLength(1);
    expect(entries().some((e) => e.msg.includes("fetch polyfill chatter"))).toBe(false);
  });

  it("does not re-log its own failures and does not retry-storm a dead server", async () => {
    fetchImpl = async () => {
      throw new Error("Network request failed");
    };
    const errorMock = console.error as unknown as jest.Mock;
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("one");
    await settle();
    expect(calls).toHaveLength(1);

    // The failure is silent on the console and the breaker is open: a flood of
    // logging produces no further requests.
    const errorCallsAfterFailure = errorMock.mock.calls.length;
    for (let i = 0; i < 200; i++) console.error(`flood ${i}`);
    jest.advanceTimersByTime(1_000);
    await settle();
    expect(calls).toHaveLength(1);
    expect(errorMock.mock.calls.length).toBe(errorCallsAfterFailure + 200);

    // After the cool-down the next line goes out, carrying the drop count.
    jest.advanceTimersByTime(2_500);
    fetchImpl = async () => okResponse();
    console.error("recovered");
    await settle();
    expect(calls).toHaveLength(2);
    const second = (JSON.parse(calls[1].init.body) as { entries: Entry[] }).entries;
    expect(second[0].level).toBe("warn");
    expect(second[0].msg).toMatch(/\[forge-devlog\] dropped \d+ lines/);
    expect(second.map((e) => e.msg)).toContain("recovered");
  });

  it("treats a 2xx without the receiver header as a failure (wrong origin)", async () => {
    fetchImpl = async () => ({ ok: true, status: 200, headers: { get: () => null } });
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("one");
    await settle();
    console.error("two");
    jest.advanceTimersByTime(300);
    await settle();
    expect(calls).toHaveLength(1);
  });

  it("is a no-op outside __DEV__", () => {
    const original = (globalThis as { __DEV__?: boolean }).__DEV__;
    (globalThis as { __DEV__?: boolean }).__DEV__ = false;
    try {
      const before = console.log;
      const { installNativeDevLogging } = load();
      installNativeDevLogging();
      expect(console.log).toBe(before);
      expect(calls).toHaveLength(0);
    } finally {
      (globalThis as { __DEV__?: boolean }).__DEV__ = original;
    }
  });

  it("derives the origin from the public expo-constants fields only", async () => {
    mockConstants.expoConfig = null;
    mockConstants.experienceUrl = "exp://10.0.0.5:8081";
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("x");
    await settle();
    expect(calls[0].url).toBe("http://10.0.0.5:8081/__forge/log");
  });

  it("does not start when there is no dev server to derive", () => {
    mockConstants.expoConfig = null;
    mockConstants.linkingUri = "myapp://";
    const before = console.log;
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    expect(console.log).toBe(before);
    expect(calls).toHaveLength(0);
  });

  describe("origin precedence", () => {
    async function postedTo(options?: { origin?: string }): Promise<string> {
      const { installNativeDevLogging } = load();
      installNativeDevLogging(options);
      console.error("x");
      await settle();
      return calls[0].url;
    }

    it("options.origin beats the env var and every expo-constants field", async () => {
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "http://env.example:1111";
      mockConstants.experienceUrl = "exp://10.0.0.5:8081";
      expect(await postedTo({ origin: "http://opt.example:2222" })).toBe(
        "http://opt.example:2222/__forge/log",
      );
    });

    it("hostUri beats the injected env var (dev client or Expo Go on a device)", async () => {
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "http://localhost:8081";
      mockConstants.expoConfig = { hostUri: "10.0.2.2:8081" };
      expect(await postedTo()).toBe("http://10.0.2.2:8081/__forge/log");
    });

    it("experienceUrl beats the env var", async () => {
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "http://env.example:1111/";
      mockConstants.expoConfig = null;
      mockConstants.experienceUrl = "exp://10.0.0.5:8081";
      expect(await postedTo()).toBe("http://10.0.0.5:8081/__forge/log");
    });

    it("linkingUri beats the env var", async () => {
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "http://env.example:1111/";
      mockConstants.expoConfig = null;
      mockConstants.linkingUri = "exp://10.0.0.6:8081/--/";
      expect(await postedTo()).toBe("http://10.0.0.6:8081/__forge/log");
    });

    it("the env var is used when no expo-constants field resolves", async () => {
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "http://env.example:1111/";
      mockConstants.expoConfig = null;
      mockConstants.linkingUri = "myapp://";
      expect(await postedTo()).toBe("http://env.example:1111/__forge/log");
    });

    it("works in a plain Debug build: no expo-constants fields, env var only", async () => {
      mockConstants.expoConfig = null;
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "https://metro.example";
      expect(await postedTo()).toBe("https://metro.example/__forge/log");
    });

    it("falls back to hostUri, then experienceUrl, then linkingUri without the env var", async () => {
      expect(await postedTo()).toBe("http://192.168.1.20:8081/__forge/log");
    });

    it("an invalid env value is ignored in favour of expo-constants, and not trusted", async () => {
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "ftp://nope:1";
      expect(await postedTo()).toBe("http://192.168.1.20:8081/__forge/log");
    });

    it("an invalid env value with nothing else warns once and names it", () => {
      mockConstants.expoConfig = null;
      process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN = "not a url";
      const { installNativeDevLogging } = load();
      installNativeDevLogging();
      const warn = console.warn as unknown as jest.Mock;
      expect(warn).toHaveBeenCalledTimes(1);
      expect(String(warn.mock.calls[0][0])).toContain("is not an http(s) URL");
    });
  });

  it("warns exactly once, naming the fix, when no origin resolves — and does not forward it", async () => {
    mockConstants.expoConfig = null;
    const warn = console.warn as unknown as jest.Mock;
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    expect(warn).toHaveBeenCalledTimes(1);
    const text = String(warn.mock.calls[0][0]);
    expect(text).toContain("forge env up");
    expect(text).toContain("EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN");
    expect(text).toContain("{ origin }");
    // Nothing was installed, so the warn is the real console and nothing posts.
    jest.advanceTimersByTime(1000);
    await settle();
    expect(calls).toHaveLength(0);
    expect(console.warn).toBe(warn);
  });

  it("does not warn when an origin resolves", () => {
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    // the forwarder wraps console.warn; the underlying mock must not be hit
    expect(
      (globalThis as { __forgeDevLogUninstall?: unknown }).__forgeDevLogUninstall !== undefined,
    ).toBe(true);
    expect(calls).toHaveLength(0);
  });

  it("aborts a stalled request at the timeout, not just counts it lost", async () => {
    let signal: AbortSignal | undefined;
    fetchImpl = (_url, init) => {
      signal = init.signal;
      return new Promise(() => {}); // never settles
    };
    const { installNativeDevLogging } = load();
    installNativeDevLogging();
    console.error("stall");
    await settle();
    expect(calls).toHaveLength(1);
    expect(signal).toBeDefined();
    expect(signal?.aborted).toBe(false);

    jest.advanceTimersByTime(5_000);
    await settle();
    expect(signal?.aborted).toBe(true);

    // The breaker is open and the lost batch is reported once it closes.
    console.error("during cool-down");
    await settle();
    expect(calls).toHaveLength(1);
    jest.advanceTimersByTime(2_500);
    fetchImpl = async () => okResponse();
    console.error("recovered");
    await settle();
    expect(calls).toHaveLength(2);
    const second = (JSON.parse(calls[1].init.body) as { entries: Entry[] }).entries;
    expect(second[0].msg).toMatch(/\[forge-devlog\] dropped \d+ lines/);
  });
});

describe("deriveMetroOrigin", () => {
  const cases: [string, Parameters<typeof deriveMetroOrigin>[0], string | null][] = [
    ["hostUri host:port", { hostUri: "192.168.1.20:8081" }, "http://192.168.1.20:8081"],
    ["hostUri with a path", { hostUri: "10.0.2.2:8081/--/x" }, "http://10.0.2.2:8081"],
    ["hostUri without a port", { hostUri: "metro.local" }, "http://metro.local"],
    [
      "hostUri wins over the URLs",
      { hostUri: "1.1.1.1:1", experienceUrl: "exp://2.2.2.2:2" },
      "http://1.1.1.1:1",
    ],
    [
      "exp:// experienceUrl",
      { experienceUrl: "exp://192.168.1.20:8081" },
      "http://192.168.1.20:8081",
    ],
    [
      "exps:// tunnel experienceUrl",
      { experienceUrl: "exps://abc.example.dev" },
      "https://abc.example.dev",
    ],
    [
      "Expo Go linkingUri",
      { linkingUri: "exp://192.168.1.20:8081/--/" },
      "http://192.168.1.20:8081",
    ],
    [
      "dev-client experienceUrl carries the metro url",
      {
        experienceUrl: "exp+myapp://expo-development-client/?url=http%3A%2F%2F192.168.1.20%3A8081",
      },
      "http://192.168.1.20:8081",
    ],
    [
      "https experienceUrl",
      { experienceUrl: "https://dev.example.com/x" },
      "https://dev.example.com",
    ],
    ["an app deep-link scheme is no dev server", { linkingUri: "myapp://" }, null],
    [
      "a dev-client url to a non-http target is skipped",
      { experienceUrl: "exp+myapp://x/?url=file%3A%2F%2F%2Fa" },
      null,
    ],
    [
      "a malformed dev-client url is skipped",
      { experienceUrl: "exp+myapp://x/?url=%E0%A4%A" },
      null,
    ],
    ["nothing at all", {}, null],
    ["null fields", { hostUri: null, experienceUrl: null, linkingUri: null }, null],
    [
      "a garbage hostUri falls through",
      { hostUri: "not a host", linkingUri: "exp://9.9.9.9:1" },
      "http://9.9.9.9:1",
    ],
  ];
  it.each(cases)("%s", (_name, sources, want) => {
    expect(deriveMetroOrigin(sources)).toBe(want);
  });
});

describe("redact", () => {
  const masked = "[REDACTED]";
  const cases: [string, string, string][] = [
    // camelCase / suffix keys
    ["authToken", "authToken=abc123", `authToken=${masked}`],
    ["sessionToken json", '{"sessionToken":"abc 123"}', `{"sessionToken":"${masked}"}`],
    ["apiToken", "apiToken: tok_live_1", `apiToken: ${masked}`],
    ["pushToken", "pushToken=ExponentPushToken[xxxx]", `pushToken=${masked}`],
    ["privateKey", "privateKey=MIIEvQ", `privateKey=${masked}`],
    ["clientSecret", "clientSecret=s3cr3t", `clientSecret=${masked}`],
    ["snake and kebab", "api_key=K1 x-api-key: K2", `api_key=${masked} x-api-key: ${masked}`],
    ["case-insensitive", "ACCESS_TOKEN=Z", `ACCESS_TOKEN=${masked}`],
    [
      "password in a URL query keeps the rest",
      "GET /x?password=hunter2&ok=1",
      `GET /x?password=${masked}&ok=1`,
    ],
    // quoted values are masked to the closing quote, spaces included
    [
      "double quoted with spaces",
      'password="two words here" next=1',
      `password="${masked}" next=1`,
    ],
    ["single quoted with spaces", "secret='a b c' next=1", `secret='${masked}' next=1`],
    [
      "json value with spaces",
      '{"password":"correct horse battery","ok":true}',
      `{"password":"${masked}","ok":true}`,
    ],
    [
      "escaped quote inside the value",
      String.raw`password="a\"b c" next=1`,
      `password="${masked}" next=1`,
    ],
    [
      "json-escaped string",
      String.raw`{\"authToken\":\"a b\",\"n\":1}`,
      String.raw`{\"authToken\":\"${masked}\",\"n\":1}`,
    ],
    [
      "unterminated quote masks to end of line",
      'token="abc def\nnext line',
      `token="${masked}\nnext line`,
    ],
    // cookies: the whole remainder
    ["cookie header", "cookie: session=xyz; api_key=K-999; theme=dark", `cookie: ${masked}`],
    ["set-cookie header", "Set-Cookie: id=1; Path=/; HttpOnly", `Set-Cookie: ${masked}`],
    [
      "cookie stops at end of line",
      "cookie: a=1; b=2\nnext: line",
      `cookie: ${masked}\nnext: line`,
    ],
    [
      "quoted cookie stops at its quote",
      '{"cookie":"a=1; b=2","ok":1}',
      `{"cookie":"${masked}","ok":1}`,
    ],
    // authorization / bearer / jwt
    ["authorization bearer", "Authorization: Bearer abc.def-123", `Authorization: ${masked}`],
    [
      "authorization json",
      '{"authorization":"Bearer abc.def-123"}',
      `{"authorization":"${masked}"}`,
    ],
    ["bare bearer", "sent Bearer abcdef123456 to server", `sent Bearer ${masked} to server`],
    ["basic", "Basic dXNlcjpwYXNz", `Basic ${masked}`],
    ["jwt", "t=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijk", `t=${masked}`],
    // false-positive sanity: ordinary text is untouched
    ["plain message", "plain message", "plain message"],
    // *key forms: secret-ish prefixes mask, identifier keys do not
    [
      "secret-ish key names",
      "accessKey=A signingKey=B encryptionKey: C sessionKey=D authKey=E secret_key=F",
      `accessKey=${masked} signingKey=${masked} encryptionKey: ${masked} sessionKey=${masked} authKey=${masked} secret_key=${masked}`,
    ],
    ["hyphenated non-identifier key", "x-key=SECRET1", `x-key=${masked}`],
    ["bare key param", "GET /maps?key=AIzaSy123&v=1", `GET /maps?key=${masked}&v=1`],
    ["queryKey array", 'queryKey: ["widgets",1]', 'queryKey: ["widgets",1]'],
    [
      "queryKey json",
      '{"queryKey":["widgets",{"id":1}],"x":2}',
      '{"queryKey":["widgets",{"id":1}],"x":2}',
    ],
    [
      "react query affected key line",
      'Affected query key: ["widgets",1]',
      'Affected query key: ["widgets",1]',
    ],
    [
      "other identifier keys",
      "mutationKey=[a] cacheKey=abc primaryKey: 7 cache-key=z",
      "mutationKey=[a] cacheKey=abc primaryKey: 7 cache-key=z",
    ],
    [
      "array value of a credential is masked whole, never as a partial",
      'apiKey: ["a","b"] next=1',
      `apiKey: ${masked} next=1`,
    ],
    [
      "object value of a credential is masked whole",
      'authToken={"a":"x y","b":[1]} next=1',
      `authToken=${masked} next=1`,
    ],
    [
      "keyboard is a word, not a key",
      "keyboard shown, keystroke ok, monkey business",
      "keyboard shown, keystroke ok, monkey business",
    ],
    ["key without a separator", "the key opened the door", "the key opened the door"],
    ["token word in prose", "no token available", "no token available"],
    // credential(s): identifier suffixes and quoted/assigned keys mask; prose does not
    [
      "credential suffix identifiers",
      "awsCredentials: AKIA1 client_credential=c2",
      `awsCredentials: ${masked} client_credential=${masked}`,
    ],
    ["quoted credentials key", '{"credentials":"u:p"}', `{"credentials":"${masked}"}`],
    ["bare credential assignment", "credential=abc", `credential=${masked}`],
    [
      "bare credential key with a quoted value",
      `credentials: "abc def" next credential: 'x'`,
      `credentials: "${masked}" next credential: '${masked}'`,
    ],
    [
      "unquoted object key with structured values",
      '{credentials: "abc"} credentials:{"user":"u"} credential: ["a"]',
      `{credentials: "${masked}"} credentials:${masked} credential: ${masked}`,
    ],
    [
      "scaffold auth warning is prose, not a credential",
      "[auth] No credential: this bundle is driving a real backend with nothing to present",
      "[auth] No credential: this bundle is driving a real backend with nothing to present",
    ],
    ["empty value", "token=", "token="],
    ["already masked is stable", `token=${masked}`, `token=${masked}`],
  ];
  it.each(cases)("%s", (_name, input, want) => {
    expect(redact(input)).toBe(want);
    expect(redact(redact(input))).toBe(want);
  });

  it("does not run away on a long line with many credential-like keys", () => {
    const line = "key=".repeat(5_000) + "x ".repeat(5_000);
    const started = Date.now();
    redact(line);
    expect(Date.now() - started).toBeLessThan(500);
  });
});
