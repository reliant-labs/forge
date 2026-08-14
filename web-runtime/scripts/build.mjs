// Build dist/ — the .js and .d.ts this package publishes.
//
// This is `prepare`, not just `build`, and that matters. npm runs `prepare`
// in exactly the two places a consumable dist/ has to exist and nowhere else:
//
//   - before `npm publish` / `npm pack`, so the tarball carries dist/;
//   - when a consumer installs this directory as a `file:` dependency —
//     the dev bridge a dev forge build writes into a generated frontend's
//     package.json. npm symlinks the directory and then runs `prepare` HERE,
//     in the link target, so the app's very first `npm install` produces the
//     dist/ it is about to resolve.
//
// It does NOT run when a consumer installs the published tarball from the
// registry: dist/ is already in it. So no user ever pays for this script.
//
// The bootstrap below is what makes the `file:` case survive a machine that
// has never developed this package. npm installs a linked package's own
// DEPENDENCIES into the consumer, but it does not install the link target's
// devDependencies, and it does not create node_modules inside the target. On
// a bare checkout — forge's CI, a contributor's first clone — `tsc` is
// therefore simply not there, and a plain `tsc` prepare dies with exit 127
// and takes the consuming app's `npm install` down with it. Rather than fail,
// or (worse) skip the build and hand the app a package with no dist/ at all,
// install this package's own devDependencies once and proceed.
//
// --ignore-scripts on that inner install is load-bearing: `npm install` with
// no arguments runs `prepare`, which is this file. Without it the bootstrap
// recurses forever.
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const pkgDir = dirname(dirname(fileURLToPath(import.meta.url)));
const tsc = join(pkgDir, "node_modules", "typescript", "bin", "tsc");

function run(command, args) {
  const result = spawnSync(command, args, {
    cwd: pkgDir,
    stdio: "inherit",
    // npm resolves through a shim on Windows; node does not.
    shell: command === "npm" && process.platform === "win32",
  });
  if (result.error) {
    console.error(`@reliantlabs/forge-web-runtime: could not run ${command}:`, result.error.message);
    process.exit(1);
  }
  if (result.status !== 0) {
    process.exit(result.status ?? 1);
  }
}

if (!existsSync(tsc)) {
  console.log(
    "@reliantlabs/forge-web-runtime: no local toolchain — installing devDependencies once to build dist/",
  );
  run("npm", ["install", "--no-audit", "--no-fund", "--ignore-scripts"]);
}

// Invoke the compiler through node rather than PATH: an npm lifecycle script
// gets node_modules/.bin on PATH, but a bare `npm run build` from a shell in
// a half-installed checkout may not.
run(process.execPath, [tsc, "-p", "tsconfig.build.json"]);
