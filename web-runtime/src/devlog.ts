// Dev-only browser log forwarding.
//
// A browser's console is a dead end: an agent debugging a scaffolded frontend
// cannot open devtools, so every console line and — worse — every uncaught
// error is invisible to it. The backend has no such gap (structured slog, the
// observe ComponentChain, and forge's dev loop already tees each process's
// stdout to .forge/logs/<env>/). This closes the frontend half by POSTing
// browser console output to a dev-server endpoint that prints it, so it lands
// in .forge/logs/<env>/frontend_<name>.log next to the server logs.
//
// DEV ONLY, and structurally so. The receiving endpoint is a Vite plugin
// gated `apply: "serve"` / a Next.js route behind a NODE_ENV check, so it does
// not exist in a production build; installDevLogging() additionally no-ops
// unless it is handed dev: true. A production bundle therefore has no
// endpoint to post to AND no code that would post.
//
// It is also reversible: uninstallDevLogging() restores the original console
// methods, and the scaffolded call site is an ordinary line in main.tsx /
// providers.tsx that a project can delete outright.

/** Console methods this module mirrors. */
const LEVELS = ["log", "info", "warn", "error", "debug"] as const;

type Level = (typeof LEVELS)[number];

/** The wire shape the dev-server endpoint expects. */
export interface DevLogPayload {
  level: Level | "error";
  msg: string;
}

export interface DevLoggingOptions {
  /**
   * Whether to install at all. Pass `import.meta.env.DEV` (Vite) or
   * `process.env.NODE_ENV !== "production"` (Next.js). When false this is a
   * no-op, so the call site needs no conditional of its own.
   */
  dev: boolean;
  /** Endpoint the dev server serves. Defaults to the forge convention. */
  endpoint?: string;
  /**
   * Also keep writing to the real console. Default true — the browser devtools
   * experience should be unchanged; this feature ADDS a sink, it does not move
   * one.
   */
  mirrorToConsole?: boolean;
}

export const DEV_LOG_ENDPOINT = "/__forge/log";

const original: Partial<Record<Level, (...args: unknown[]) => void>> = {};
let installed = false;

// Guards against infinite recursion. The forwarder calls fetch(), and anything
// that logs inside fetch (an interceptor, a polyfill, a devtools extension)
// would otherwise re-enter the override forever.
let forwarding = false;

/** Render one console argument as a string, keeping Error stacks intact. */
function render(arg: unknown): string {
  if (arg instanceof Error) {
    return `${arg.name}: ${arg.message}${arg.stack ? `\n${arg.stack}` : ""}`;
  }
  if (typeof arg === "string") return arg;
  try {
    return JSON.stringify(arg);
  } catch {
    // Circular structures, BigInt, DOM nodes — never let logging throw.
    return String(arg);
  }
}

function forward(
  endpoint: string,
  level: DevLogPayload["level"],
  args: unknown[],
): void {
  if (forwarding) return;
  forwarding = true;
  try {
    const body: DevLogPayload = { level, msg: args.map(render).join(" ") };
    void fetch(endpoint, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      // A log emitted during unload (the interesting case for a crash on
      // navigation) is cancelled without this.
      keepalive: true,
      // Never let log forwarding trip a credentials or CORS policy.
      credentials: "omit",
    }).catch(() => {
      // The dev server going away must not surface as an app error.
    });
  } finally {
    forwarding = false;
  }
}

function onWindowError(endpoint: string, event: ErrorEvent): void {
  forward(endpoint, "error", [
    event.error ??
      `${event.message} @ ${event.filename}:${event.lineno}:${event.colno}`,
  ]);
}

function onRejection(endpoint: string, event: PromiseRejectionEvent): void {
  forward(endpoint, "error", ["unhandled rejection:", event.reason]);
}

let errorHandler: ((e: ErrorEvent) => void) | undefined;
let rejectionHandler: ((e: PromiseRejectionEvent) => void) | undefined;

/**
 * Mirror console output and uncaught errors to the dev server, which prints
 * them into .forge/logs/<env>/frontend_<name>.log.
 *
 * The window listeners are the point of the feature as much as the console
 * override is: an uncaught TypeError or a rejected promise produces no console
 * call of its own, and those are exactly the failures worth seeing.
 *
 * Safe to call more than once; the second call is a no-op.
 */
export function installDevLogging(options: DevLoggingOptions): void {
  // `dev` is read off `options` rather than destructured with a default, and
  // checked FIRST. A bundler constant-folds `import.meta.env.DEV` to false at
  // the CALL SITE and can then drop the argument entirely, leaving a bare
  // `installDevLogging({})` — with a defaulted `dev` that call would install
  // the override in a production bundle. `options?.dev !== true` is therefore
  // fail-closed: absent, undefined, or anything non-true means do nothing.
  if (options?.dev !== true) return;
  if (installed) return;
  if (typeof window === "undefined") return; // SSR / prerender pass

  const { endpoint = DEV_LOG_ENDPOINT, mirrorToConsole = true } = options;
  installed = true;

  for (const level of LEVELS) {
    // Keep the ORIGINAL reference, not a bound copy: uninstall must restore
    // console.log to the identical function it replaced, so a second consumer
    // holding that reference (or a test asserting on it) sees no difference.
    const previous = console[level] as (...args: unknown[]) => void;
    original[level] = previous;
    console[level] = (...args: unknown[]) => {
      if (mirrorToConsole) previous.apply(console, args);
      forward(endpoint, level, args);
    };
  }

  errorHandler = (e) => onWindowError(endpoint, e);
  rejectionHandler = (e) => onRejection(endpoint, e);
  window.addEventListener("error", errorHandler);
  window.addEventListener("unhandledrejection", rejectionHandler);
}

/** Restore the original console methods and remove the window listeners. */
export function uninstallDevLogging(): void {
  if (!installed) return;
  for (const level of LEVELS) {
    const previous = original[level];
    if (previous) console[level] = previous;
    delete original[level];
  }
  if (errorHandler) window.removeEventListener("error", errorHandler);
  if (rejectionHandler)
    window.removeEventListener("unhandledrejection", rejectionHandler);
  errorHandler = undefined;
  rejectionHandler = undefined;
  installed = false;
}

/** Whether the console override is currently installed. Exposed for tests. */
export function devLoggingInstalled(): boolean {
  return installed;
}
