// Scaffolded by forge. Yours to own — forge will not overwrite this file.
// Expo's flat config supports ESLint 9 without importing browser/Next rules.
const expo = require("eslint-config-expo/flat");

module.exports = [
  ...expo,
  { ignores: [".expo/**", "dist/**", "coverage/**", "src/gen/**"] },
];
