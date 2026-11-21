package generator

// The browser-telemetry SDK a frontend that imports
// @reliantlabs/forge-web-runtime/telemetry must declare itself.
//
// web-runtime 0.13.19 through 0.13.x carried @hyperdx/otel-web and rrweb as
// its own DEPENDENCIES. npm installs a dependency into every consumer whether
// or not an entry it imports reaches it, so a consumer of './node' alone (an
// Electron main process) installed about 90 MB of browser packages for
// nothing (forge defect ae15c269). From 0.14.0 both are exact OPTIONAL peers:
// only './telemetry' imports them, and the app that imports it supplies them.
//
// So forge declares them in every frontend whose sources import
// './telemetry': each web scaffold renders them into package.json, and
// `forge generate` adds them to an existing frontend (scaffolded or adopted)
// on the run that raises its runtime floor to 0.14, before any install can
// resolve a runtime without them. Declaring them against an older runtime is
// harmless: it carries the same exact versions as dependencies, which npm and
// pnpm dedupe onto the app's copy.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/webruntimepeers"
)

// webRuntimeTelemetryEntry is the runtime subpath that imports the SDK.
const webRuntimeTelemetryEntry = WebRuntimePackage + "/telemetry"

// telemetryPeers are the runtime peers './telemetry' imports, which a
// frontend importing it must declare. Their specifiers come from the runtime's
// own peerDependencies (webruntimepeers), so the version lives in one place.
var telemetryPeers = []string{"@hyperdx/otel-web", "rrweb"}

// exactVersionRe matches a bare x.y.z, the shape of an exact peer specifier.
var exactVersionRe = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$`)

// telemetryPeerSpecs returns name → the specifier the runtime declares, for
// every telemetry peer the runtime declares. A peer it stops declaring is
// simply not managed any more.
func telemetryPeerSpecs() map[string]string {
	out := map[string]string{}
	for _, name := range telemetryPeers {
		if spec, ok := webruntimepeers.PeerSpec(name); ok {
			out[name] = spec
		}
	}
	return out
}

// wantTelemetryPeer returns the specifier forge wants for a telemetry peer the
// manifest declares as current. An EXACT peer admits one version, and npm
// refuses an install (ERESOLVE) that puts any other beside the runtime, so a
// different value is replaced. A range peer is a floor, like the runtime's
// own (specSatisfiesFloor): a value at or above it is the project's to keep.
func wantTelemetryPeer(current, spec string) string {
	if current == spec {
		return current
	}
	if !exactVersionRe.MatchString(spec) && specSatisfiesFloor(current, spec) {
		return current
	}
	return spec
}

// frontendImportsTelemetry reports whether any source file of the frontend at
// feDir imports '@reliantlabs/forge-web-runtime/telemetry'. Every forge web
// scaffold does (src/lib/otel_gen.ts); an adopted app may, from any file.
// Build output, dependencies and dot-directories are not sources.
func frontendImportsTelemetry(feDir string) bool {
	needle := []byte(webRuntimeTelemetryEntry)
	found := false
	_ = filepath.WalkDir(feDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: not a source this can judge
		}
		if d.IsDir() {
			if path == feDir {
				return nil
			}
			switch name := d.Name(); {
			case strings.HasPrefix(name, "."), name == "node_modules", name == "dist", name == "out",
				name == "build", name == "coverage":
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".mts", ".cts":
		default:
			return nil
		}
		b, err := os.ReadFile(path)
		if err == nil && bytes.Contains(b, needle) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// declaredDependencies returns the manifest's top-level "dependencies".
func declaredDependencies(text string) map[string]string {
	var doc struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
	return doc.Dependencies
}

// dependencyValueRe matches name's entry in a manifest, capturing the quoted
// value. Like webRuntimeDepRe, a package name is unique enough in a
// package.json that the first match is the entry.
func dependencyValueRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`"` + regexp.QuoteMeta(name) + `"\s*:\s*("[^"]*")`)
}

// EnsureTelemetryDependencies declares the telemetry peers in
// <feRelDir>/package.json when the frontend's sources import
// '@reliantlabs/forge-web-runtime/telemetry', at the specifier the runtime
// declares for each. Surgical, like EnsureWebRuntimeDependency: one value
// replaced or one entry inserted, a run that changes nothing writes nothing,
// and every failure is a warning. It reports whether it rewrote the manifest.
//
// A frontend that never imports './telemetry' (Expo, an adopted app without
// telemetry) is left alone: the point of the peers being optional is that it
// installs none of them.
func EnsureTelemetryDependencies(projectDir, feRelDir string) bool {
	feDir := filepath.Join(projectDir, feRelDir)
	pkgPath := filepath.Join(feDir, "package.json")
	info, err := os.Stat(pkgPath)
	if err != nil || !frontendImportsTelemetry(feDir) {
		return false
	}
	original, err := os.ReadFile(pkgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", pkgPath, err)
		return false
	}
	text := string(original)
	declared := declaredDependencies(text)
	specs := telemetryPeerSpecs()
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := specs[name]
		current, found := declared[name]
		switch {
		case found && wantTelemetryPeer(current, spec) == current:
			continue
		case found:
			re := dependencyValueRe(name)
			at := re.FindStringSubmatchIndex(text)
			if at == nil {
				continue // declared somewhere this splice cannot reach; leave it
			}
			text = text[:at[2]] + jsonString(spec) + text[at[3]:]
		default:
			text, err = insertDependency(text, name, spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: could not declare %s in %s: %v\n", name, pkgPath, err)
				return false
			}
		}
	}
	if text == string(original) {
		return false
	}
	if !json.Valid([]byte(text)) {
		fmt.Fprintf(os.Stderr, "warning: declaring the telemetry SDK in %s would have produced invalid JSON; left unchanged\n", pkgPath)
		return false
	}
	checksums.RecordPreWriteAbs(pkgPath)
	if err := atomicfile.WriteFile(pkgPath, []byte(text), info.Mode().Perm()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", pkgPath, err)
		return false
	}
	return true
}

// telemetryDependencyNotices is EnsureTelemetryDependencies' read-only
// counterpart for a disowned manifest: one line per peer it would declare or
// change.
func telemetryDependencyNotices(feDir, text string) []string {
	if !frontendImportsTelemetry(feDir) {
		return nil
	}
	declared := declaredDependencies(text)
	specs := telemetryPeerSpecs()
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		spec := specs[name]
		current, found := declared[name]
		switch {
		case !found:
			out = append(out, fmt.Sprintf("this frontend imports %s, whose peer %s package.json does not declare — `npm install %s@%s`",
				webRuntimeTelemetryEntry, name, name, spec))
		case wantTelemetryPeer(current, spec) != current:
			out = append(out, fmt.Sprintf("package.json declares %s %s; %s requires exactly %s — `npm install %s@%s`",
				name, current, WebRuntimePackage, spec, name, spec))
		}
	}
	return out
}
