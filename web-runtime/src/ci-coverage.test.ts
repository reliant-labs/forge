import {
  mkdtempSync,
  mkdirSync,
  readFileSync,
  rmSync,
  symlinkSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, relative, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import { expect, test } from "vitest";

// Exercise the lane's actual CLI arguments with real Vitest/V8, not a glob
// reimplementation. Only forge's recording wrapper is replaced in the fixture.
test("frontend lane covers executable sources only and propagates Vitest failures", () => {
  const webRuntime = resolve(dirname(fileURLToPath(import.meta.url)), "..");
  const script = readFileSync(
    join(webRuntime, "../scripts/ci-web-runtime-test.sh"),
    "utf8",
  );
  const root = mkdtempSync(join(tmpdir(), "forge-ci-coverage-"));
  try {
    const project = join(root, "web-runtime");
    mkdirSync(join(root, "scripts"));
    mkdirSync(join(project, "src/testdata"), { recursive: true });
    symlinkSync(join(webRuntime, "node_modules"), join(project, "node_modules"), "junction");
    writeFileSync(join(project, "package.json"), '{"type":"module"}\n');
    writeFileSync(join(root, "scripts/ci-web-runtime-test.sh"), script);
    writeFileSync(join(root, "scripts/ci-forge.sh"), `#!/usr/bin/env bash
set -euo pipefail
if [[ "$2" == begin ]]; then
  printf '%s/run\\n' "$(git rev-parse --show-toplevel)"
  exit 0
fi
while [[ "$1" != -- ]]; do shift; done
shift
exec "$@"
`, { mode: 0o755 });
    const git = spawnSync("git", ["init", "--quiet", root], { encoding: "utf8" });
    expect(git.status, git.stderr).toBe(0);

    const sources: Record<string, string> = {
      "used.ts": "export const answer = () => 42;\n",
      "view.tsx": "export const view = () => <div>covered</div>;\n",
      "unimported.ts": "export const unused = () => 7;\n",
      "asset.svg": "<svg xmlns=\"http://www.w3.org/2000/svg\"/>\n",
      "types.d.ts": "export declare const value: string;\n",
      "testdata/compiler.ts": "this is deliberately invalid compiler fixture input\n",
      "used.test.ts": `import { expect, test } from 'vitest';
import { answer } from './used';
import { view } from './view';
test('fixture', () => { expect(answer()).toBe(42); expect(view).toBeTypeOf('function'); });
`,
    };
    for (const [name, source] of Object.entries(sources)) {
      writeFileSync(join(project, "src", name), source);
    }
    const runLane = () => spawnSync("bash", [join(root, "scripts/ci-web-runtime-test.sh")], {
      cwd: root,
      encoding: "utf8",
      timeout: 60000,
      env: { ...process.env, FORGE_TESTS: "on" },
    });
    const passed = runLane();
    expect(passed.error).toBeUndefined();
    expect(passed.status, passed.stdout + passed.stderr).toBe(0);
    const coverage = JSON.parse(readFileSync(join(root, "run/frontend/coverage-final.json"), "utf8"));
    const files = Object.keys(coverage).map((path) => relative(project, path).replaceAll("\\", "/")).sort();
    expect(files).toEqual(["src/unimported.ts", "src/used.ts", "src/view.tsx"]);
    const unused = coverage[join(project, "src/unimported.ts")];
    expect(Object.values(unused.s).length).toBeGreaterThan(0);
    expect(Object.values(unused.s).every((hits) => hits === 0)).toBe(true);

    writeFileSync(join(project, "src/used.test.ts"), `import { test, expect } from 'vitest';
test('intentional lane failure', () => { expect(1).toBe(2); });
`);
    const failed = runLane();
    expect(failed.error).toBeUndefined();
    expect(failed.status).toBe(1);
    expect(failed.stdout + failed.stderr).toContain("intentional lane failure");
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}, 150000);
