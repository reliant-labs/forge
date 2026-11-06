// App-owned Forge dev entrypoint. Customize here; regeneration leaves it alone.
// Expo SDK 52 reads --port / RCT_METRO_PORT, not Forge's PORT.
function devArgs(args, port) {
  if (!port) return ["start", ...args];
  if (!/^[0-9]+$/.test(port) || Number(port) < 1 || Number(port) > 65535) {
    throw new Error("PORT must be an integer between 1 and 65535");
  }
  // Forge's allocated listener must agree with its readiness checks. Do not
  // silently allow a forwarded Expo flag to move it to another port.
  if (args.some((arg) => arg === "--port" || arg.startsWith("--port=") || arg === "-p" || /^-p[=0-9]/.test(arg))) {
    throw new Error("PORT is set; configure the dev port in Forge, not with --port/-p");
  }
  return ["start", "--port", port, ...args];
}

module.exports = { devArgs };

if (require.main === module) {
  process.argv = [process.argv[0], require.resolve("expo/bin/cli"), ...devArgs(process.argv.slice(2), process.env.PORT)];
  // Like Expo's own bin/cli, load in-process: no shell, detached child or
  // signal/exit-code proxy. Expo owns this foreground process and its handlers.
  require("expo/bin/cli");
}
