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

module.exports = config;
