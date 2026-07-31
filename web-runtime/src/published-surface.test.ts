// Part of @reliant-labs/web-runtime — the web twin of forge/pkg.
//
// The publish contract: what a consumer resolves, and what it must never be
// able to resolve.
//
// This package used to publish its TypeScript SOURCES — `types` pointed at
// src/index.ts — which made every consumer typecheck this package's internals
// against its own React. That is not a version-pinning bug to chase; it is
// structural. A React 18 consumer (Expo SDK 52 pins 18.3) compiling sources
// written against @types/react 19 got errors in files it never rendered:
//
//   providers.tsx: 'RuntimeErrorBoundary' cannot be used as a JSX component
//   resource.tsx:  Type '0n' is not assignable to type 'ReactNode'
//
// Now it publishes dist/. Every consumer gets .d.ts, which their tsconfig's
// skipLibCheck does not re-typecheck, and whose React references are resolved
// in THEIR program against THEIR React. This test pins the three properties
// that keeps true.
//
// `pretest` builds dist/ before vitest runs, so these assertions always read
// a current build rather than whatever happened to be on disk.
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

const pkgDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");

interface Manifest {
  files: string[];
  main: string;
  types: string;
  exports: Record<string, { types: string; default: string } | string>;
  peerDependencies: Record<string, string>;
}

const pkg = JSON.parse(
  readFileSync(join(pkgDir, "package.json"), "utf8"),
) as Manifest;

const distFiles = readdirSync(join(pkgDir, "dist"));
const declarations = distFiles.filter((f) => f.endsWith(".d.ts"));

describe("the published surface", () => {
  it("resolves to dist/, and offers no route back into src/", () => {
    // Nothing a resolver can follow may name src/. `exports` fences modern
    // resolvers off the rest of the tree by itself; `main`/`types` are what
    // the node10-era resolvers read instead.
    const entryPoints = [
      pkg.main,
      pkg.types,
      ...Object.values(pkg.exports).flatMap((e) =>
        typeof e === "string" ? [e] : [e.types, e.default],
      ),
    ];
    for (const entry of entryPoints) {
      expect(entry.startsWith("./src/"), `${entry} still points into src/`).toBe(
        false,
      );
    }

    // And src/ is not in the tarball at all, so a tool that GLOBS the package
    // directory rather than resolving through it — Tailwind's `@source`
    // directive scans exactly this package — cannot reach the sources either.
    expect(pkg.files).not.toContain("src");
    expect(pkg.files).toEqual(["dist", "interceptors", "README.md"]);
  });

  it("declares a React type surface that is identical on React 18 and 19", () => {
    // The peer range is the promise; this is the property that makes it true.
    expect(pkg.peerDependencies["react"]).toBe("^18.3.0 || ^19.0.0");

    // @types/react 19 declares JSX.Element inside the "react" module; 18.3
    // declares it globally and re-exports it from "react/jsx-runtime". So an
    // INFERRED component return type is emitted as
    // `import("react").JSX.Element` when built against 19 and
    // `import("react/jsx-runtime").JSX.Element` when built against 18 — the
    // published .d.ts would silently depend on which major happened to be
    // installed when it was compiled, and the wrong one breaks the consumer.
    //
    // Every exported component therefore declares `: ReactElement`, a name
    // both majors export from "react". Adding a component and letting its
    // return type be inferred is what this assertion catches.
    const offenders: string[] = [];
    for (const name of declarations) {
      const body = readFileSync(join(pkgDir, "dist", name), "utf8");
      for (const [line, text] of body.split("\n").entries()) {
        if (/\bJSX\.Element\b/.test(text)) {
          offenders.push(`dist/${name}:${line + 1}: ${text.trim()}`);
        }
      }
    }
    expect(
      offenders,
      "declare these components' return type as ReactElement instead of letting it infer",
    ).toEqual([]);
  });

  it("emits a .d.ts and a .js for every module a consumer can import", () => {
    for (const entry of Object.values(pkg.exports)) {
      if (typeof entry === "string") {
        continue; // "./package.json"
      }
      for (const target of [entry.types, entry.default]) {
        expect(distFiles, `${target} is declared but not emitted`).toContain(
          target.replace("./dist/", ""),
        );
      }
    }
    // Nothing from a test file leaks into the artifact.
    expect(distFiles.filter((f) => f.includes(".test."))).toEqual([]);
  });
});
