package deploytarget

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// A mobile app's https universal links (Apple Associated Domains, Android App
// Links) are bound to its host by two files at the HOST ROOT:
//
//	/.well-known/apple-app-site-association   (no extension)
//	/.well-known/assetlinks.json
//
// Both platforms require `Content-Type: application/json` and NO redirect.
// The extensionless file means a static host cannot infer the type, so every
// target that serves the site must set it:
//
//   - hosted: the platform serves StaticSiteSpec.WellKnown that way;
//   - OnFirebase: firebase.json `headers` (firebaseHeaders);
//   - OnBucket: the object's content type, set on upload (uploadWellKnown);
//   - anything else (your own CDN or server): your header config. forge names
//     it in the deploy output.

// WellKnownDir is the directory under the host root the files are served from.
const WellKnownDir = ".well-known"

// WellKnownContentType is the one content type every association file is
// served with.
const WellKnownContentType = "application/json"

func sortedWellKnownNames(files map[string]string) []string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// writeWellKnown writes files to <staging>/.well-known/<name>. It is a no-op
// for none, so a site that serves no app is byte-identical to before.
func writeWellKnown(stagingDir string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}
	dir := filepath.Join(stagingDir, WellKnownDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", WellKnownDir, err)
	}
	for _, name := range sortedWellKnownNames(files) {
		if err := atomicfile.WriteFile(filepath.Join(dir, name), []byte(files[name]), 0o644); err != nil {
			return fmt.Errorf("write %s/%s: %w", WellKnownDir, name, err)
		}
	}
	return nil
}
