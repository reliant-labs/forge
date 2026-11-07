package templates

import (
	"regexp"
	"strings"
	"testing"
)

// Expo inlines EXPO_PUBLIC_* only for a literal `process.env.NAME` property
// read; a dynamic lookup silently sees nothing in a Debug bundle.
func TestNativeDevlogReadsOriginEnvStatically(t *testing.T) {
	raw, err := FrontendTemplates().Get("react-native/src/lib/devlog.ts")
	if err != nil {
		t.Fatal(err)
	}
	code := stripLineComments(string(raw))
	if !strings.Contains(code, "process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN") {
		t.Error("devlog.ts must read process.env.EXPO_PUBLIC_FORGE_DEVLOG_ORIGIN as a static property access")
	}
	for _, dynamic := range []string{`process.env[`, `= process.env;`, `...process.env`, `env[`} {
		if strings.Contains(code, dynamic) {
			t.Errorf("devlog.ts must not read process.env dynamically (%q): Expo only inlines the static form", dynamic)
		}
	}
}

// normalizeError reports "Unknown error" for null / non-Error values where the
// previous userMessage helper produced the generic retry copy; both shared
// helpers map it back, and still import only the portable /interceptors entry.
func TestNativeSharedHelpersKeepGenericErrorCopy(t *testing.T) {
	for _, path := range []string{"shared/src/lib/query-client.ts", "shared/src/hooks/use-optimistic-mutation.ts"} {
		raw, err := FrontendTemplates().Get(path)
		if err != nil {
			t.Fatal(err)
		}
		src := string(raw)
		if !strings.Contains(src, `"Something went wrong. Please try again."`) || !strings.Contains(src, `"Unknown error"`) {
			t.Errorf("%s must map normalizeError's \"Unknown error\" to the generic retry copy", path)
		}
		if regexp.MustCompile(`normalizeError\([^)]*\)\.message\s*\|\|`).MatchString(src) {
			t.Errorf("%s uses normalizeError(...).message || fallback, which lets \"Unknown error\" through", path)
		}
		if regexp.MustCompile(`from "@reliantlabs/forge-web-runtime"`).MatchString(src) {
			t.Errorf("%s imports the runtime root barrel; use /interceptors", path)
		}
	}
}
