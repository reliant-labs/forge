// Metro bundler configuration — https://docs.expo.dev/guides/customizing-metro/
//
// Expo SDK 58's defaults already cover what earlier SDKs made this file carry:
//
//   - package.json "exports": enabled by default since React Native 0.79 / Expo
//     SDK 53 (metro-config defaults `unstable_enablePackageExports: true`).
//     @connectrpc/connect, @bufbuild/protobuf and @reliantlabs/forge-web-runtime
//     are reached through export maps only.
//   - Workspace roots: @expo/metro-config adds the workspace root's watch
//     folders and node_modules paths itself (pnpm-workspace.yaml and
//     package.json "workspaces" are both recognised).
//
// What remains is the local runtime bridge. When the forge binary is a dev
// build, `forge generate` symlinks its own checkout of
// @reliantlabs/forge-web-runtime into node_modules so edits land here with
// nothing published. Metro only watches the project and workspace roots, so a
// symlink pointing outside them has to be watched explicitly, and the helpers
// its transformed files pull in (@babel/runtime) have to resolve back against
// THIS app's node_modules. Both are inert once the package is installed
// normally from the registry.
//
// The last block is the dev-log receiver: the native half of `/__forge/log`,
// the endpoint the web scaffolds' dev servers serve. See devLogMiddleware below.
const path = require("node:path");

const { getDefaultConfig } = require("expo/metro-config");

const config = getDefaultConfig(__dirname);

const projectNodeModules = path.join(__dirname, "node_modules");

try {
  // require.resolve follows symlinks, so this is the runtime's REAL location.
  const runtimeDir = path.dirname(
    require.resolve("@reliantlabs/forge-web-runtime/package.json", {
      paths: [__dirname],
    }),
  );
  if (!runtimeDir.startsWith(projectNodeModules)) {
    config.watchFolders = [...(config.watchFolders ?? []), runtimeDir];
    config.resolver.nodeModulesPaths = [
      ...(config.resolver.nodeModulesPaths ?? []),
      projectNodeModules,
    ];
  }
} catch {
  // Not installed yet (pre-`pnpm install`). Metro will report the missing
  // import itself, with a better message than a config-time crash.
}

// ── TypeScript `.js` import specifiers ───────────────────────────────────
//
// protoc-gen-es is run with import_extension=.js (the web scaffolds need it:
// Vite and Next resolve `./x_pb.js` to x_pb.ts), so generated files import
// siblings as "../../../forge/v1/forge_pb.js" while only forge_pb.ts exists.
// TypeScript accepts that; Metro does not map `.js` to `.ts`, so any screen
// that imports a generated client fails with `Unable to resolve`. The
// workspace packages/api is shared with web, so the fix has to be here.
//
// This follows TypeScript's rule, and only on failure: a relative, absolute or
// package import ending in .js/.mjs/.cjs is resolved exactly as Metro would
// first; only if that throws a "could not resolve" error is the TypeScript
// source extension tried (.js -> .ts, .tsx; .mjs -> .mts; .cjs -> .cts). A
// successful resolution is never rewritten, and a module imported FROM inside
// node_modules is left alone, so a package that ships real .js is untouched.
// If every retry fails, the original error is rethrown.
//
// To turn it off: delete this block.
const TS_SOURCE_EXTS = { ".js": [".ts", ".tsx"], ".mjs": [".mts"], ".cjs": [".cts"] };

function isResolutionFailure(error) {
  const name = error?.constructor?.name;
  return name === "FailedToResolvePathError" || name === "FailedToResolveNameError";
}

const previousResolveRequest = config.resolver?.resolveRequest;
config.resolver = {
  ...config.resolver,
  resolveRequest: (context, moduleName, platform) => {
    const next = previousResolveRequest ?? context.resolveRequest;
    try {
      return next(context, moduleName, platform);
    } catch (error) {
      const ext = path.extname(moduleName);
      const candidates = TS_SOURCE_EXTS[ext];
      const origin = context.originModulePath ?? "";
      if (
        !candidates ||
        !isResolutionFailure(error) ||
        origin.split(path.sep).includes("node_modules")
      ) {
        throw error;
      }
      const stem = moduleName.slice(0, -ext.length);
      for (const tsExt of candidates) {
        try {
          return next(context, stem + tsExt, platform);
        } catch (retryError) {
          if (!isResolutionFailure(retryError)) throw retryError;
        }
      }
      throw error;
    }
  },
};

// ── Dev-log receiver ─────────────────────────────────────────────────────
//
// A device or simulator console is a dead end for anyone who is not holding
// the device — which includes every LLM agent debugging this app. The client
// (src/lib/devlog.ts, installed from app/_layout.tsx) POSTs console output,
// uncaught errors and unhandled rejections to Metro; this middleware prints
// them to the Metro process's stdout as `[browser:<level>] ...`, where
// `forge env up` already tees the frontend into
// `.forge/logs/<env>/frontend_<name>.log`. So the same grep works as on web:
//
//   grep '\[browser:error\]' .forge/logs/dev/*.log
//
// The wire protocol, limits and malformed-post handling are the web
// scaffolds' (vite-spa/src/lib/devlog-plugin.ts), deliberately:
//
//   {"entries":[{"level":"info","msg":"…"}, …]}   v2 batch
//   {"level":"info","msg":"…"}                    v1 single line
//
// A malformed or oversized post prints a warn line rather than being
// swallowed, and every answered post carries `X-Forge-Devlog: 2`.
//
// DEV ONLY, structurally: Metro runs this middleware only inside the dev
// server. `expo export` and every production bundle never start it.
//
// To turn it off: delete this block and the `__DEV__` devlog line in
// app/_layout.tsx.
const DEV_LOG_ENDPOINT = "/__forge/log";
const DEV_LOG_MAX_LINE = 8_000;
const DEV_LOG_MAX_BODY = 1024 * 1024;
const DEV_LOG_LEVELS = new Set(["log", "info", "warn", "error", "debug"]);

