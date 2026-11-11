package kcl_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/kcl"
)

// TestReleaseProbePathsAgreeWithGo evaluates forge.Probe {paths = ...} for
// every case in kcl.ReleaseProbePathCases and requires KCL to accept exactly
// the paths lists pkg/release accepts. A path KCL renders and the control
// plane refuses fails the deploy at record time; one KCL refuses and Go
// accepts is a probe nobody can declare. The module evaluated is the embedded
// one, so the test binary carries every input and the result caches soundly.
func TestReleaseProbePathsAgreeWithGo(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: one KCL evaluation per case (~0.3s each); runs in task test")
	}
	kclplugin.Register()
	for _, paths := range kcl.ReleaseProbePathCases {
		quoted := make([]string, len(paths))
		for i, p := range paths {
			quoted[i] = strconv.Quote(p)
		}
		dir := t.TempDir()
		files := map[string]string{
			"kcl.mod": "[package]\nname = \"probepaths\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
			"main.k":  "import forge\n\nprobe = forge.Probe {paths = [" + strings.Join(quoted, ", ") + "]}\n",
		}
		for f, c := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, err := kclrender.Run(dir, dir, nil)
		if kclOK, goOK := err == nil, kcl.ReleaseProbePathsOK(paths); kclOK != goOK {
			t.Errorf("paths %q: KCL accepts = %v, Go accepts = %v (KCL error: %v)", paths, kclOK, goOK, err)
		}
	}
}
