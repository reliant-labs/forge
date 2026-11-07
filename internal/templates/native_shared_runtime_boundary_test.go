package templates

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This is a source boundary guard, not a claim about Metro tree shaking or
// Hermes execution. Explicit and inline type imports erase; value imports do not.
var nativeRuntimeImport = regexp.MustCompile(`(?m)^import\s+(type\s+)?([^;]+?)\s+from\s+["'](@reliantlabs/forge-web-runtime(?:/[^"']+)?)["']`)

func nativeRuntimeImportIsTypeOnly(explicit, bindings string) bool {
	if strings.TrimSpace(explicit) == "type" {
		return true
	}
	bindings = strings.TrimSpace(bindings)
	if !strings.HasPrefix(bindings, "{") || !strings.HasSuffix(bindings, "}") {
		return false
	}
	seen := false
	for _, binding := range strings.Split(strings.Trim(bindings, "{}"), ",") {
		binding = strings.TrimSpace(binding)
		if binding == "" {
			continue
		}
		seen = true
		if !strings.HasPrefix(binding, "type ") {
			return false
		}
	}
	return seen
}

func TestNativeRuntimeImportClassification(t *testing.T) {
	for _, tc := range []struct {
		source string
		types  bool
	}{
		{`import type { ConnectClientError } from "@reliantlabs/forge-web-runtime";`, true},
		{`import { type ConnectClientError } from "@reliantlabs/forge-web-runtime";`, true},
		{`import { userMessage, type ConnectClientError } from "@reliantlabs/forge-web-runtime";`, false},
		{`import * as runtime from "@reliantlabs/forge-web-runtime";`, false},
		{`import runtime from "@reliantlabs/forge-web-runtime";`, false},
	} {
		m := nativeRuntimeImport.FindStringSubmatch(tc.source)
		if m == nil {
			t.Fatalf("did not parse %s", tc.source)
		}
		if got := nativeRuntimeImportIsTypeOnly(m[1], m[2]); got != tc.types {
			t.Errorf("%s: type-only = %v, want %v", tc.source, got, tc.types)
		}
	}
}

func TestNativeSharedHelpersUsePortableRuntime(t *testing.T) {
	files, err := filepath.Glob("frontend/shared/src/hooks/*.ts")
	if err != nil || len(files) == 0 {
		t.Fatalf("discover shared hooks: %v (%d files)", err, len(files))
	}
	files = append(files, "frontend/shared/src/lib/query-client.ts")
	values, types := 0, 0
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range nativeRuntimeImport.FindAllStringSubmatch(string(raw), -1) {
			kind := "runtime value"
			if nativeRuntimeImportIsTypeOnly(m[1], m[2]) {
				kind = "erased type-only declaration"
				types++
			} else {
				values++
			}
			// Published 0.13.1–0.13.5 lack this export. Do not break newly
			// scaffolded apps before the additive runtime export is published.
			// This explicit debt allowlist must disappear with that floor bump.
			if m[3] == "@reliantlabs/forge-web-runtime" && strings.TrimSpace(m[2]) == "{ userMessage }" &&
				(file == "frontend/shared/src/lib/query-client.ts" || file == "frontend/shared/src/hooks/use-optimistic-mutation.ts") {
				continue
			}
			if m[3] != "@reliantlabs/forge-web-runtime/interceptors" {
				t.Errorf("%s: %s import uses %s instead of the portable /interceptors entry", file, kind, m[3])
			}
		}
	}
	if values < 2 || types < 5 {
		t.Fatalf("guard missed expected shared helper imports: %d values, %d types", values, types)
	}
}

// Pin the actual public subpath, rather than accepting a syntactically narrower
// import that refers to a symbol the package does not export.
func TestNativeSharedHelpersPortableExports(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "web-runtime/src/interceptors.ts"))
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile(`(?s)export\s*\{([^}]+)\}\s*from\s*"\./errors\.js"`).FindAllStringSubmatch(string(raw), -1)
	for _, symbol := range []string{"userMessage", "ConnectClientError"} {
		found := false
		for _, block := range blocks {
			for _, name := range strings.Split(block[1], ",") {
				found = found || strings.TrimSpace(name) == symbol
			}
		}
		if !found {
			t.Errorf("/interceptors must re-export the existing errors.js %s implementation", symbol)
		}
	}
}
