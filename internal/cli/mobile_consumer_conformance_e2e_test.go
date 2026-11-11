//go:build e2e

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
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

// expoDoctorVersion pins expo-doctor, which is not an app dependency and is not
// bundled by the SDK 58 CLI (`expo doctor` there only says "use npx expo-doctor").
// Never float it: a new release adds checks and would fail a green scaffold
// with no change on our side. Bump it deliberately, with the Expo SDK pin in
// the mobile package.json template, after a hand run reports N/N checks passed.
const expoDoctorVersion = "1.21.6"

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
		if strings.HasPrefix(lower, "npm_config_") || strings.HasPrefix(lower, "pnpm_config_") || key == linkForgeEnv || key == "GOWORK" {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "npm_config_globalconfig="+config, "npm_config_userconfig="+userConfig, "pnpm_config_userconfig="+userConfig,
		"npm_config_registry=https://registry.npmjs.org", "npm_config_lockfile_include_tarball_url=true", "pnpm_config_lockfile_include_tarball_url=true", "CI=1", "EXPO_NO_TELEMETRY=1",
		// expo-doctor 1.21.6 validates dependencies against live React Native
		// Directory metadata, so a Directory change could redden the e2e with no
		// change here (the unreachable classifier only covers an outage). Turn that
		// one check off; the other checks still run and are counted.
		"EXPO_DOCTOR_ENABLE_DIRECTORY_CHECK=0")
	return mobileConsumerRunner{env: env}
}

func (r mobileConsumerRunner) run(t *testing.T, dir, command string, args ...string) string {
	t.Helper()
	out, _, err := r.runExit(t, dir, command, args...)
	if err != nil {
		t.Fatalf("%s %v in %s: %v\n%s", command, args, dir, err, out)
	}
	t.Logf("%s %v: %s", command, args, out)
	return out
}

// runExit runs a command and returns its output and exit code without failing
// the test on a nonzero exit; err is set for any failure, including a command
// that never started.
func (r mobileConsumerRunner) runExit(t *testing.T, dir, command string, args ...string) (string, int, error) {
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
	exit := 0
	if err != nil {
		exit = -1
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		}
	}
	return string(out), exit, err
}

