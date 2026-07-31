// CSS lint lane for this frontend. `npm run lint:styles` checks; `npm run
// lint:styles:fix` applies stylelint's autofix.
//
// The stylelint floor in package.json (>= 17.2.0) is load-bearing, not
// housekeeping. Before 17.2.0 the `lightness-notation` autofix rewrote an
// oklch lightness through `(0.58 * 100).toPrecision(3)` and then trimmed
// trailing zeros with /0+$/, so `oklch(0.58 0.09 196)` became
// `oklch(58.% 0.09 196deg)` — `58.` is not a valid CSS <number>, the browser
// drops the whole declaration, and a re-lint passes because `58.%` still
// reads as a percentage. Eight two-decimal lightnesses hit it (.07 .14 .28
// .29 .55 .56 .57 .58, where x*100 lands off-integer in binary floating
// point). 17.2.0 trims with /\.?0+$/. Do not lower the floor.
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
