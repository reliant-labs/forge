// ESLint v9 flat config. Replaces the deprecated `next lint` + .eslintrc.json
// combo that was removed in Next.js 16. The shape of this file is
// intentionally compact: one config array entry per concern, composed via
// FlatCompat for plugins that still ship legacy ("eslintrc-style") configs.
//
// Keep this file checked in (not generated) so users can extend it without
// fighting the scaffold.

import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

import js from "@eslint/js";
import tseslint from "typescript-eslint";
import { FlatCompat } from "@eslint/eslintrc";

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);

// FlatCompat translates legacy "extends" entries (e.g. next/core-web-vitals)
// into flat-config objects. eslint-config-next has not yet shipped a native
// flat config; the Next.js maintainers' recommended workaround is exactly
// this compat shim. Remove once eslint-config-next ships flat config.
const compat = new FlatCompat({
  baseDirectory: __dirname,
});

// Named so import/no-anonymous-default-export stays happy when users add
// plugins that enforce it (e.g. airbnb, next/typescript in strict mode).
const config = [
  {
    // Files ESLint must never touch: build artefacts, node_modules, and
    // generated code. Keeping this as the first entry makes the intent
    // explicit and short-circuits traversal.
    ignores: [
      "node_modules/**",
      ".next/**",
      "out/**",
      "coverage/**",
      "dist/**",
      "build/**",
      // Protobuf-es output is regenerated on every `buf generate` run and
      // carries /* eslint-disable */ prologue banners that trigger the
      // `reportUnusedDisableDirectives` warning under next/core-web-vitals.
      // Treat the whole tree as read-only to silence the noise.
      "src/gen/**",
    ],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...compat.extends("next/core-web-vitals", "next/typescript"),
  {
    rules: {
      complexity: "off",
      "max-lines": "off",
      "max-depth": "off",
      "max-params": "off",
    },
  },
];

export default config;