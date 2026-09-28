package generator

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pinProbePage is a prerendered page whose mutation hook is BUILT BY THE
// RUNTIME (createMutationHook calls useQueryClient inside
// @reliantlabs/forge-web-runtime/service-hooks), while the QueryClientProvider
// above it is mounted by APP code (src/app/providers.tsx). The two meet only
// if both resolve @tanstack/react-query to one module instance — which is the
// property a directory pin breaks.
const pinProbePage = `"use client";

import { createMutationHook } from "@reliantlabs/forge-web-runtime/service-hooks";

const useProbe = createMutationHook(["pin-probe"], async () => "ok");

export default function PinProbePage() {
  const probe = useProbe();
  return <p>pin probe {probe.status}</p>;
}
`

// TestNextBuildPrerendersWithInstalledPeerPins is the end-to-end reproduction
// of "the tsconfig paths pins split packages in the bundle", run against the
// real toolchain.
//
// A frontend scaffolded exactly as `forge scaffold frontend` does it — render,
// `npm install`, then the post-install pin reconcile — must `next build` a
// prerendered page whose hook comes from the runtime. The pins resolve in this
// layout (./node_modules is the frontend's own install), which is the
// precondition for the defect: Next's webpack resolver follows a resolving
// tsconfig `paths` target for app code only, and a DIRECTORY target skips the
// package's `exports`, so app code bundled react-query's build/legacy while
// the runtime bundled build/modern:
//
//	Error occurred prerendering page "/pin-probe".
//	Error: No QueryClient set, use QueryClientProvider to set one
//
// Declaration-file pins are skipped by that resolver, so both importers go
// through `exports` and share one module.
//
// It also runs the pre-fix state (the scaffold WITHOUT the reconcile, i.e.
// directory pins) and requires it to FAIL, so the test cannot pass by the
// probe silently not being exercised.
//
// Needs npm and the registry; skipped under -short.
func TestNextBuildPrerendersWithInstalledPeerPins(t *testing.T) {
	if testing.Short() {
		t.Skip("real npm install + next build (~1-2 min); skipped in -short")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}
	t.Parallel()

	root := t.TempDir()
	if err := GenerateFrontendFilesWithOptions(root, "example.com/app", "app", "web", 8080, "", FrontendGenOptions{
		// Public: no route guard, so the probe page is actually prerendered
		// (a guarded route renders nothing server-side and would hide the
		// failure). TypedConfig: what the scaffold verbs render with.
		Public:      true,
		TypedConfig: ScaffoldedFrontendTypedConfig(),
	}); err != nil {
		t.Fatalf("scaffold frontend: %v", err)
	}
	if err := WriteScaffoldedFrontendConfigTS(root, "app", "web", "nextjs", 8080); err != nil {
		t.Fatalf("write config_gen.ts: %v", err)
	}
	feDir := filepath.Join(root, "frontends", "web")
	probeDir := filepath.Join(feDir, "src", "app", "pin-probe")
	if err := os.MkdirAll(probeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(probeDir, "page.tsx"), []byte(pinProbePage), 0o644); err != nil {
		t.Fatal(err)
	}

	// A test binary is a DEV build, so the scaffold also wrote forge's
	// maintainer-only web-runtime bridge (a gitignored workspace root linking
	// this checkout's web-runtime). A user's project has none; it installs the
	// published runtime from the registry, which is the layout under test.
	// Removing the bridge is exactly the documented way back to that shape,
	// and it avoids flipping the process-global dev-build switch under a
	// parallel test.
	for _, bridge := range []string{"package.json", ".forge-link"} {
		if err := os.RemoveAll(filepath.Join(root, bridge)); err != nil {
			t.Fatal(err)
		}
	}

	npm(t, feDir, 6*time.Minute, "install", "--no-audit", "--no-fund", "--prefer-offline")
	tsconfig := filepath.Join(feDir, "tsconfig.json")
	directoryPins, err := os.ReadFile(tsconfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(directoryPins), `"./node_modules/@tanstack/react-query"`) {
		t.Fatalf("fixture assumption broken: the scaffold no longer renders directory pins before the install, "+
			"so the pre-fix half of this test would prove nothing:\n%s", directoryPins)
	}

	// ── Before: directory pins, as every scaffold shipped them. ──
	out, buildErr := npmOutput(feDir, 5*time.Minute, "run", "build")
	if buildErr == nil {
		t.Fatalf("next build SUCCEEDED with directory pins; the probe no longer reproduces the split, "+
			"so the after-half proves nothing. Output tail:\n%s", tail(out, 40))
	}
	if !strings.Contains(out, "No QueryClient set") {
		t.Fatalf("next build with directory pins failed for a reason other than the bundle split:\n%s", tail(out, 60))
	}

	// ── After: the post-install reconcile the scaffold verbs run. ──
	ReconcileFrontendTsconfigPeers(root)
	healed, err := os.ReadFile(tsconfig)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(healed), `"./node_modules/@tanstack/react-query/build/modern/index.d.ts"`) {
		t.Fatalf("reconcile did not aim react-query at its declaration file:\n%s", healed)
	}
	_ = os.RemoveAll(filepath.Join(feDir, ".next-prod"))
	if out, err := npmOutput(feDir, 5*time.Minute, "run", "build"); err != nil {
		t.Fatalf("next build failed with declaration-file pins: %v\n%s", err, tail(out, 60))
	}
	html, err := os.ReadFile(filepath.Join(feDir, ".next-prod", "server", "app", "pin-probe.html"))
	if err != nil {
		t.Fatalf("the probe page was not prerendered: %v", err)
	}
	if !strings.Contains(string(html), "pin probe") {
		t.Errorf("prerendered probe page does not carry its content:\n%s", tail(string(html), 5))
	}

	// tsc must still dedupe through the new pins — the only reason they exist.
	if out, err := npmOutput(feDir, 3*time.Minute, "exec", "--", "tsc", "--noEmit"); err != nil {
		t.Fatalf("tsc --noEmit failed with declaration-file pins: %v\n%s", err, tail(out, 40))
	}
}

func npm(t *testing.T, dir string, timeout time.Duration, args ...string) {
	t.Helper()
	if out, err := npmOutput(dir, timeout, args...); err != nil {
		t.Fatalf("npm %s: %v\n%s", strings.Join(args, " "), err, tail(out, 60))
	}
}

func npmOutput(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npm", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "NEXT_TELEMETRY_DISABLED=1", "CI=1")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
