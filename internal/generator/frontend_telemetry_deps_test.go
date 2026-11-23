package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/webruntimepeers"
)

// writeFrontendSource writes one source file under frontends/<feName>.
func writeFrontendSource(t *testing.T, projectDir, feName, rel, body string) {
	t.Helper()
	p := filepath.Join(projectDir, "frontends", feName, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const telemetryImport = `import { initBrowserTelemetry } from "@reliantlabs/forge-web-runtime/telemetry";` + "\n"

// peersRuntimeRange is the first runtime range that takes the telemetry SDK as
// peers. The scaffold floor stays below it until npm serves that runtime (the
// release bookkeeping raises it), so these tests name it rather than the floor.
var peersRuntimeRange = "^" + strings.TrimPrefix(telemetryPeersSince, "v")

// runtimeEntry is a manifest "dependencies" entry for the runtime at spec.
func runtimeEntry(spec string) string {
	return "\n    \"" + WebRuntimePackage + "\": \"" + spec + "\""
}

// A runtime older than 0.14 carries the SDK as its own dependencies, so a
// frontend that holds its runtime there (a disowned or hand-pinned manifest)
// keeps the manifest it wrote, with no notice either, even though its
// sources import ./telemetry. A 0.14 range, or a dev `file:` bridge to a
// runtime that declares the peers, gets them.
func TestEnsureTelemetryDependencies_OnlyForARuntimeThatTakesThePeers(t *testing.T) {
	feRel := filepath.Join("frontends", "web")
	rrweb, _ := webruntimepeers.PeerSpec("rrweb")
	for name, tc := range map[string]struct {
		runtime string
		want    bool
	}{
		"^0.13 with its own SDK version": {runtime: "^0.13.21", want: false},
		"exact 0.13":                     {runtime: "0.13.20", want: false},
		">=0.13 floor":                   {runtime: ">=0.13.0", want: false},
		"git URL":                        {runtime: "github:reliant-labs/forge#main", want: false},
		"^0.14":                          {runtime: "^0.14.0", want: true},
		"~0.14.2":                        {runtime: "~0.14.2", want: true},
		"file: bridge with peers":        {runtime: "file:../../runtime-peers", want: true},
		"file: bridge without peers":     {runtime: "file:../../runtime-old", want: false},
	} {
		t.Run(name, func(t *testing.T) {
			projectDir := filepath.Join(t.TempDir(), "app")
			for dir, manifest := range map[string]string{
				"runtime-peers": `{"name":"` + WebRuntimePackage + `","version":"0.14.0","peerDependencies":{"rrweb":"` + rrweb + `"}}`,
				"runtime-old":   `{"name":"` + WebRuntimePackage + `","version":"0.13.21","dependencies":{"rrweb":"2.1.7"}}`,
			} {
				if err := os.MkdirAll(filepath.Join(projectDir, dir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(projectDir, dir, "package.json"), []byte(manifest), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			manifest := writeFrontendManifest(t, projectDir, "web", runtimeEntry(tc.runtime)+",\n    \"rrweb\": \"2.0.0\"")
			writeFrontendSource(t, projectDir, "web", "src/lib/otel_gen.ts", telemetryImport)
			before, _ := os.ReadFile(manifest)
			notices := strings.Join(FrontendDependencyNotices(projectDir, feRel), "\n")
			changed := EnsureTelemetryDependencies(projectDir, feRel)
			if changed != tc.want {
				t.Fatalf("changed = %v, want %v", changed, tc.want)
			}
			got := readDeps(t, manifest)
			if !tc.want {
				after, _ := os.ReadFile(manifest)
				if string(before) != string(after) {
					t.Errorf("manifest changed:\n%s", after)
				}
				if strings.Contains(notices, "rrweb") || strings.Contains(notices, "otel-web") {
					t.Errorf("a runtime that carries the SDK got a telemetry notice:\n%s", notices)
				}
				return
			}
			if got["rrweb"] != rrweb {
				t.Errorf("rrweb = %q, want the runtime's peer %q", got["rrweb"], rrweb)
			}
			if !strings.Contains(notices, "rrweb") {
				t.Errorf("a disowned manifest on this runtime gets no rrweb notice:\n%s", notices)
			}
		})
	}
}

// The runtime's telemetry SDK parts are exact optional peers (forge defect
// ae15c269), so a frontend importing ./telemetry must declare them, and
// generate declares them: missing ones are added, a different exact version
// is replaced (npm refuses any other beside the runtime), and a second run
// changes nothing.
func TestEnsureTelemetryDependencies_DeclaresTheRuntimesExactPeers(t *testing.T) {
	feRel := filepath.Join("frontends", "web")
	want := map[string]string{}
	for _, name := range []string{"@hyperdx/otel-web", "rrweb"} {
		spec, ok := webruntimepeers.PeerSpec(name)
		if !ok {
			t.Fatalf("the runtime no longer declares %s as a peer", name)
		}
		want[name] = spec
	}
	for name, extra := range map[string]string{
		"none declared":  "\n    \"react\": \"^19.1.0\"",
		"one stale":      "\n    \"rrweb\": \"2.0.0\",\n    \"react\": \"^19.1.0\"",
		"empty object":   "",
		"both declared":  "\n    \"@hyperdx/otel-web\": \"" + want["@hyperdx/otel-web"] + "\",\n    \"rrweb\": \"" + want["rrweb"] + "\"",
		"adopted import": "\n    \"react\": \"^19.1.0\"",
	} {
		t.Run(name, func(t *testing.T) {
			projectDir := filepath.Join(t.TempDir(), "app")
			deps := runtimeEntry(peersRuntimeRange)
			if extra != "" {
				deps += "," + extra
			}
			manifest := writeFrontendManifest(t, projectDir, "web", deps)
			src := "src/lib/otel_gen.ts"
			if name == "adopted import" {
				src = "app/telemetry/boot.tsx"
			}
			writeFrontendSource(t, projectDir, "web", src, telemetryImport)

			changed := EnsureTelemetryDependencies(projectDir, feRel)
			if name == "both declared" && changed {
				t.Error("rewrote a manifest that already declares both peers")
			}
			got := readDeps(t, manifest)
			for dep, spec := range want {
				if got[dep] != spec {
					t.Errorf("%s = %q, want %q", dep, got[dep], spec)
				}
			}
			if strings.Contains(extra, "react") && got["react"] != "^19.1.0" {
				t.Errorf("react = %q, an unrelated entry must survive", got["react"])
			}
			before, _ := os.ReadFile(manifest)
			if EnsureTelemetryDependencies(projectDir, feRel) {
				t.Error("second run rewrote the manifest")
			}
			after, _ := os.ReadFile(manifest)
			if string(before) != string(after) {
				t.Error("second run changed the manifest")
			}
			if notices := FrontendDependencyNotices(projectDir, feRel); len(notices) != 0 {
				for _, n := range notices {
					if strings.Contains(n, "otel-web") || strings.Contains(n, "rrweb") {
						t.Errorf("a reconciled manifest still gets a notice: %s", n)
					}
				}
			}
		})
	}
}

// A frontend that never imports ./telemetry (Expo, an adopted app without
// telemetry) is the consumer the optional peers exist for: it gets none.
// Build output and dependencies are not its sources.
func TestEnsureTelemetryDependencies_LeavesAFrontendWithoutTelemetryAlone(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "app")
	manifest := writeFrontendManifest(t, projectDir, "web", "\n    \"react\": \"^19.1.0\"")
	writeFrontendSource(t, projectDir, "web", "src/main.tsx", `import "@reliantlabs/forge-web-runtime";`+"\n")
	writeFrontendSource(t, projectDir, "web", "node_modules/x/index.js", telemetryImport)
	writeFrontendSource(t, projectDir, "web", "dist/assets/index.js", telemetryImport)
	before, _ := os.ReadFile(manifest)
	if EnsureTelemetryDependencies(projectDir, filepath.Join("frontends", "web")) {
		t.Fatal("declared the telemetry SDK in a frontend that never imports ./telemetry")
	}
	after, _ := os.ReadFile(manifest)
	if string(before) != string(after) {
		t.Fatalf("manifest changed:\n%s", after)
	}
}

// A disowned manifest gets notices instead of edits.
func TestFrontendDependencyNotices_NameMissingTelemetryPeers(t *testing.T) {
	projectDir := filepath.Join(t.TempDir(), "app")
	writeFrontendManifest(t, projectDir, "web", "\n    \""+WebRuntimePackage+"\": \""+peersRuntimeRange+"\"")
	writeFrontendSource(t, projectDir, "web", "src/lib/otel_gen.ts", telemetryImport)
	notices := strings.Join(FrontendDependencyNotices(projectDir, filepath.Join("frontends", "web")), "\n")
	for _, name := range []string{"@hyperdx/otel-web", "rrweb"} {
		spec, _ := webruntimepeers.PeerSpec(name)
		if !strings.Contains(notices, "npm install "+name+"@"+spec) {
			t.Errorf("no notice asks for %s@%s:\n%s", name, spec, notices)
		}
	}
}
