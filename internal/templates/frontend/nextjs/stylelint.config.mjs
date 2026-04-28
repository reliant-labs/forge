const tailwindAtRules = [
  "theme",
  "source",
  "utility",
  "variant",
  "custom-variant",
  "plugin",
  "reference",
  "config",
  "layer",
  "apply",
];

const config = {
  extends: ["stylelint-config-standard"],
  ignoreFiles: [".next/**", "coverage/**", "dist/**", "build/**", "src/gen/**"],
  rules: {
    "at-rule-no-unknown": [
      true,
      {
        ignoreAtRules: tailwindAtRules,
      },
    ],
    "declaration-no-important": true,
  },
};

export default config;
