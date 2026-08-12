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
  peerDependenciesMeta?: Record<string, { optional?: boolean }>;
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

  it("publishes the engine subpaths OUTSIDE the barrel", () => {
    // The exact entry points a consumer can name. Adding one is a public API
    // decision, so it is spelled out here rather than derived.
    expect(Object.keys(pkg.exports).sort()).toEqual([
      ".",
      "./interceptors",
      "./mock-transport",
      "./otel",
      "./package.json",
      "./service-hooks",
    ]);

    // `./mock-transport`, `./otel` and `./service-hooks` are deliberately NOT
    // re-exported from the barrel, and each for its own reason:
    //
    //   - mock-transport is dev-only. It pulls the fixture dispatch engine
    //     in, and a production bundle has to be able to shake it out
    //     entirely. A barrel re-export would anchor it in every import of
    //     this package.
    //   - otel imports the eight OpenTelemetry SDK packages. Only the
    //     Next.js scaffold installs those; a Vite-SPA or React-Native
    //     frontend declares @opentelemetry/api alone. Re-exporting from the
    //     barrel would leave those frontends resolving packages they never
    //     installed — a build error in a file they do not use.
    //   - service-hooks imports @tanstack/react-query. Every frontend forge
    //     scaffolds installs it, but the barrel is also what a consumer
    //     imports for interceptors and errors alone; anchoring React Query
    //     there would make a non-React consumer resolve a package it never
    //     declared.
    const barrel = readFileSync(join(pkgDir, "dist", "index.d.ts"), "utf8");
    for (const forbidden of ["./mock-transport", "./otel", "./service-hooks"]) {
      expect(
        barrel.includes(forbidden),
        `index.d.ts re-exports ${forbidden}; it must stay reachable only through its own subpath`,
      ).toBe(false);
    }
  });

  it("declares the OTel SDK peers optional, and @opentelemetry/api required", () => {
    // The `./otel` subpath's imports are peers, not dependencies: the
    // consuming app owns the OTel version so the browser loads exactly one
    // copy of the SDK. Marking them OPTIONAL is what lets the frontends that
    // never import `./otel` install nothing and still get a clean, warning-
    // free `npm install`.
    const sdkPeers = Object.keys(pkg.peerDependencies).filter(
      (name) =>
        name.startsWith("@opentelemetry/") && name !== "@opentelemetry/api",
    );
    expect(sdkPeers.length).toBeGreaterThan(0);
    for (const name of sdkPeers) {
      expect(
        pkg.peerDependenciesMeta?.[name]?.optional,
        `${name} must be an OPTIONAL peer — only the Next.js scaffold installs it`,
      ).toBe(true);
    }

    // @opentelemetry/api is the exception and must stay required: trace.ts
    // propagates W3C traceparent on every RPC from the BARREL, with no
    // collector and no SDK. Every frontend resolves it.
    expect(pkg.peerDependenciesMeta?.["@opentelemetry/api"]?.optional).not.toBe(
      true,
    );
  });

  it("declares @tanstack/react-query an optional peer, reachable only via ./service-hooks", () => {
    // Same rule as the OTel SDKs: the ONLY module that imports React Query is
    // the ./service-hooks subpath. A consumer that imports the barrel for
    // interceptors and errors must not be told it is missing a dependency it
    // will never resolve.
    expect(pkg.peerDependencies["@tanstack/react-query"]).toBeDefined();
    expect(
      pkg.peerDependenciesMeta?.["@tanstack/react-query"]?.optional,
      "@tanstack/react-query must be an OPTIONAL peer — only ./service-hooks imports it",
    ).toBe(true);
  });

  it("keeps the base-path helpers IN the barrel", () => {
    // The counterpart to the subpath rule above. basepath.ts has no imports
    // at all, so there is no dependency footprint to fence off — and the
    // scaffolded src/lib/basepath_gen.ts is a one-liner that reaches for it
    // from the barrel. A regression that moved it behind a subpath would
    // break every generated frontend.
    const barrel = readFileSync(join(pkgDir, "dist", "index.d.ts"), "utf8");
    for (const symbol of ["createBasePath", "joinBasePath", "normalizeBasePath"]) {
      expect(barrel.includes(symbol), `barrel must export ${symbol}`).toBe(true);
    }
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
