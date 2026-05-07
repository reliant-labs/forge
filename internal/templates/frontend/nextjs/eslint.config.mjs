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
import importPlugin from "eslint-plugin-import";
import react from "eslint-plugin-react";
import unicorn from "eslint-plugin-unicorn";
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
      // Next.js auto-generates next-env.d.ts on `next dev` / `next build`
      // and uses a triple-slash directive that
      // @typescript-eslint/triple-slash-reference rejects. Standard
      // convention is to ignore it — Next regenerates it as needed.
      "next-env.d.ts",
    ],
  },
  js.configs.recommended,
  ...tseslint.configs.recommended,
  ...compat.extends("next/core-web-vitals", "next/typescript"),
  {
    plugins: {
      react,
      import: importPlugin,
      unicorn,
    },
    rules: {
      complexity: "off",
      "max-lines": "off",
      "max-depth": "off",
      "max-params": "off",
      "react/forbid-dom-props": [
        "warn",
        {
          forbid: [
            {
              propName: "style",
              message:
                "Prefer Tailwind utilities, CSS variables, or component variants. Use inline styles only for dynamic values that cannot be expressed safely in CSS.",
            },
          ],
        },
      ],
      // Import hygiene — block circular deps outright; nudge users toward
      // named exports and grouped/sorted imports. Errors on the cycle
      // case because cycles silently break code-splitting + cause subtle
      // initialisation bugs; warn-only on the others so users can ratchet.
      "import/no-cycle": ["error", { maxDepth: 10 }],
      "import/no-default-export": "warn",
      "import/order": [
        "warn",
        {
          groups: [
            "builtin",
            "external",
            "internal",
            "parent",
            "sibling",
            "index",
            "object",
            "type",
          ],
          "newlines-between": "always",
          alphabetize: { order: "asc", caseInsensitive: true },
        },
      ],
      // Selective unicorn rules. The plugin as a whole is too noisy
      // (e.g. no-array-for-each fires on idiomatic React .map adjacent
      // patterns); these three are uncontroversial wins.
      "unicorn/prefer-string-trim-start-end": "warn",
      "unicorn/prefer-set-has": "warn",
      "unicorn/prefer-includes": "warn",
    },
  },
  {
    // Next.js requires default exports for pages, layouts, route handlers,
    // and a handful of other entry-point files. Disable no-default-export
    // for those paths only.
    files: [
      "src/app/**/page.{ts,tsx}",
      "src/app/**/layout.{ts,tsx}",
      "src/app/**/loading.{ts,tsx}",
      "src/app/**/error.{ts,tsx}",
      "src/app/**/not-found.{ts,tsx}",
      "src/app/**/template.{ts,tsx}",
      "src/app/**/route.{ts,tsx}",
      "src/middleware.{ts,tsx}",
      "next.config.{js,ts,mjs}",
      "*.config.{js,ts,mjs}",
    ],
    rules: {
      "import/no-default-export": "off",
    },
  },
];

export default config;