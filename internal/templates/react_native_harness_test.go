package templates

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// Test the actual JavaScript mapping, including pnpm's nested node_modules.
func TestNativeJestTransformGuards(t *testing.T) {
	if testing.Short() {
		t.Skip("executes node; runs in task test")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	content, err := FrontendTemplates().Render("react-native/jest.config.cjs", FrontendTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	script := `const vm = require('vm'), assert = require('assert');
const original = ['/node_modules/(?!((jest-)?react-native|@react-native(-community)?)|expo(nent)?|@expo(nent)?/.*)', '/node_modules/react-native-reanimated/plugin/'];
const sandbox = {module:{exports:{}}, require:()=>({transformIgnorePatterns:original})};
vm.runInNewContext(process.argv[1], sandbox);
const patterns = sandbox.module.exports.transformIgnorePatterns;
assert.equal(patterns[1], original[1]);
for (const pkg of ['@reliantlabs/forge-web-runtime/dist/service-hooks.js', '@connectrpc/connect-web/dist/esm/index.js', '@bufbuild/protobuf/dist/esm/index.js', 'react-native/index.js', 'expo/src/Expo.ts']) {
 for (const prefix of ['/fixture/node_modules/', '/fixture/node_modules/.pnpm/pkg@1/node_modules/']) {
  assert(!patterns.some(p=>new RegExp(p).test(prefix+pkg)), prefix+pkg);
 }
}
// @connectrpc/connect ships CJS and must NOT be transformed: Babel-transforming
// it breaks the runtime's ConnectClientError subclass ("Cannot call a class as a
// function"), turning every error-path test into Code.Unknown.
for (const path of ['/fixture/node_modules/lodash/index.js', '/fixture/node_modules/.pnpm/lodash@1/node_modules/lodash/index.js', '/fixture/node_modules/react-native-reanimated/plugin/index.js', '/fixture/node_modules/@connectrpc/connect/dist/cjs/index.js', '/fixture/node_modules/.pnpm/pkg@1/node_modules/@connectrpc/connect/dist/cjs/index.js']) {
 assert(patterns.some(p=>new RegExp(p).test(path)), path);
}
`
	if out, err := exec.Command(node, "-e", script, string(content)).CombinedOutput(); err != nil {
		t.Fatalf("transform guards: %v\n%s", err, out)
	}
}

// Exercise the composed tree in both layouts: browser test/lint infrastructure
// must not leak into native, and tests must never become Expo Router routes.
func TestNativeHarnessComposes(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		data := FrontendTemplateData{FrontendName: "mobile", ProjectName: "fixture", Platform: "react-native", Workspaces: workspace, APIPackage: "@fixture/api", HooksPackage: "@fixture/hooks", UINativePackage: "@fixture/ui-native"}
		files, err := ListFrontendTree("react-native")
		if err != nil {
			t.Fatal(err)
		}
		rendered := map[string]string{}
		for _, file := range files {
			content, err := FrontendTemplates().Render(file.Path, data)
			if err != nil {
				t.Fatal(err)
			}
			rendered[file.Rel] = string(content)
			if strings.HasPrefix(file.Rel, "app/") && (strings.Contains(file.Rel, ".test.") || strings.Contains(file.Rel, "__tests__")) {
				t.Errorf("test becomes a route: %s", file.Rel)
			}
			if strings.Contains(file.Rel, "vitest") {
				t.Errorf("browser harness in native tree: %s", file.Rel)
			}
		}
		for name, fragments := range map[string][]string{
			"eslint.config.cjs":         {`require("eslint-config-expo/flat")`, "...expo"},
			"jest.config.cjs":           {`preset: "jest-expo"`, `require("jest-expo/jest-preset")`, `expo.transformIgnorePatterns.map`, `setupFiles: ["<rootDir>/tests/setup/expo-fetch.ts"]`, `@reliantlabs/forge-web-runtime/|@connectrpc/connect-web/|@bufbuild/protobuf/`, `\\.pnpm/|${esmPackages}`, `testMatch: ["<rootDir>/tests/**/*.test.[jt]s?(x)"]`, "watch: false", "maxWorkers: 2"},
			"tests/setup/expo-fetch.ts": {`jest.mock("expo/fetch"`, `globalThis.fetch(...args)`},
			"tests/app-config.test.ts":  {`from "@jest/globals"`, `from "../app.json"`, `from "../package.json"`, `expect(manifest.main)`, `[app.expo.scheme].flat()`, `expect(schemes).toContain(app.expo.ios.bundleIdentifier)`},
		} {
			content := rendered[name]
			for _, fragment := range append(fragments, "Yours to own", "forge will not overwrite this file") {
				if !strings.Contains(content, fragment) {
					t.Errorf("workspace=%v %s missing %q", workspace, name, fragment)
				}
			}
			if strings.Contains(content, "DO NOT EDIT") {
				t.Errorf("%s claims generated ownership", name)
			}
		}
		var manifest struct{ Scripts, Dependencies, DevDependencies map[string]string }
		if err := json.Unmarshal([]byte(rendered["package.json.tmpl"]), &manifest); err != nil {
			t.Fatal(err)
		}
		for key, want := range map[string]string{"lint": "eslint .", "test": "jest", "typecheck": "tsc --noEmit"} {
			if got := manifest.Scripts[key]; got != want {
				t.Errorf("%s script = %q, want %q", key, got, want)
			}
		}
		for key, want := range map[string]string{"eslint": "^9.25.0", "eslint-config-expo": "~58.0.4", "jest": "^30.0.0", "jest-expo": "~58.0.14", "@jest/globals": "^30.0.0", "@react-native/jest-preset": "~0.88.1", "@types/react": "~19.3.0", "babel-preset-expo": "~58.0.25", "typescript": "~6.0.3", "prettier": PrettierVersion} {
			if got := manifest.DevDependencies[key]; got != want {
				t.Errorf("%s = %q, want %q", key, got, want)
			}
		}
		if manifest.Dependencies["@babel/runtime"] != "^7.25.0" {
			t.Error("native Babel-generated helpers require a direct runtime dependency under isolated pnpm")
		}
		if manifest.Dependencies["react"] != "19.3.0" || manifest.Dependencies["expo"] != "~58.0.24" {
			t.Error("native SDK58 matrix changed")
		}
	}
}
