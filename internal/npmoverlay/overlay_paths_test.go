package npmoverlay

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// startOverlay starts an overlay over one @acme/lib 1.2.0 checkout and
// returns it with a log the test can read.
func startOverlay(t *testing.T, upstream string, mod func(*Package)) (*Overlay, string, *bytes.Buffer) {
	t.Helper()
	dir := writePackage(t, "@acme/lib", "1.2.0")
	pkg := Package{Dir: dir}
	if mod != nil {
		mod(&pkg)
	}
	var log bytes.Buffer
	o, err := Start(Options{Upstream: upstream, Packages: []Package{pkg}, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	return o, dir, &log
}

func npmOrSkip(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("runs npm pack; runs in task test")
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH")
	}
}

func TestStartRefusesBadPackageSets(t *testing.T) {
	good := writePackage(t, "@acme/lib", "1.0.0")
	badJSON := t.TempDir()
	if err := os.WriteFile(filepath.Join(badJSON, "package.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	noVersion := t.TempDir()
	if err := os.WriteFile(filepath.Join(noVersion, "package.json"), []byte(`{"name":"@acme/x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		pkgs []Package
		want string
	}{
		{"nothing to overlay", nil, "no packages to overlay"},
		{"the same package twice", []Package{{Dir: good}, {Dir: good}}, "@acme/lib is overlaid twice"},
		{"a checkout with no package.json", []Package{{Dir: t.TempDir()}}, "package.json"},
		{"a package.json that is not JSON", []Package{{Dir: badJSON}}, "package.json"},
		{"a package.json naming no version", []Package{{Dir: noVersion}}, "names no package and version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := Start(Options{Packages: tc.pkgs, Log: io.Discard})
			if err == nil {
				_ = o.Close()
				t.Fatal("Start succeeded")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
		})
	}
}

// Start routes every scope once, however many packages share it, and an
// empty Upstream falls back to the public registry.
func TestStartRoutesEachScopeOnceAndDefaultsTheUpstream(t *testing.T) {
	o, err := Start(Options{Packages: []Package{
		{Dir: writePackage(t, "@acme/lib", "1.0.0")},
		{Dir: writePackage(t, "@acme/other", "2.0.0")},
		{Dir: writePackage(t, "@zeta/tool", "3.0.0")},
	}, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = o.Close() })
	if o.upstream != DefaultUpstream {
		t.Errorf("upstream = %q, want %q", o.upstream, DefaultUpstream)
	}
	rc, err := os.ReadFile(envValue(t, o, "npm_config_globalconfig"))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"@acme", "@zeta"} {
		if n := strings.Count(string(rc), scope+":registry="+o.URL+"\n"); n != 1 {
			t.Errorf("npmrc routes %s %d times, want once:\n%s", scope, n, rc)
		}
	}
	if got := envValue(t, o, "pnpm_config_minimum_release_age_exclude"); got != `["@acme/lib","@acme/other","@zeta/tool"]` {
		t.Errorf("release-age exclusions = %s, want every overlaid package, sorted", got)
	}
}

// The files the overlay writes can carry the user's registry credentials, so
// they are private, and Close removes them together with the server.
func TestCloseStopsTheServerAndRemovesItsFiles(t *testing.T) {
	o, _, _ := startOverlay(t, "http://127.0.0.1:1", nil)
	npmrc := envValue(t, o, "npm_config_globalconfig")
	if runtime.GOOS != "windows" {
		info, err := os.Stat(npmrc)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("npmrc mode = %o, want 600", perm)
		}
	}
	if resp, err := http.Get(o.URL + "@acme%2fother"); err == nil {
		_ = resp.Body.Close()
	}
	if err := o.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(npmrc); !os.IsNotExist(err) {
		t.Errorf("npmrc survived Close (stat err %v)", err)
	}
	if resp, err := http.Get(o.URL + "@acme%2fother"); err == nil {
		_ = resp.Body.Close()
		t.Error("the overlay still answers after Close")
	}
}

func TestServeIsReadOnly(t *testing.T) {
	o, _, _ := startOverlay(t, "http://127.0.0.1:1", nil)
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		req, _ := http.NewRequest(method, o.URL+"@acme%2flib", strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s answered HTTP %d, want 405", method, resp.StatusCode)
		}
	}
}

// HEAD is as read-only as GET and is let through.
func TestServeAnswersHEADForAProxiedPackage(t *testing.T) {
	up, _ := fakeUpstream(t)
	o, _, _ := startOverlay(t, up.URL, nil)
	req, _ := http.NewRequest(http.MethodHead, o.URL+"@acme%2fother", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD answered HTTP %d, want 200", resp.StatusCode)
	}
}

// A package the overlay does not replace is relayed with the caller's Accept
// header (npm asks for the abbreviated document) and the upstream's status.
func TestProxyRelaysAcceptAndStatus(t *testing.T) {
	var accept atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept.Store(r.Header.Get("Accept"))
		http.Error(w, `{"error":"gone"}`, http.StatusGone)
	}))
	t.Cleanup(up.Close)
	o, _, _ := startOverlay(t, up.URL, nil)

	req, _ := http.NewRequest(http.MethodGet, o.URL+"@acme%2fother", nil)
	req.Header.Set("Accept", "application/vnd.npm.install-v1+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Errorf("status = %d, want the upstream's 410", resp.StatusCode)
	}
	if got := accept.Load(); got != "application/vnd.npm.install-v1+json" {
		t.Errorf("upstream saw Accept %v, want the caller's", got)
	}
	// With no Accept the proxy asks for JSON itself.
	if status, _ := get(t, o.URL+"@acme%2fother"); status != http.StatusGone {
		t.Errorf("status = %d, want 410", status)
	}
	if got := accept.Load(); got != "application/json" {
		t.Errorf("upstream saw Accept %v, want application/json by default", got)
	}
}

// An unreachable upstream is a 502 that is also logged, not a hang or a 200.
func TestProxyReportsAnUnreachableUpstreamAs502(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	url := up.URL
	up.Close()
	o, _, log := startOverlay(t, url, nil)

	status, body := get(t, o.URL+"@acme%2fother")
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d (%s), want 502", status, body)
	}
	if !strings.Contains(log.String(), "proxy @acme/other") {
		t.Errorf("failure not logged: %q", log.String())
	}
}

// If the packument of an overlaid package cannot be fetched, the install must
// fail loudly with the cause rather than get a packument without the local
// version.
func TestPackumentFailureModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"upstream 500", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) }, "HTTP 500: boom"},
		{"upstream sends garbage", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "<html>") }, "parse the upstream packument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(tc.handler)
			t.Cleanup(up.Close)
			o, dir, log := startOverlay(t, up.URL, nil)
			// Stand in for npm pack: the failure under test is the packument.
			lp := o.packages["@acme/lib"]
			lp.once.Do(func() { lp.err = lp.load(tarballFor(t, dir)) })

			status, body := get(t, o.URL+"@acme%2flib")
			if status != http.StatusBadGateway || !strings.Contains(string(body), tc.want) {
				t.Errorf("HTTP %d %q, want 502 containing %q", status, body, tc.want)
			}
			if !strings.Contains(log.String(), tc.want) {
				t.Errorf("failure not logged: %q", log.String())
			}
		})
	}
}

// A package that has never been published (upstream 404) is served from the
// local version alone.
func TestPackumentOfANeverPublishedPackage(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(up.Close)
	o, dir, _ := startOverlay(t, up.URL, nil)
	lp := o.packages["@acme/lib"]
	lp.once.Do(func() { lp.err = lp.load(tarballFor(t, dir)) })

	status, body := get(t, o.URL+"@acme%2flib")
	if status != http.StatusOK || !strings.Contains(string(body), `"latest":"1.2.0"`) {
		t.Fatalf("HTTP %d: %s, want a packument holding only the local version", status, body)
	}
}

// A failing Prepare is reported at Pack time, before any install starts, and
// again as a 500 to a client that asks for the package, with the cause.
func TestPrepareFailureSurfacesFromPackAndFromTheRegistry(t *testing.T) {
	errBuild := errors.New("tsc exited 2")
	o, dir, log := startOverlay(t, "http://127.0.0.1:1", func(p *Package) {
		p.Prepare = func() error { return errBuild }
	})
	err := o.Pack()
	if !errors.Is(err, errBuild) || !strings.Contains(err.Error(), "prepare "+dir) {
		t.Fatalf("Pack = %v, want the prepare error naming %s", err, dir)
	}
	status, body := get(t, o.URL+"@acme%2flib")
	if status != http.StatusInternalServerError || !strings.Contains(string(body), "tsc exited 2") {
		t.Errorf("HTTP %d %q, want 500 carrying the build error", status, body)
	}
	if !strings.Contains(log.String(), "tsc exited 2") {
		t.Errorf("failure not logged: %q", log.String())
	}
	// The tarball URL of a package that failed to pack is not found.
	if status, _ := get(t, o.URL+"-/tarballs/@acme%2flib/acme-lib-1.2.0.tgz"); status != http.StatusNotFound {
		t.Errorf("tarball of an unpackable package: HTTP %d, want 404", status)
	}
}

func TestServeTarballRefusals(t *testing.T) {
	o, dir, _ := startOverlay(t, "http://127.0.0.1:1", nil)
	lp := o.packages["@acme/lib"]
	lp.once.Do(func() { lp.err = lp.load(tarballFor(t, dir)) })

	for _, path := range []string{
		"-/tarballs/no-slash-at-all",
		"-/tarballs/@acme%2fnope/acme-lib.tgz",
		"-/tarballs/@acme%2flib/wrong-name.tgz",
	} {
		if status, _ := get(t, o.URL+path); status != http.StatusNotFound {
			t.Errorf("GET %s: HTTP %d, want 404", path, status)
		}
	}
	if status, body := get(t, o.URL+"-/tarballs/@acme%2flib/"+lp.file); status != http.StatusOK || len(body) == 0 {
		t.Errorf("GET of the real tarball: HTTP %d, %d bytes", status, len(body))
	}
}

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestTarballManifest(t *testing.T) {
	good := tgz(t, map[string]string{"package/index.js": "x", "package/package.json": `{"name":"@acme/lib","version":"1.0.0"}`})
	m, err := tarballManifest(good)
	if err != nil || m["name"] != "@acme/lib" || m["version"] != "1.0.0" {
		t.Fatalf("tarballManifest = %v, %v", m, err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want string
	}{
		{"not gzip", []byte("plain text"), "gzip"},
		{"no package.json", tgz(t, map[string]string{"package/index.js": "x"}), "no package/package.json"},
		{"manifest outside package/", tgz(t, map[string]string{"other/package.json": "{}"}), "no package/package.json"},
		{"manifest not JSON", tgz(t, map[string]string{"package/package.json": "{oops"}), "package/package.json"},
		{"empty archive", tgz(t, nil), "no package/package.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if m, err := tarballManifest(tc.data); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("tarballManifest = %v, %v; want an error containing %q", m, err, tc.want)
			}
		})
	}
}

// A tarball that does not hold the package the overlay promised is refused:
// serving it would hand npm a different package under the promised integrity.
func TestLoadRefusesATarballOfAnotherPackageOrVersion(t *testing.T) {
	for name, manifest := range map[string]string{
		"other name":    `{"name":"@acme/other","version":"1.2.0"}`,
		"other version": `{"name":"@acme/lib","version":"9.9.9"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.tgz")
			if err := os.WriteFile(path, tgz(t, map[string]string{"package/package.json": manifest}), 0o644); err != nil {
				t.Fatal(err)
			}
			lp := &localPackage{name: "@acme/lib", version: "1.2.0"}
			err := lp.load(path)
			if err == nil || !strings.Contains(err.Error(), "want @acme/lib@1.2.0") {
				t.Fatalf("load = %v, want a mismatch naming @acme/lib@1.2.0", err)
			}
			if lp.tarball != nil {
				t.Error("a refused tarball was kept")
			}
		})
	}
	lp := &localPackage{name: "@acme/lib", version: "1.2.0"}
	if err := lp.load(filepath.Join(t.TempDir(), "missing.tgz")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("load of a missing file = %v, want ErrNotExist", err)
	}
	notTar := filepath.Join(t.TempDir(), "bad.tgz")
	if err := os.WriteFile(notTar, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := lp.load(notTar); err == nil || !strings.Contains(err.Error(), "bad.tgz") {
		t.Errorf("load of junk = %v, want an error naming the file", err)
	}
}

func TestEscapeName(t *testing.T) {
	for in, want := range map[string]string{
		"@acme/lib":  "@acme%2flib",
		"lib":        "lib",
		"@scope/a b": "@scope%2fa%20b",
		"a b":        "a%20b",
	} {
		if got := escapeName(in); got != want {
			t.Errorf("escapeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReadNameVersion(t *testing.T) {
	dir := writePackage(t, "@acme/lib", "1.2.3")
	name, version, err := readNameVersion(filepath.Join(dir, "package.json"))
	if err != nil || name != "@acme/lib" || version != "1.2.3" {
		t.Fatalf("readNameVersion = %q, %q, %v", name, version, err)
	}
	if _, _, err := readNameVersion(filepath.Join(dir, "absent.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing manifest = %v, want ErrNotExist", err)
	}
}

// mergePackument refuses an upstream document it cannot parse rather than
// serving the local version alone and hiding the published ones.
func TestMergePackumentRefusesAnUnparseableUpstream(t *testing.T) {
	lp := &localPackage{name: "@acme/lib", version: "1.0.0", file: "f.tgz", manifest: map[string]any{}}
	if _, err := mergePackument([]byte("{broken"), lp, "http://x/"); err == nil || !strings.Contains(err.Error(), "parse the upstream packument") {
		t.Fatalf("err = %v, want the parse failure named", err)
	}
}

func TestExistingConfigPaths(t *testing.T) {
	t.Run("user config precedence", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		for _, k := range []string{"pnpm_config_userconfig", "PNPM_CONFIG_USERCONFIG", "npm_config_userconfig", "NPM_CONFIG_USERCONFIG"} {
			t.Setenv(k, "")
		}
		if got, want := existingUserConfig(), filepath.Join(home, ".npmrc"); got != want {
			t.Errorf("default = %q, want %q", got, want)
		}
		t.Setenv("npm_config_userconfig", "/npm/rc")
		if got := existingUserConfig(); got != "/npm/rc" {
			t.Errorf("npm_config_userconfig ignored: %q", got)
		}
		t.Setenv("pnpm_config_userconfig", "/pnpm/rc")
		if got := existingUserConfig(); got != "/pnpm/rc" {
			t.Errorf("pnpm_config_userconfig must win over npm's: %q", got)
		}
	})
	t.Run("global config derivation", func(t *testing.T) {
		for _, k := range []string{"npm_config_globalconfig", "NPM_CONFIG_GLOBALCONFIG", "npm_config_prefix", "NPM_CONFIG_PREFIX"} {
			t.Setenv(k, "")
		}
		t.Setenv("npm_config_prefix", "/opt/npm")
		if got, want := existingGlobalConfig(), filepath.Join("/opt/npm", "etc", "npmrc"); got != want {
			t.Errorf("from prefix = %q, want %q", got, want)
		}
		t.Setenv("NPM_CONFIG_GLOBALCONFIG", "/explicit/npmrc")
		if got := existingGlobalConfig(); got != "/explicit/npmrc" {
			t.Errorf("an explicit globalconfig must win over the prefix: %q", got)
		}
	})
	t.Run("global config from node's location", func(t *testing.T) {
		for _, k := range []string{"npm_config_globalconfig", "NPM_CONFIG_GLOBALCONFIG", "npm_config_prefix", "NPM_CONFIG_PREFIX"} {
			t.Setenv(k, "")
		}
		t.Setenv("PATH", t.TempDir())
		if got := existingGlobalConfig(); got != "" {
			t.Errorf("with no node on PATH and no prefix, got %q, want \"\"", got)
		}
	})
}

// A user's own npmrc missing is no reason to fail: the overlay file is just
// the scope routes. A prior file without a trailing newline keeps its last
// line intact.
func TestWriteRoutedHandlesMissingAndUnterminatedConfigs(t *testing.T) {
	o := &Overlay{dir: t.TempDir(), URL: "http://127.0.0.1:9/", scopes: []string{"@acme"}}
	path := filepath.Join(o.dir, "rc")
	if err := o.writeRouted(path, filepath.Join(o.dir, "does-not-exist")); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "@acme:registry=http://127.0.0.1:9/\n" {
		t.Errorf("routes only = %q", got)
	}
	prev := filepath.Join(o.dir, "prev")
	if err := os.WriteFile(prev, []byte("a=b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.writeRouted(path, prev); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a=b\n@acme:registry=http://127.0.0.1:9/\n" {
		t.Errorf("unterminated previous config = %q", got)
	}
}

// Pack runs a real `npm pack`: with a Prepare hook the package's lifecycle
// scripts are skipped (the hook owns the build), without one npm runs them,
// as npm publish does.
func TestPackRunsNpmPackAndServesTheTarball(t *testing.T) {
	npmOrSkip(t)
	var prepared atomic.Int32
	o, dir, log := startOverlay(t, "http://127.0.0.1:1", func(p *Package) {
		p.Prepare = func() error { prepared.Add(1); return nil }
	})
	// A prepack script that would leave a marker if npm ran it.
	manifest := `{"name":"@acme/lib","version":"1.2.0","main":"index.js","files":["index.js"],"scripts":{"prepack":"node -e \"require('fs').writeFileSync('prepack-ran','')\""}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.Pack(); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if err := o.Pack(); err != nil {
		t.Fatalf("second Pack: %v", err)
	}
	if prepared.Load() != 1 {
		t.Errorf("Prepare ran %d times, want once however often Pack is called", prepared.Load())
	}
	if _, err := os.Stat(filepath.Join(dir, "prepack-ran")); err == nil {
		t.Error("npm ran the prepack script although Prepare owns the build")
	}
	if !strings.Contains(log.String(), "packed @acme/lib@1.2.0") {
		t.Errorf("pack not logged: %q", log.String())
	}
	lp := o.packages["@acme/lib"]
	status, body := get(t, o.URL+"-/tarballs/@acme%2flib/"+lp.file)
	if status != http.StatusOK || !bytes.Equal(body, lp.tarball) || len(body) == 0 {
		t.Errorf("tarball: HTTP %d, %d bytes", status, len(body))
	}
	if m, err := tarballManifest(body); err != nil || m["version"] != "1.2.0" {
		t.Errorf("served tarball manifest = %v, %v", m, err)
	}
}

func TestPackRunsLifecycleScriptsWithoutAPrepareHook(t *testing.T) {
	npmOrSkip(t)
	o, dir, _ := startOverlay(t, "http://127.0.0.1:1", nil)
	manifest := `{"name":"@acme/lib","version":"1.2.0","main":"index.js","files":["index.js"],"scripts":{"prepack":"node -e \"require('fs').writeFileSync('prepack-ran','')\""}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := o.Pack(); err != nil {
		t.Fatalf("Pack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prepack-ran")); err != nil {
		t.Errorf("npm pack did not run the prepack script: %v", err)
	}
}

// A package.json edited after Start changes what npm pack produces; the
// overlay refuses the result instead of serving it under the promised name.
func TestPackRefusesAPackageThatChangedAfterStart(t *testing.T) {
	npmOrSkip(t)
	o, dir, _ := startOverlay(t, "http://127.0.0.1:1", nil)
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"@acme/lib","version":"9.9.9","files":["index.js"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := o.Pack()
	if err == nil || !strings.Contains(err.Error(), "did package.json change after the overlay started") {
		t.Fatalf("Pack = %v, want the changed-package refusal", err)
	}
}

// When npm itself fails, its output is part of the error: that is the only
// place a build failure is explained.
func TestPackReportsNpmFailureWithItsOutput(t *testing.T) {
	npmOrSkip(t)
	o, dir, _ := startOverlay(t, "http://127.0.0.1:1", nil)
	if err := os.Remove(filepath.Join(dir, "package.json")); err != nil {
		t.Fatal(err)
	}
	err := o.Pack()
	if err == nil || !strings.Contains(err.Error(), "npm pack --pack-destination") || !strings.Contains(err.Error(), dir) {
		t.Fatalf("Pack = %v, want the npm command and package directory named", err)
	}
}
