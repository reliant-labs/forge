// This package carries no jsdom by design (see service-hooks.test.ts), so the
// two DOM surfaces devlog touches — window.addEventListener and the event
// objects — are stubbed here rather than pulling in a renderer. They are small
// and stable enough that a stub tests the same thing a DOM would.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  DEV_LOG_ENDPOINT,
  devLoggingInstalled,
  installDevLogging,
  uninstallDevLogging,
} from "./devlog.js";

interface Posted {
  level: string;
  msg: string;
}

type Listener = (event: unknown) => void;

/** Minimal window stand-in that records listeners so tests can fire them. */
function stubWindow(): { fire: (type: string, event: unknown) => void } {
  const listeners = new Map<string, Set<Listener>>();
  vi.stubGlobal("window", {
    addEventListener(type: string, fn: Listener) {
      const set = listeners.get(type) ?? new Set<Listener>();
      set.add(fn);
      listeners.set(type, set);
    },
    removeEventListener(type: string, fn: Listener) {
      listeners.get(type)?.delete(fn);
    },
  });
  return {
    fire(type, event) {
      for (const fn of listeners.get(type) ?? []) fn(event);
    },
  };
}

/** Capture what would have been POSTed to the dev server. */
function stubFetch(): { posted: Posted[]; calls: () => number } {
  const posted: Posted[] = [];
  const fetchMock = vi.fn((_url: string, init?: RequestInit) => {
    posted.push(JSON.parse(String(init?.body)) as Posted);
    return Promise.resolve(new Response(null, { status: 204 }));
  });
  vi.stubGlobal("fetch", fetchMock);
  return { posted, calls: () => fetchMock.mock.calls.length };
}

describe("dev log forwarding", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    stubWindow();
  });

  afterEach(() => {
    uninstallDevLogging();
    vi.unstubAllGlobals();
  });

  it("does nothing in production", () => {
    const { calls } = stubFetch();
    installDevLogging({ dev: false });

    expect(devLoggingInstalled()).toBe(false);
    console.log("this must not ship anywhere");
    expect(calls()).toBe(0);
  });

  // Regression: a bundler constant-folds `dev: import.meta.env.DEV` to false
  // and can then drop the property (or the whole argument) as dead weight,
  // leaving `installDevLogging({})` live in a production bundle. Observed in a
  // real `vite build` of the scaffold. `dev` must be fail-closed, not
  // defaulted, or that call silently installs the override in production.
  it("stays fail-closed when the bundler strips the dev flag", () => {
    const { calls } = stubFetch();

    installDevLogging({} as never);
    expect(devLoggingInstalled()).toBe(false);

    installDevLogging({ dev: undefined } as never);
    expect(devLoggingInstalled()).toBe(false);

    console.log("must not be forwarded");
    expect(calls()).toBe(0);
  });

  it("forwards console output to the dev endpoint", () => {
    const { posted } = stubFetch();
    installDevLogging({ dev: true, mirrorToConsole: false });

    console.log("hello");
    console.warn("careful", { userId: 42 });

    expect(posted).toEqual([
      { level: "log", msg: "hello" },
      { level: "warn", msg: 'careful {"userId":42}' },
    ]);
  });

  it("keeps Error stacks intact", () => {
    const { posted } = stubFetch();
    installDevLogging({ dev: true, mirrorToConsole: false });

    console.error(new Error("boom"));

    expect(posted[0]?.level).toBe("error");
    expect(posted[0]?.msg).toContain("Error: boom");
    // The stack is the reason this feature exists; assert it survived.
    expect(posted[0]?.msg).toContain("devlog.test");
  });

  it("still writes to the real console by default", () => {
    stubFetch();
    const spy = vi.spyOn(console, "log").mockImplementation(() => {});
    installDevLogging({ dev: true });

    console.log("visible in devtools");

    expect(spy).toHaveBeenCalledWith("visible in devtools");
  });

  it("does not recurse when fetch itself logs", () => {
    const posted: Posted[] = [];
    // A fetch implementation that logs — the recursion trap this guards.
    vi.stubGlobal(
      "fetch",
      vi.fn((_url: string, init?: RequestInit) => {
        posted.push(JSON.parse(String(init?.body)) as Posted);
        console.log("fetch internals talking");
        return Promise.resolve(new Response(null, { status: 204 }));
      }),
    );
    installDevLogging({ dev: true, mirrorToConsole: false });

    console.log("one line");

    // Exactly one POST: the re-entrant log is dropped, not looped.
    expect(posted).toHaveLength(1);
    expect(posted[0]?.msg).toBe("one line");
  });

  it("survives values JSON cannot serialize", () => {
    const { posted } = stubFetch();
    installDevLogging({ dev: true, mirrorToConsole: false });

    const circular: Record<string, unknown> = {};
    circular.self = circular;
    expect(() => console.log(circular)).not.toThrow();

    expect(posted).toHaveLength(1);
  });

  it("forwards uncaught errors that produced no console call", () => {
    const { posted } = stubFetch();
    const win = stubWindow();
    installDevLogging({ dev: true, mirrorToConsole: false });

    win.fire("error", { error: new Error("UNCAUGHT") });

    expect(posted).toHaveLength(1);
    expect(posted[0]?.level).toBe("error");
    expect(posted[0]?.msg).toContain("UNCAUGHT");
  });

  it("forwards unhandled promise rejections", () => {
    const { posted } = stubFetch();
    const win = stubWindow();
    installDevLogging({ dev: true, mirrorToConsole: false });

    win.fire("unhandledrejection", { reason: new Error("REJECTED") });

    expect(posted).toHaveLength(1);
    expect(posted[0]?.msg).toContain("unhandled rejection:");
    expect(posted[0]?.msg).toContain("REJECTED");
  });

  it("posts to the forge endpoint convention", () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(new Response(null, { status: 204 })),
    );
    vi.stubGlobal("fetch", fetchMock);
    installDevLogging({ dev: true, mirrorToConsole: false });

    console.log("x");

    expect(fetchMock).toHaveBeenCalledWith(DEV_LOG_ENDPOINT, expect.anything());
  });

  it("restores the original console on uninstall", () => {
    const { calls } = stubFetch();
    const before = console.log;
    installDevLogging({ dev: true, mirrorToConsole: false });
    expect(console.log).not.toBe(before);

    uninstallDevLogging();

    expect(console.log).toBe(before);
    expect(devLoggingInstalled()).toBe(false);
    const posts = calls();
    console.log("after uninstall");
    expect(calls()).toBe(posts);
  });
});
