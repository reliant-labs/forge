// Scaffolded by forge. Yours to own — forge will not overwrite this file.
// Keep tests outside app/: Expo Router treats files there as routes.
const expo = require("jest-expo/jest-preset");

// Generated hooks use ESM runtime/Connect modules. @connectrpc/connect itself
// is NOT listed: it ships CJS, and Babel-transforming it makes ConnectError a
// class the runtime's ConnectClientError cannot extend ("Cannot call a class as
// a function"), which breaks every error path a test exercises.
// Extend Expo's exceptions,
// rather than replacing its native transforms (or transforming all dependencies).
// pnpm's outer .pnpm directory is a container: the inner node_modules segment
// still checks the actual package against Expo's and these exceptions.
const esmPackages =
  "@reliantlabs/forge-web-runtime/|@connectrpc/connect-web/|@bufbuild/protobuf/";
const transformIgnorePatterns = expo.transformIgnorePatterns.map((pattern) =>
  pattern.replace("/node_modules/(?!", `/node_modules/(?!\\.pnpm/|${esmPackages}|`),
);

// Jest appends a project's setupFiles to the preset's (React Native's and
// Expo's own, which keep running first), so list only this app's: the
// expo/fetch -> globalThis.fetch mapping that lets tests stub the network.
module.exports = {
  preset: "jest-expo",
  setupFiles: ["<rootDir>/tests/setup/expo-fetch.ts"],
  transformIgnorePatterns,
  testMatch: ["<rootDir>/tests/**/*.test.[jt]s?(x)"],
  watch: false,
  maxWorkers: 2,
};
