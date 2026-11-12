package templates

import (
	"regexp"
	"strings"
	"testing"
)

// The behaviour of query-lifecycle.ts is proven by its Jest test
// (react-native/tests/query-lifecycle.test.ts), which the mobile consumer
// conformance fixture runs. These checks pin what Go can see: the files ship in
// the composed native tree, the root layout wires the adapter, the native
// adapter does not lean on DOM listeners, and the Expo module it needs is
// declared at the SDK 58 bundled version.
func TestNativeQueryLifecycleAdapters(t *testing.T) {
	files, err := ListFrontendTree("react-native")
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, f := range files {
		have[f.Rel] = true
	}
	for _, rel := range []string{"src/lib/query-lifecycle.ts", "tests/query-lifecycle.test.ts"} {
		if !have[rel] {
			t.Errorf("composed react-native tree is missing %s", rel)
		}
	}

	render := func(name string) string {
		t.Helper()
		b, err := FrontendTemplates().Render(name, FrontendTemplateData{FrontendName: "mobile", ProjectName: "demo"})
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return string(b)
	}

	adapter := render("react-native/src/lib/query-lifecycle.ts")
	for _, want := range []string{
		"focusManager.setFocused(",
		"onlineManager.setOnline(",
		`AppState.addEventListener("change"`,
		"Network.addNetworkStateListener",
		"Symbol.for(",
	} {
		if !strings.Contains(adapter, want) {
			t.Errorf("query-lifecycle.ts lost %q", want)
		}
	}
	// The mutation write-safety default lives in the SHARED client (web +
	// native), not in this adapter: the adapter must never rewrite the client's
	// defaults, and must say that offline queries report fetchStatus "paused".
	if strings.Contains(adapter, "setDefaultOptions") {
		t.Error("query-lifecycle.ts must not mutate the client's default options; mutations networkMode belongs to shared/src/lib/query-client.ts")
	}
	for _, want := range []string{`fetchStatus: "paused"`, "SHARED query-client"} {
		if !strings.Contains(adapter, want) {
			t.Errorf("query-lifecycle.ts header no longer documents %q", want)
		}
	}
	for _, banned := range []string{"window.addEventListener", "document.addEventListener", "document.visibilityState"} {
		if strings.Contains(adapter, banned) {
			t.Errorf("query-lifecycle.ts must not use DOM listeners (%q)", banned)
		}
	}

	layout := render("react-native/app/_layout.tsx.tmpl")
	for _, want := range []string{
		`from "../src/lib/query-lifecycle"`,
		"useEffect(() => setupQueryLifecycle(queryClient), [])",
	} {
		if !strings.Contains(layout, want) {
			t.Errorf("_layout.tsx lost %q", want)
		}
	}

	// expo-network ~58.0.5 is the published expo@58.0.24 bundledNativeModules.json pin.
	for _, workspace := range []bool{false, true} {
		m := nativeLifecycleManifest(t, "react-native", workspace)
		if got, want := m.Dependencies["expo-network"], "~58.0.5"; got != want {
			t.Errorf("workspace=%v: expo-network = %q, want %q", workspace, got, want)
		}
	}
}

// Write safety is the shared client's decision, so it covers web and native
// alike: a mutation fired offline must run and fail, never pause and replay.
func TestSharedQueryClientMutationsNeverPauseAndReplay(t *testing.T) {
	t.Parallel()
	raw, err := FrontendTemplates().Get("shared/src/lib/query-client.ts")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	mutations := regexp.MustCompile(`(?s)mutations:\s*\{(.*?)\n    \},`).FindStringSubmatch(src)
	if mutations == nil {
		t.Fatal("query-client.ts has no defaultOptions.mutations block")
	}
	for _, want := range []string{"retry: 0", `networkMode: "always"`, "pause"} {
		if !strings.Contains(mutations[1], want) {
			t.Errorf("mutations defaults lost %q:\n%s", want, mutations[1])
		}
	}
	if !strings.Contains(src, `fetchStatus: "paused"`) {
		t.Error(`query-client.ts header must document that offline queries report fetchStatus "paused"`)
	}
}
