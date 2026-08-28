// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// Dev-server half of browser log forwarding.
//
// The browser console is a dead end for anyone who is not sitting in front of
// devtools — which includes every LLM agent debugging this app. This plugin
// accepts the log lines that @reliantlabs/forge-web-runtime's
// installDevLogging() posts and prints them to the dev server's stdout, where
// `forge env up` is already tee-ing them to:
//
//   .forge/logs/<env>/frontend_<name>.log
//
// So a `console.error` in a React component — and any uncaught error or
// unhandled rejection, which produce no console call at all — lands in a file
// on disk next to the backend service logs.
//
// DEV ONLY, structurally: `apply: "serve"` means Vite loads this for the dev
// server and never for `vite build`, so the endpoint cannot exist in a
// production bundle.
//
// To turn it off: delete the `devLogPlugin()` entry from vite.config.ts (and
// the installDevLogging() call in src/main.tsx). Nothing else depends on it.

import type { Plugin } from "vite";

/** Endpoint the web-runtime client posts to. Keep in sync with DEV_LOG_ENDPOINT. */
const ENDPOINT = "/__forge/log";

/** Cap a single log line so a runaway loop cannot fill the disk. */
const MAX_LINE = 8_000;

interface DevLogBody {
  level?: string;
  msg?: string;
}

export function devLogPlugin(): Plugin {
  return {
    name: "forge-dev-log",
    apply: "serve",
    configureServer(server) {
      server.middlewares.use(ENDPOINT, (req, res) => {
        if (req.method !== "POST") {
          res.statusCode = 405;
          res.end();
          return;
        }

        let body = "";
        let tooLong = false;
        req.on("data", (chunk: Buffer) => {
          if (tooLong) return;
          body += chunk.toString();
          if (body.length > MAX_LINE * 2) tooLong = true;
        });

        req.on("end", () => {
          try {
            const { level = "log", msg = "" } = JSON.parse(body) as DevLogBody;
            const line =
              msg.length > MAX_LINE
                ? `${msg.slice(0, MAX_LINE)}… (truncated)`
                : msg;
            // This console.log IS the log sink — it is what puts the browser
            // line on the dev server's stdout. No eslint-disable here: the
            // scaffold does not enable no-console, so a directive naming it is
            // an UNUSED directive, which ESLint 9 reports as a warning and the
            // lint-clean gate rejects.
            console.log(`[browser:${level}] ${line}`);
          } catch {
            // A malformed post is not worth failing a dev request over.
          }
          res.statusCode = 204;
          res.end();
        });
      });
    },
  };
}
