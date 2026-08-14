// Metro bundler configuration — https://docs.expo.dev/guides/customizing-metro/
//
// Two departures from the Expo defaults. Both are load-bearing for the API
// client; delete either and `expo start` fails to resolve a module.
//
//   1. unstable_enablePackageExports. @connectrpc/connect, @bufbuild/protobuf
//      and @reliantlabs/forge-web-runtime declare their entry points ONLY through
//      package.json "exports" — no physical file at the old path. Metro in
//      this Expo SDK still defaults that resolution off, so without the flag
//      the first RPC import dies on `Unable to resolve module
//      @bufbuild/protobuf/wire`. A later Expo SDK turns it on by default.
//
//   2. watchFolders / nodeModulesPaths, for the local runtime bridge. When
//      the forge binary is a dev build, `forge generate` symlinks its own
//      checkout of @reliantlabs/forge-web-runtime into node_modules so edits land
//      here with nothing published. Metro crawls only the project root, so a
//      symlink pointing outside it has to be watched explicitly — and the
//      helpers its transformed files pull in (@babel/runtime) have to resolve
//      back against THIS app's node_modules. Both lines are inert once the
//      package is installed normally from the registry.
const path = require("node:path");

const { getDefaultConfig } = require("expo/metro-config");

const config = getDefaultConfig(__dirname);

config.resolver.unstable_enablePackageExports = true;

const projectNodeModules = path.join(__dirname, "node_modules");
config.resolver.nodeModulesPaths = [
  ...(config.resolver.nodeModulesPaths ?? []),
  projectNodeModules,
];

try {
  // require.resolve follows symlinks, so this is the runtime's REAL location.
  const runtimeDir = path.dirname(
    require.resolve("@reliantlabs/forge-web-runtime/package.json", {
      paths: [__dirname],
    }),
  );
  if (!runtimeDir.startsWith(projectNodeModules)) {
    config.watchFolders = [...(config.watchFolders ?? []), runtimeDir];
  }
} catch {
  // Not installed yet (pre-`npm install`). Metro will report the missing
  // import itself, with a better message than a config-time crash.
}

module.exports = config;
