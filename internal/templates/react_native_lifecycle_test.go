package templates

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func nativeLifecycleManifest(t *testing.T, kind string, workspace bool) struct {
	Scripts      map[string]string
	Dependencies map[string]string
} {
	t.Helper()
	b, err := FrontendTemplates().Render(kind+"/package.json.tmpl", FrontendTemplateData{FrontendName: "mobile", ProjectName: "demo", Workspaces: workspace, APIPackage: "@demo/api", HooksPackage: "@demo/hooks", UINativePackage: "@demo/ui-native"})
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Scripts      map[string]string
		Dependencies map[string]string
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestNativeLifecycleManifest(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		m := nativeLifecycleManifest(t, "react-native", workspace)
		for name, want := range map[string][]string{
			"start": {"expo", "start"}, "ios": {"expo", "start", "--ios"}, "android": {"expo", "start", "--android"}, "web": {"expo", "start", "--web"},
			"dev": {"node", "expo-dev.cjs"}, "build": {"expo", "export", "--platform", "web", "--output-dir", "dist"},
		} {
			if got := strings.Fields(m.Scripts[name]); !reflect.DeepEqual(got, want) {
				t.Errorf("%s argv = %q, want %q", name, got, want)
			}
		}
		for _, web := range []string{"nextjs", "vite-spa"} {
			if got, want := m.Dependencies["@reliantlabs/forge-web-runtime"], nativeLifecycleManifest(t, web, false).Dependencies["@reliantlabs/forge-web-runtime"]; got != want {
				t.Errorf("runtime range %s != %s %s", got, web, want)
			}
		}
		// SDK 52's web renderer is React DOM 18.3.1, compatible with the
		// existing React 18 range; do not rely on a transitive peer install.
		for name, want := range map[string]string{"expo": "~52.0.0", "react": "^18.3.0", "react-dom": "18.3.1", "react-native": "0.76.9", "react-native-web": "~0.19.0"} {
			if got := m.Dependencies[name]; got != want {
				t.Errorf("SDK 52 dependency %s = %q, want %q", name, got, want)
			}
		}
	}
	b, err := FrontendTemplates().Render("react-native/app.json.tmpl", FrontendTemplateData{FrontendName: "mobile", ProjectName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	var app struct {
		Expo struct {
			Web struct{ Bundler, Output string }
		}
	}
	if err := json.Unmarshal(b, &app); err != nil {
		t.Fatal(err)
	}
	if app.Expo.Web.Bundler != "metro" || app.Expo.Web.Output != "static" {
		t.Fatalf("web export requires Metro static HTML, got %+v", app.Expo.Web)
	}
	files, err := ListFrontendTree("react-native")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Rel == "expo-dev.cjs" {
			return
		}
	}
	t.Fatal("dev entrypoint missing from composed scaffold")
}

// The assertions execute the actual owned entrypoint against a fake Expo module,
// not a substring approximation of shell/Node argv semantics. No Expo install,
// Metro listener, device or native packaging is involved.
func TestNativeLifecycleDevEntrypoint(t *testing.T) {
	if testing.Short() {
		t.Skip("executes Node; runs in full scoped tests")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	root := t.TempDir()
	source, err := FrontendTemplates().Render("react-native/expo-dev.cjs", FrontendTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("expo-dev.cjs", string(source))
	write("node_modules/expo/bin/cli.js", `console.log(JSON.stringify(process.argv.slice(2))); process.exitCode = Number(process.env.FAKE_EXPO_EXIT || 0);`)
	// Pure builder cases include flag conflicts, invalid environment values and
	// literal arguments that a shell wrapper could accidentally interpolate.
	write("check.cjs", `const assert = require('node:assert/strict');
const {devArgs} = require('./expo-dev.cjs');
assert.deepEqual(devArgs([], undefined), ['start']);
assert.deepEqual(devArgs(['--port','9000'], ''), ['start','--port','9000']);
assert.deepEqual(devArgs(['--host','localhost','--clear','a b;$HOME'], '4321'), ['start','--port','4321','--host','localhost','--clear','a b;$HOME']);
for (const port of ['0','65536','abc','1.5','-1',' 42','42;echo']) assert.throws(() => devArgs([], port), /PORT/);
for (const args of [['--port','9000'],['--port=9000'],['-p','9000'],['-p9000'],['-p=9000']]) assert.throws(() => devArgs(args,'4321'), /PORT/);
`)
	if out, err := exec.Command(node, filepath.Join(root, "check.cjs")).CombinedOutput(); err != nil {
		t.Fatalf("argv contracts: %v\n%s", err, out)
	}
	for _, code := range []string{"0", "23"} {
		cmd := exec.Command(node, filepath.Join(root, "expo-dev.cjs"), "--host", "localhost", "a b;$HOME")
		// Avoid inheriting PORT/FAKE_EXPO_EXIT duplicates from the host.
		for _, e := range os.Environ() {
			if !strings.HasPrefix(e, "PORT=") && !strings.HasPrefix(e, "FAKE_EXPO_EXIT=") {
				cmd.Env = append(cmd.Env, e)
			}
		}
		cmd.Env = append(cmd.Env, "PORT=4321", "FAKE_EXPO_EXIT="+code)
		out, err := cmd.CombinedOutput()
		if code == "0" && err != nil {
			t.Fatal(err)
		}
		if code == "23" {
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 23 {
				t.Fatalf("exit not preserved: %v", err)
			}
		}
		var got []string
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		want := []string{"start", "--port", "4321", "--host", "localhost", "a b;$HOME"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("argv = %q, want %q", got, want)
		}
	}
}