// runExpoCheck runs one Expo project check and fails the test on a real
// problem. It returns a non-empty reason when the check could not run because
// Expo's service was unreachable; the caller skips once the rest of the case
// has run, so a host outage costs only this check's verdict.
func (r mobileConsumerRunner) runExpoCheck(t *testing.T, dir string, classify func(string, int) expoCheckVerdict, command string, args ...string) string {
	t.Helper()
	out, exit, err := r.runExit(t, dir, command, args...)
	switch classify(out, exit) {
	case expoCheckPassed:
		t.Logf("%s %v: %s", command, args, out)
		return ""
	case expoCheckUnreachable:
		t.Logf("%s %v: Expo service unreachable (exit %d):\n%s", command, args, exit, out)
		return fmt.Sprintf("Expo API (api.expo.dev / exp.host) unreachable: %s %v could not complete", command, args)
	default:
		t.Fatalf("%s %v in %s failed (exit %d, err %v):\n%s", command, args, dir, exit, err, out)
		return ""
	}
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
		// The native-auth files are owned too: an app that edited them keeps its edits.
		for _, rel := range []string{
			"eslint.config.cjs", "jest.config.cjs", "tests/app-config.test.ts",
			"src/lib/auth/config.ts", "app/sign-in.tsx", "tests/auth-config.test.ts",
		} {
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
	// Expo's own project checks, in the frontend dir with the case's package
	// manager. Both reach Expo's API; an unreachable API skips at the end of the
	// case (see runExpoCheck), a real finding fails now.
	var expoSkip string
	for _, reason := range []string{
		r.runExpoCheck(t, mobile, classifyExpoDoctor, "pnpm", "dlx", "expo-doctor@"+expoDoctorVersion),
		r.runExpoCheck(t, mobile, classifyExpoInstallCheck, "pnpm", "exec", "expo", "install", "--check"),
	} {
		if reason != "" {
			expoSkip = reason
		}
	}
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
	// A route that imports the generated service client. The generated _pb files
	// import siblings as "…/forge_pb.js" (protoc-gen-es import_extension=.js, which
	// the web scaffolds need), which only Metro's resolver wrapper in the owned
	// metro.config.js maps back to forge_pb.ts: no other route reaches a _pb file,
	// so without this, a bundle that cannot resolve them would still export green.
	widgetPb := "../src/gen/services/widget/v1/widget_pb"
	if shape == "workspace" {
		widgetPb = "@consumer/api/services/widget/v1/widget_pb"
	}
	writeFileE2E(t, filepath.Join(mobile, "app/generated-client.tsx"), `import { Text } from 'react-native';
import { WidgetService } from 'WIDGET_PB';
import { connectClient } from '../src/lib/connect';
export default function GeneratedClient() {
 const client = connectClient(WidgetService);
 return <Text>{typeof client.getWidget}</Text>;
}
`)
	routePath := filepath.Join(mobile, "app/generated-client.tsx")
	writeFileE2E(t, routePath, strings.ReplaceAll(readFileE2E(t, routePath), "WIDGET_PB", widgetPb))
	r.run(t, mobile, "pnpm", "run", "typecheck")
	// Warnings fail too. pnpm 12 forwards args after the script name to it
	// directly; a `--` separator would reach eslint literally.
	r.run(t, mobile, "pnpm", "run", "lint", "--max-warnings", "0")
	if shape == "workspace" {
		for _, pkg := range []string{"api", "hooks", "ui-web", "ui-native"} {
			r.run(t, filepath.Join(project, "packages", pkg), "pnpm", "run", "typecheck")
		}
		r.run(t, filepath.Join(project, "frontends", "web"), "pnpm", "run", "typecheck")
	}
	assertNativeAuthScaffold(t, mobile)
	jestOut := r.run(t, mobile, "pnpm", "run", "test")
	assertNativeAuthSuitesPassed(t, jestOut)
	for _, platform := range []string{"ios", "android"} {
		outDir := filepath.Join(t.TempDir(), platform)
		r.run(t, mobile, "pnpm", "exec", "expo", "export", "--platform", platform, "--output-dir", outDir)
		assertNoDevlogInExport(t, outDir)
		assertNoAuthFakesInExport(t, outDir)
	}
	// `pnpm run dev` starts Expo, which rewrites a tsconfig.json it finds
	// non-canonical (typed routes). The scaffold already ships Expo's canonical
	// include, so the first run must leave the owned file byte-identical.
	tsconfigPath := filepath.Join(mobile, "tsconfig.json")
	tsconfigBefore := readFileE2E(t, tsconfigPath)
	r.assertDevServerBundles(t, mobile)
	if got := readFileE2E(t, tsconfigPath); got != tsconfigBefore {
		t.Fatalf("pnpm run dev rewrote the scaffold's tsconfig.json:\n--- before\n%s\n--- after\n%s", tsconfigBefore, got)
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
	if expoSkip != "" {
		t.Skip(expoSkip + " (every other step of this case passed)")
	}
}

// nativeAuthSuites are the Jest suites the native-auth slice scaffolds; the
// consumer's `pnpm run test` must run every one of them.
var nativeAuthSuites = []string{
	"auth-config", "auth-oidc", "auth-store-session", "auth-provider",
	"auth-context", "auth-sign-in-error", "connect-transport",
}

// assertNativeAuthScaffold fails when a native-auth file is missing, so a
// scaffold that silently dropped one cannot pass on the suites that remain.
func assertNativeAuthScaffold(t *testing.T, mobile string) {
	t.Helper()
	files := []string{
		"app/sign-in.tsx", "app/oauthredirect.tsx", "app/signedout.tsx", "tests/auth-fakes.ts",
	}
	for _, name := range []string{"config", "oidc", "token-store", "oidc-provider", "session", "native", "transport", "sign-in-error"} {
		files = append(files, "src/lib/auth/"+name+".ts")
	}
	for _, suite := range nativeAuthSuites {
		ext := ".test.ts"
		if suite == "auth-context" {
			ext = ".test.tsx"
		}
		files = append(files, "tests/"+suite+ext)
	}
	for _, rel := range files {
		if _, err := os.Stat(filepath.Join(mobile, rel)); err != nil {
			t.Errorf("native auth scaffold is missing %s: %v", rel, err)
		}
	}
	app := readFileE2E(t, filepath.Join(mobile, "app.json"))
	for _, want := range []string{"expo-secure-store", "com.consumer.mobile"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.json does not mention %q:\n%s", want, app)
		}
	}
}

// assertNativeAuthSuitesPassed reads Jest's own report: each auth suite must
// have run and passed (a testMatch regression would drop them silently).
func assertNativeAuthSuitesPassed(t *testing.T, out string) {
	t.Helper()
	for _, suite := range nativeAuthSuites {
		if !strings.Contains(out, "PASS tests/"+suite+".test.") {
			t.Errorf("jest did not report a passing tests/%s suite:\n%s", suite, out)
		}
	}
	if strings.Contains(out, "FAIL ") {
		t.Errorf("jest reported a failing suite:\n%s", out)
	}
}

// authFakeMarkers are literals that exist only in tests/auth-fakes.ts. Test
// fakes live outside app/ and src/, so a production bundle containing one means
// app code imported them.
var authFakeMarkers = []string{"https://idp.example.com", "com.acme.app:/oauthredirect", "FakeSecureStore", "FakeIdp"}

// assertNoAuthFakesInExport proves the production bundle carries no test
// fakes, and that the search can see the bundle: the real redirect route name
// must be present, or an unreadable bundle would pass vacuously.
func assertNoAuthFakesInExport(t *testing.T, outDir string) {
	t.Helper()
	sawRoute := false
	err := filepath.WalkDir(outDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(b)
		for _, marker := range authFakeMarkers {
			if strings.Contains(body, marker) {
				t.Errorf("production export %s contains test fake %q", path, marker)
			}
		}
		if strings.Contains(body, "oauthredirect") {
			sawRoute = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRoute {
		t.Errorf("production export %s has no oauthredirect route: the fake search cannot see the bundle", outDir)
	}
}

// assertNoDevlogInExport proves the production bundle carries no dev-log
// forwarder: every file the export wrote is searched for the forwarder's tag.
func assertNoDevlogInExport(t *testing.T, outDir string) {
	t.Helper()
	files := 0
	err := filepath.WalkDir(outDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		files++
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "forge-devlog") {
			t.Errorf("production export %s contains %q", path, "forge-devlog")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 {
		t.Fatalf("export wrote no files under %s", outDir)
	}
}

// assertDevServerBundles runs the scaffold's own `pnpm run dev` (what `forge
// env up` runs), not `pnpm exec expo`: pnpm's .bin shim sets NODE_PATH, which
// hides a preset Metro cannot resolve from the app root under isolated
// node_modules. It then fetches the iOS bundle the way a device would, via the
// manifest's launch asset, and requires a real 200.
func (r mobileConsumerRunner) assertDevServerBundles(t *testing.T, dir string) {
	t.Helper()
	port := freePortE2E(t)
	logPath := filepath.Join(t.TempDir(), "dev.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pnpm", "run", "dev")
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, r.env...), fmt.Sprintf("PORT=%d", port), "EXPO_OFFLINE=1")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	startInOwnProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessTree(cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pnpm run dev: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		_ = killProcessTree(cmd.Process.Pid, syscall.SIGKILL)
		<-exited
	}()
	devLog := func() string { b, _ := os.ReadFile(logPath); return string(b) }

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 6 * time.Minute}
	get := func(url string, header map[string]string) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return 0, nil, err
		}
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return resp.StatusCode, body, err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		select {
		case err := <-exited:
			t.Fatalf("pnpm run dev exited before Metro was ready: %v\n%s", err, devLog())
		default:
		}
		if code, body, err := get(base+"/status", nil); err == nil && code == 200 && strings.Contains(string(body), "packager-status:running") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Metro did not become ready on %s\n%s", base, devLog())
		}
		time.Sleep(time.Second)
	}
	code, body, err := get(base+"/", map[string]string{"expo-platform": "ios", "Accept": "application/expo+json,application/json"})
	if err != nil || code != 200 {
		t.Fatalf("iOS manifest: status %d err %v\n%s\n%s", code, err, body, devLog())
	}
	var manifest struct {
		LaunchAsset struct {
			URL string `json:"url"`
		} `json:"launchAsset"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil || manifest.LaunchAsset.URL == "" {
		t.Fatalf("iOS manifest has no launchAsset.url (%v): %s", err, body)
	}
	bundleURL := manifest.LaunchAsset.URL
	if u, err := url.Parse(bundleURL); err == nil {
		bundleURL = base + u.RequestURI() // the manifest host is the LAN address
	}
	code, body, err = get(bundleURL, nil)
	if err != nil || code != 200 || len(body) == 0 {
		t.Fatalf("iOS bundle %s: status %d, %d bytes, err %v\n%.2000s\n%s", bundleURL, code, len(body), err, body, devLog())
	}
	if strings.Contains(devLog(), "Cannot find module") {
		t.Fatalf("dev server logged a resolution failure:\n%s", devLog())
	}
	t.Logf("pnpm run dev served %d bytes for %s", len(body), bundleURL)

	// The dev-log receiver, with the scaffold's v2 batch shape. The second
	// entry tries to forge a line from another level and carry an ANSI escape.
	const marker = "DEVLOGPROBE"
	payload, err := json.Marshal(map[string]any{"entries": []map[string]string{
		{"level": "info", "msg": "hello-" + marker},
		{"level": "info", "msg": "first-" + marker + "\n[browser:error] forged-" + marker + "\x1b[31mansi-" + marker},
	}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/__forge/log", strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /__forge/log: %v\n%s", err, devLog())
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("POST /__forge/log: status %d, want 204\n%s", resp.StatusCode, devLog())
	}
	if got := resp.Header.Get("X-Forge-Devlog"); got != "2" {
		t.Fatalf("X-Forge-Devlog = %q, want 2", got)
	}
	var lines []string
	for end := time.Now().Add(15 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		lines = strings.Split(devLog(), "\n")
		if strings.Contains(devLog(), "ansi-"+marker) || time.Now().After(end) {
			break
		}
	}
	var sawHello, sawForged bool
	for _, line := range lines {
		if !strings.Contains(line, marker) && !strings.HasPrefix(line, "[browser:error] forged") {
			continue
		}
		if strings.HasPrefix(line, "[browser:error] forged") {
			t.Fatalf("sender forged a [browser:error] line:\n%s", devLog())
		}
		if strings.ContainsRune(line, 0x1b) {
			t.Fatalf("dev log line carries an ESC character: %q", line)
		}
		if strings.HasPrefix(line, "[browser:info] ") && strings.Contains(line, "hello-"+marker) {
			sawHello = true
		}
		if strings.Contains(line, "forged-"+marker) {
			if !strings.HasPrefix(line, "[browser:info] ") {
				t.Fatalf("continuation line lacks the sender's level prefix: %q", line)
			}
			sawForged = true
		}
	}
	if !sawHello || !sawForged {
		t.Fatalf("dev log missing posted entries (hello=%v forged=%v):\n%s", sawHello, sawForged, devLog())
	}
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