// Log text is untrusted: it is whatever the app (or a page it rendered, or a
// server error it echoed) wrote. Written raw into a line-oriented log file it
// could carry terminal escapes, or a newline followed by "[browser:error] ..."
// to forge a line of its own. So: strip ANSI CSI sequences, then every control
// character except \n and \t (CR included), and split on \n so EACH line is
// printed with its own `[browser:<level>] ` prefix. Identical to the web
// receivers' sanitizer (vite-spa devlog-plugin, Next.js route).
function sanitizeDevLogText(s) {
  return s.replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, "").replace(/[\x00-\x08\x0b-\x1f\x7f-\x9f]/g, "");
}

// One entry -> one or more fully prefixed lines (joined by "\n" when printed).
function formatDevLogEntry(entry) {
  const level =
    typeof entry.level === "string" && DEV_LOG_LEVELS.has(entry.level) ? entry.level : "log";
  const msg = typeof entry.msg === "string" ? entry.msg : "";
  const line =
    msg.length > DEV_LOG_MAX_LINE ? `${msg.slice(0, DEV_LOG_MAX_LINE)}… (truncated)` : msg;
  return sanitizeDevLogText(line)
    .split("\n")
    .map((part) => `[browser:${level}] ${part}`)
    .join("\n");
}

// Pure: turns a posted body into an HTTP status and the lines to print.
function handleDevLogBody(body, byteLength) {
  const size = byteLength ?? Buffer.byteLength(body);
  if (size > DEV_LOG_MAX_BODY) {
    return {
      status: 413,
      lines: [`[browser:warn] [forge-devlog] dropped oversized post (${size} bytes)`],
    };
  }
  let parsed;
  try {
    parsed = JSON.parse(body);
  } catch {
    return { status: 400, lines: ["[browser:warn] [forge-devlog] dropped malformed post"] };
  }
  if (parsed === null || typeof parsed !== "object") {
    return { status: 400, lines: ["[browser:warn] [forge-devlog] dropped malformed post"] };
  }
  const entries = Array.isArray(parsed.entries)
    ? parsed.entries.filter((e) => e !== null && typeof e === "object")
    : [parsed];
  return { status: 204, lines: entries.map(formatDevLogEntry) };
}

// Only `application/json` is accepted. The native forwarder always sends it,
// and a cross-site <form> cannot (its simple content types are
// application/x-www-form-urlencoded, multipart/form-data and text/plain),
// so a page open in the developer's browser cannot write lines into this
// log. The web receivers cannot make the same demand: their unload flush is
// a sendBeacon, which can only send text/plain without a preflight.
function isJsonRequest(req) {
  const type = String(req.headers?.["content-type"] ?? "")
    .split(";")[0]
    .trim()
    .toLowerCase();
  return type === "application/json";
}

function devLogRequest(req, res) {
  if (req.method !== "POST") {
    res.statusCode = 405;
    res.end();
    return;
  }
  if (!isJsonRequest(req)) {
    req.resume();
    res.statusCode = 415;
    res.end();
    return;
  }
  let body = "";
  let size = 0;
  req.on("data", (chunk) => {
    size += chunk.length;
    // Keep DRAINING past the cap — stopping mid-stream leaves the socket
    // half-read and the client waiting — but stop accumulating.
    if (size <= DEV_LOG_MAX_BODY) body += chunk.toString();
  });
  req.on("end", () => {
    const { status, lines } = handleDevLogBody(body, size);
    // ONE console.log for the whole batch, so a concurrent writer cannot
    // splice unrelated lines into it. Expo routes console.log to Metro's
    // stdout, which is the log sink.
    if (lines.length > 0) console.log(lines.join("\n"));
    res.statusCode = status;
    res.setHeader("X-Forge-Devlog", "2");
    res.end();
  });
}

// `server.enhanceMiddleware` is the only config-level hook Metro 0.87 offers
// for a dev-server route (`unstable_extraMiddleware` is a runServer option, not
// config). It is marked deprecated, but Expo CLI still routes its own
// middleware stack through it and chains whatever the project set here.
const previousEnhanceMiddleware = config.server?.enhanceMiddleware;
config.server = {
  ...config.server,
  enhanceMiddleware: (metroMiddleware, server) => {
    const inner = previousEnhanceMiddleware
      ? previousEnhanceMiddleware(metroMiddleware, server)
      : metroMiddleware;
    return (req, res, next) => {
      const pathname = (req.url ?? "").split("?")[0];
      if (pathname !== DEV_LOG_ENDPOINT) return inner(req, res, next);
      return devLogRequest(req, res);
    };
  },
};

module.exports = config;
