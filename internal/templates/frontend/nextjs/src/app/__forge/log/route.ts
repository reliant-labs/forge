// yours: scaffolded once, never touched again — forge will not overwrite this file
//
// Dev-server half of browser log forwarding (Next.js).
//
// The Vite scaffold does this with a `apply: "serve"` plugin; the App Router
// has no equivalent hook, so the receiver is an ordinary route handler that
// refuses to do anything in production.
//
// It accepts the log lines that @reliantlabs/forge-web-runtime's
// installDevLogging() posts and prints them to the dev server's stdout, where
// `forge env up` is already tee-ing them to:
//
//   .forge/logs/<env>/frontend_<name>.log
//
// So a `console.error` in a component — and any uncaught error or unhandled
// rejection, which produce no console call at all — lands in a file on disk
// next to the backend service logs.
//
// The production guard is the first statement in the handler: `next build`
// still compiles this route, so unlike the Vite plugin it CAN exist in a
// deployed app, and must answer 404 there. installDevLogging() also no-ops in
// production, so nothing posts to it in the first place.
//
// To turn it off: delete this file and the installDevLogging() call in
// src/app/providers.tsx. Nothing else depends on it.

/** Cap a single log line so a runaway loop cannot fill the disk. */
const MAX_LINE = 8_000;

interface DevLogBody {
  level?: string;
  msg?: string;
}

export async function POST(request: Request): Promise<Response> {
  if (process.env.NODE_ENV === "production") {
    return new Response(null, { status: 404 });
  }

  try {
    const { level = "log", msg = "" } = (await request.json()) as DevLogBody;
    const line = msg.length > MAX_LINE ? `${msg.slice(0, MAX_LINE)}… (truncated)` : msg;
    // This console.log IS the log sink — it is what puts the browser line on
    // the dev server's stdout. No eslint-disable here: the scaffold does not
    // enable no-console, so a directive naming it is an UNUSED directive,
    // which ESLint 9 reports as a warning and the lint-clean gate rejects.
    console.log(`[browser:${level}] ${line}`);
  } catch {
    // A malformed post is not worth failing a dev request over.
  }

  return new Response(null, { status: 204 });
}
