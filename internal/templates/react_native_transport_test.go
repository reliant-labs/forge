package templates

import (
	"regexp"
	"strings"
	"testing"
)

// Server-streaming RPCs only work on Hermes if the transport reads a streaming
// Response.body. React Native's global fetch buffers, so the native transport
// must hand connect-web expo/fetch explicitly. The native Jest test
// (react-native/tests/connect-transport.test.ts) drives the rendered transport;
// this pins the wiring that test cannot see from inside the app.
func TestReactNativeTransportUsesExpoFetch(t *testing.T) {
	for _, workspaces := range []bool{false, true} {
		data := FrontendTemplateData{
			FrontendName: "mobile", ProjectName: "fixture", Platform: "react-native",
			Workspaces: workspaces, APIPackage: "@fixture/api", HooksPackage: "@fixture/hooks",
		}
		body, err := FrontendTemplates().Render("react-native/src/lib/connect.ts.tmpl", data)
		if err != nil {
			t.Fatal(err)
		}
		src := string(body)

		for name, pattern := range map[string]string{
			"expo/fetch import":                 `import \{ fetch as expoFetch \} from "expo/fetch";`,
			"connect-web transport":             `import \{ createConnectTransport \} from "@connectrpc/connect-web";`,
			"runtime interceptors entry":        `from "@reliantlabs/forge-web-runtime/interceptors"`,
			"fetch handed to the transport":     `(?s)createConnectTransport\(\{[^}]*\bfetch: fetchImpl,[^}]*\}\)`,
			"interceptors handed to it":         `(?s)createConnectTransport\(\{[^}]*\binterceptors,[^}]*\}\)`,
			"base url handed to it":             `(?s)createConnectTransport\(\{[^}]*\bbaseUrl,[^}]*\}\)`,
			"injectable fetch defaults to expo": `fetchImpl: typeof globalThis\.fetch = expoFetch`,
			"production transport":              `export const transport = createTransport\(\);`,
			"base url from env":                 `process\.env\.EXPO_PUBLIC_API_URL \?\? DEV_API_URL`,
			"token getter seam":                 `export function setAuthTokenGetter`,
			"token getter reaches the stack":    `getToken: \(\) => _getToken\(\)`,
		} {
			if !regexp.MustCompile(pattern).MatchString(src) {
				t.Errorf("workspaces=%v: %s missing (/%s/) in:\n%s", workspaces, name, pattern, src)
			}
		}
		if strings.Contains(src, "globalThis.fetch,") && !strings.Contains(src, "typeof globalThis.fetch") {
			t.Errorf("workspaces=%v: transport must not default to the global fetch", workspaces)
		}
		if got := strings.Contains(src, "setApiTransport(transport)"); got != workspaces {
			t.Errorf("workspaces=%v: setApiTransport wiring = %v", workspaces, got)
		}
	}
}

// expo/fetch is a subpath of the expo package the scaffold already depends on,
// so the transport needs no extra dependency, and the Jest test ships beside
// the other owned native tests (outside app/, where it would become a route).
func TestReactNativeTransportTestShipsAndNeedsNoNewDependency(t *testing.T) {
	data := FrontendTemplateData{FrontendName: "mobile", ProjectName: "fixture", Platform: "react-native"}
	pkg, err := FrontendTemplates().Render("react-native/package.json.tmpl", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pkg), `"expo": "~58.`) {
		t.Error("package.json must depend on the expo package that provides expo/fetch")
	}

	files, err := ListFrontendTree("react-native")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range files {
		if f.Rel != "tests/connect-transport.test.ts" {
			continue
		}
		found = true
		body, err := FrontendTemplates().Render(f.Path, data)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{`from "../src/lib/connect"`, "ReadableStream", "AbortController", `jest.mock("expo/fetch"`} {
			if !strings.Contains(string(body), want) {
				t.Errorf("transport test no longer contains %q", want)
			}
		}
	}
	if !found {
		t.Error("tests/connect-transport.test.ts is not in the composed react-native tree")
	}

	// The app tests that stub globalThis.fetch only work because a Jest setup
	// file maps `expo/fetch` onto the global; the transport test opts out of
	// that mapping with its own factory so production stays pinned to expo/fetch.
	have := map[string]bool{}
	for _, f := range files {
		have[f.Rel] = true
	}
	for _, rel := range []string{"tests/setup/expo-fetch.ts", "tests/fetch-stub.test.ts"} {
		if !have[rel] {
			t.Errorf("composed react-native tree is missing %s", rel)
		}
	}
	jestCfg, err := FrontendTemplates().Render("react-native/jest.config.cjs", data)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(jestCfg), `setupFiles: ["<rootDir>/tests/setup/expo-fetch.ts"]`) {
		t.Error("jest.config.cjs must wire tests/setup/expo-fetch.ts as a setup file")
	}
	if strings.Contains(string(jestCfg), "...expo.setupFiles") {
		t.Error("jest appends setupFiles to the preset's; spreading the preset's duplicates them")
	}
}
