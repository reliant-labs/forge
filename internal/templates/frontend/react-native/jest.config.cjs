// Scaffolded by forge. Yours to own — forge will not overwrite this file.
// Keep tests outside app/: Expo Router treats files there as routes.
const expo = require("jest-expo/jest-preset");

// Generated hooks use ESM runtime/Connect modules. Extend Expo's exceptions,
// rather than replacing its native transforms (or transforming all dependencies).
// pnpm's outer .pnpm directory is a container: the inner node_modules segment
// still checks the actual package against Expo's and these exceptions.
const esmPackages =
  "@reliantlabs/forge-web-runtime/|@connectrpc/(connect|connect-web)/|@bufbuild/protobuf/";
const transformIgnorePatterns = expo.transformIgnorePatterns.map((pattern) =>
  pattern.replace("/node_modules/(?!", `/node_modules/(?!\\.pnpm/|${esmPackages}|`),
);

module.exports = {
  preset: "jest-expo",
  transformIgnorePatterns,
  testMatch: ["<rootDir>/tests/**/*.test.[jt]s?(x)"],
  watch: false,
  maxWorkers: 2,
};
