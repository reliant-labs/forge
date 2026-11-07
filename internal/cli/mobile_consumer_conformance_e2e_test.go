//go:build e2e

package cli

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/generator"
	"gopkg.in/yaml.v3"
)

// These are JS consumer checks, not native builds or device/store acceptance.
// Registry and packed-source provenance are deliberately separate: the ordinary
// corpus runner enables both the Go dev bridge and an npm registry overlay.
// Standalone and workspace exercise packed source and public registry artifacts;
// customized exercises the registry. Customized is not adoption ingress.
func TestE2EMobileConsumerConformance(t *testing.T) {
	t.Parallel()
	requireTool(t, "node", "npm", "pnpm")
	bin := buildforgeBinary(t) // shared sync.Once checkout build, not PATH's forge
	for _, shape := range []string{"standalone", "workspace", "customized"} {
		sources := []string{"registry"}
		if shape != "customized" {
			sources = append(sources, "packed")
		}
		for _, source := range sources {
			t.Run(shape+"/"+source, func(t *testing.T) {
				t.Parallel()
				mobileConsumerFixture(t, bin, shape, source)
			})
		}
	}
}

const mobileConsumerProto = `syntax = "proto3";
package services.widget.v1;
option go_package = "example.com/consumer/gen/services/widget/v1;widgetv1";
service WidgetService {
 rpc GetWidget(GetWidgetRequest) returns (GetWidgetResponse) { option idempotency_level = NO_SIDE_EFFECTS; }
}
message GetWidgetRequest { int64 id = 1; }
message GetWidgetResponse { string name = 1; }
`

// Import generated factories BEFORE connect bootstrap. With eager workspace
// clients this throws during evaluation, before the test body ever runs.
const mobileConsumerBootstrap = `import { expect, test } from '@jest/globals';
import { widgetServiceQueries } from 'HOOKS_IMPORT';
import '../src/lib/connect';
import { QueryClient } from '@tanstack/react-query';

test('generated service query reaches the bootstrapped transport', async () => {
 const original = globalThis.fetch;
 const requests: {url: string; method: string; contentType: string | null; body: string}[] = [];
 globalThis.fetch = async (input, init) => {
  const request = new Request(input, init);
  requests.push({url: request.url, method: request.method, contentType: request.headers.get('Content-Type'), body: await request.text()});
  return new Response(JSON.stringify({name: 'recorded'}), {status: 200, headers: {'Content-Type': 'application/json'}});
 };
 const client = new QueryClient({defaultOptions: {queries: {retry: false}}});
 try {
  const result = await client.fetchQuery(widgetServiceQueries.getWidget({id: 1n}));
  expect(result.name).toBe('recorded');
  expect(requests).toHaveLength(1);
  expect(new URL(requests[0].url).pathname).toBe('/services.widget.v1.WidgetService/GetWidget');
  expect(requests[0].method).toBe('POST');
  expect(requests[0].contentType).toBe('application/json');
  expect(JSON.parse(requests[0].body)).toEqual({id: '1'});
 } finally { client.clear(); globalThis.fetch = original; }
});
`

type mobileConsumerRunner struct{ env []string }

func newMobileConsumerRunner(t *testing.T) mobileConsumerRunner {
	t.Helper()
	configDir := t.TempDir()
	config := filepath.Join(configDir, "global.npmrc")
	userConfig := filepath.Join(configDir, "user.npmrc")
	writeFileE2E(t, config, "")
	writeFileE2E(t, userConfig, "")
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "npm_config_") || key == linkForgeEnv || key == "GOWORK" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "npm_config_globalconfig="+config, "npm_config_userconfig="+userConfig,
		"npm_config_registry=https://registry.npmjs.org", "npm_config_lockfile_include_tarball_url=true", "CI=1", "EXPO_NO_TELEMETRY=1")
	return mobileConsumerRunner{env: env}
}

func (r mobileConsumerRunner) run(t *testing.T, dir, command string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	t.Logf("running %s %v in %s", command, args, dir)
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir, cmd.Env = dir, r.env
	startInOwnProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", command, args, dir, err, out)
	}
	t.Logf("%s %v: %s", command, args, out)
	return string(out)
}

