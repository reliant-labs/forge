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
    // Tailwind v4 documents `@import "tailwindcss";` as the canonical entry
    // point. stylelint-config-standard prefers `url("tailwindcss")` notation;
    // turn that rule off so the standard Tailwind v4 setup lints clean.
    "import-notation": null,
  },
};

export default config;
