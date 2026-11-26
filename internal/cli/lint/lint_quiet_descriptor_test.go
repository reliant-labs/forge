package lint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// `forge lint --quiet` prints only what failed, so a lane that refreshes the
// proto descriptor on demand must not print the extractor's progress lines
// ("🔨 Running protoc-gen-forge (mode=descriptor)…"): the run holds the
// refresh's output, which the extractor then prints only on failure
// (internal/cli refreshDescriptor). A run without --quiet keeps them.
func TestQuietLintHoldsTheOnDemandDescriptorRefresh(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quiet bool
	}{{"quiet", true}, {"verbose", false}} {
		t.Run(tc.name, func(t *testing.T) {
			root := lintProject(t, migrationsOffYAML)
			proto := filepath.Join(root, "proto", "services", "widgets", "v1", "widgets.proto")
			if err := os.MkdirAll(filepath.Dir(proto), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(proto, []byte("syntax = \"proto3\";\npackage widgets.v1;\nservice WidgetService {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			calls, heldDuring := 0, false
			codegen.RegisterDescriptorBuilder(func(dir string) error {
				calls++
				heldDuring = codegen.DescriptorRefreshQuiet()
				return codegen.WriteDescriptor(dir, nil)
			})
			t.Cleanup(func() { codegen.RegisterDescriptorBuilder(nil) })

			stop := captureStdout(t)
			err := runLintStructured(t.Context(), lintFlags{idempotency: true, quiet: tc.quiet}, []string{"./..."})
			_ = stop()
			if err != nil {
				t.Fatalf("lint --idempotency: %v", err)
			}
			if calls == 0 {
				t.Fatal("the lane never refreshed the missing descriptor; the fixture no longer exercises the refresh")
			}
			if heldDuring != tc.quiet {
				t.Errorf("descriptor refresh held its output = %v during a quiet=%v run, want %v", heldDuring, tc.quiet, tc.quiet)
			}
			if codegen.DescriptorRefreshQuiet() {
				t.Error("the run returned still holding the descriptor refresh's output")
			}
		})
	}
}