func mobileConsumerFixture(t *testing.T, bin, shape, source string) {
	t.Helper()
	r := newMobileConsumerRunner(t)
	dir := t.TempDir()
	args := []string{"project", "new", "consumer", "--mod", "example.com/consumer", "--service", "widget"}
	if shape == "workspace" {
		args = append(args, "--frontend-workspaces")
	}
	r.run(t, dir, bin, args...)
	project := filepath.Join(dir, "consumer")
	// The unreleased Go API still comes from this checkout, but ONLY its Go
	// module copy. It has no web-runtime, so cannot supply a hidden JS devlink.
	addCorpusForgePkgReplace(t, project)
	r.run(t, project, "go", "work", "edit", "-dropuse", findRepoRoot(t))
	writeFileE2E(t, filepath.Join(project, "proto/services/widget/v1/widget.proto"), mobileConsumerProto)
	mobile := filepath.Join(project, "frontends", "mobile")
	if shape == "workspace" {
		// Explicit consumer-layout setup using the canonical production emitter.
		// CLI --frontend-workspaces currently loses the opt-in when project new
		// has no frontends. This fixture does not certify that CLI ingress.
		if err := generator.WriteFrontendWorkspaceFiles(project, "consumer", true); err != nil {
			t.Fatal(err)
		}
		r.run(t, project, bin, "scaffold", "frontend", "web", "--kind", "vite-spa", "--no-generate")
	}
	r.run(t, project, bin, "scaffold", "frontend", "mobile", "--kind", "mobile", "--no-generate")
	// Upgrade preservation for a customized scaffold (not adoption ingress).
	// Retain executable customizations, not only an unrelated sentinel file.
	owned := map[string]string{}
	if shape == "customized" {
		path := filepath.Join(mobile, "metro.config.js")
		body := readFileE2E(t, path) + "\n// App-owned Metro customization.\nmodule.exports.resolver.assetExts.push('fixtureasset');\n"
		writeFileE2E(t, path, body)
		owned[path] = body
		for _, rel := range []string{"eslint.config.cjs", "jest.config.cjs", "tests/app-config.test.ts"} {
			path := filepath.Join(mobile, rel)
			body := readFileE2E(t, path) + "\n// App-owned conformance customization.\n"
			writeFileE2E(t, path, body)
			owned[path] = body
		}
	}
	r.run(t, project, bin, "generate")
	if _, err := os.Lstat(filepath.Join(project, ".forge-link")); !os.IsNotExist(err) {
		t.Fatalf("consumer must not have a local runtime bridge: %v", err)
	}
	installDir := mobile
	if shape == "workspace" {
		installDir = project
	}
	if source == "packed" {
		// Pack a private copy: no prepare/build output is written into the shared
		// checkout, and the installed artifact cannot fall back to source paths.
		runtime := filepath.Join(t.TempDir(), "runtime")
		if err := mobileConsumerCopyRuntime(findRepoRoot(t), filepath.Dir(runtime)); err != nil {
			t.Fatal(err)
		}
		runtime = filepath.Join(filepath.Dir(runtime), "web-runtime")
		r.run(t, runtime, "npm", "install", "--ignore-scripts")
		r.run(t, runtime, "npm", "run", "build")
		packDir := t.TempDir()
		r.run(t, runtime, "npm", "pack", "--ignore-scripts", "--pack-destination", packDir)
		packs, err := filepath.Glob(filepath.Join(packDir, "*.tgz"))
		if err != nil || len(packs) != 1 {
			t.Fatalf("pack artifact: %v %v", packs, err)
		}
		mobileConsumerRuntimeOverride(t, installDir, packs[0])
	}
	if registry := strings.TrimSpace(r.run(t, installDir, "pnpm", "config", "get", "registry")); strings.TrimRight(registry, "/") != "https://registry.npmjs.org" {
		t.Fatalf("unexpected registry %q", registry)
	}
	r.run(t, installDir, "pnpm", "install", "--no-frozen-lockfile")
	lock := readFileE2E(t, filepath.Join(installDir, "pnpm-lock.yaml"))
	if source == "registry" && !strings.Contains(lock, "https://registry.npmjs.org/@reliantlabs/forge-web-runtime/-/") {
		t.Fatal("lockfile does not prove public runtime tarball provenance")
	}
	if strings.Contains(lock, ".forge-link") || strings.Contains(lock, "127.0.0.1") || strings.Contains(lock, "localhost:") {
		t.Fatal("lockfile contains dev bridge/overlay resolution")
	}
	// Node resolves the installed artifact, never a source checkout or dev link.
	r.run(t, mobile, "node", "-e", `const fs=require('fs'),path=require('path'); const root=fs.realpathSync(process.argv[1])+path.sep; const p=fs.realpathSync(require.resolve('@reliantlabs/forge-web-runtime/interceptors')); if(!p.startsWith(root)||!p.includes('node_modules')||p.includes('.forge-link')) throw Error(p); const pkg=require('@reliantlabs/forge-web-runtime/package.json'); console.log('runtime provenance: '+pkg.version+' '+p);`, project)
	hooksImport := "../src/hooks/widget-service-hooks_gen"
	if shape == "workspace" {
		hooksImport = "@consumer/hooks"
	}
	probe := strings.ReplaceAll(mobileConsumerBootstrap, "HOOKS_IMPORT", hooksImport)
	writeFileE2E(t, filepath.Join(mobile, "tests/consumer-bootstrap.test.ts"), probe)
	// Make Metro visit every portable export, including one the starter UI
	// would otherwise never reach. This is an owned route, not generated code.
	writeFileE2E(t, filepath.Join(mobile, "app/conformance.tsx"), `import { Text } from 'react-native';
import * as errors from '@reliantlabs/forge-web-runtime/interceptors';
import * as hooks from '@reliantlabs/forge-web-runtime/service-hooks';
import * as mocks from '@reliantlabs/forge-web-runtime/mock-transport';
export default function Conformance() { return <Text>{[errors, hooks, mocks].map(x => Object.keys(x).length).join(',')}</Text>; }
`)
	r.run(t, mobile, "pnpm", "run", "typecheck")
	r.run(t, mobile, "pnpm", "run", "lint")
	if shape == "workspace" {
		for _, pkg := range []string{"api", "hooks", "ui-web", "ui-native"} {
			r.run(t, filepath.Join(project, "packages", pkg), "pnpm", "run", "typecheck")
		}
		r.run(t, filepath.Join(project, "frontends", "web"), "pnpm", "run", "typecheck")
	}
	r.run(t, mobile, "pnpm", "run", "test")
	for _, platform := range []string{"ios", "android"} {
		r.run(t, mobile, "pnpm", "exec", "expo", "export", "--platform", platform, "--output-dir", filepath.Join(t.TempDir(), platform))
	}
	hookPath := filepath.Join(mobile, "src/hooks/widget-service-hooks_gen.ts")
	if shape == "workspace" {
		hookPath = filepath.Join(project, "packages/hooks/src/generated/widget-service-hooks_gen.ts")
	}
	var generated string
	for i := 0; i < 2; i++ {
		r.run(t, project, bin, "generate")
		current := readFileE2E(t, hookPath)
		if i == 1 && current != generated {
			t.Fatal("generated service hooks did not converge")
		}
		generated = current
		for path, want := range owned {
			if got := readFileE2E(t, path); got != want {
				t.Fatalf("generate replaced app-owned %s", path)
			}
		}
	}
	r.run(t, mobile, "pnpm", "run", "typecheck")
}

// Copy only package inputs, never another agent's installed or built output.
func mobileConsumerCopyRuntime(root, dst string) error {
	src := filepath.Join(root, "web-runtime")
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" || entry.Name() == "dist" || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, "web-runtime", rel), 0755)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, "web-runtime", rel), body, 0644)
	})
}

func mobileConsumerRuntimeOverride(t *testing.T, dir, pack string) {
	t.Helper()
	path := filepath.Join(dir, "pnpm-workspace.yaml")
	var manifest map[string]any
	if err := yaml.Unmarshal([]byte(readFileE2E(t, path)), &manifest); err != nil {
		t.Fatal(err)
	}
	overrides, _ := manifest["overrides"].(map[string]any)
	if overrides == nil {
		overrides = map[string]any{}
		manifest["overrides"] = overrides
	}
	overrides["@reliantlabs/forge-web-runtime"] = "file:" + filepath.ToSlash(pack)
	body, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeFileE2E(t, path, string(body))
}
