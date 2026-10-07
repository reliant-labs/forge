package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

// `forge release bundle <env> <version>` pulls exactly the bundle the release
// recorded, unpacks it, and with --live-diff runs the server-side diff the
// 2026-10-07 review ran by hand — one kubectl diff per cluster, reporting every
// object that would change. It writes nothing to any cluster.
func TestReleaseBundle_PullsTheRecordedBundleAndDiffsItLive(t *testing.T) {
	dir := newLedgerTestProject(t, "bundle-pull")
	stubEnvShape(t, "bundle-pull")
	ctx := context.Background()
	written, err := writeEnvBundle(ctx, dir, "prod", bundleBuildInputs{
		Release: "v1", Pins: release.BundlePins{Images: map[string]string{"api": "sha256:" + rep64('1')}},
		Now: bundleTestNow, errOut: io.Discard,
	})
	if err != nil {
		t.Fatalf("record the bundle: %v", err)
	}

	type call struct{ context, dir string }
	var calls []call
	prev := kubectlServerSideDiff
	kubectlServerSideDiff = func(_ context.Context, kubeContext, path string) (string, int, error) {
		calls = append(calls, call{kubeContext, path})
		return "diff -u -N /tmp/LIVE-1/apps.v1.Deployment.app.api /tmp/MERGED-2/apps.v1.Deployment.app.api\n" +
			"--- /tmp/LIVE-1/apps.v1.Deployment.app.api\n+++ /tmp/MERGED-2/apps.v1.Deployment.app.api\n" +
			"-  replicas: 1\n+  replicas: 2\n", 1, nil
	}
	t.Cleanup(func() { kubectlServerSideDiff = prev })

	out := t.TempDir()
	var buf bytes.Buffer
	if err := runReleaseBundle(ctx, &buf, dir, "prod", "v1", releaseBundleOptions{Dir: out, LiveDiff: true}); err != nil {
		t.Fatalf("release bundle: %v\n%s", err, buf.String())
	}
	got := buf.String()
	if !strings.Contains(got, shortDigest(written.Digest)) || !strings.Contains(got, "release v1") {
		t.Errorf("the summary does not name the release's bundle %s:\n%s", shortDigest(written.Digest), got)
	}
	if _, err := os.Stat(filepath.Join(out, "bundle.json")); err != nil {
		t.Errorf("bundle.json not written: %v", err)
	}
	if len(calls) != 1 || calls[0].context != "prod-ctx" || !strings.HasPrefix(calls[0].dir, filepath.Join(out, "manifests")) {
		t.Fatalf("kubectl diff calls = %+v; want one, against prod-ctx, over the unpacked manifests", calls)
	}
	if entries, _ := os.ReadDir(calls[0].dir); len(entries) == 0 {
		t.Errorf("the diffed directory %s is empty: the manifests were not unpacked there", calls[0].dir)
	}
	if !strings.Contains(got, "1 object(s) would change") || !strings.Contains(got, "apps.v1.Deployment.app.api") {
		t.Errorf("the live diff summary does not name the changed object:\n%s", got)
	}

	// The digest form reaches the same bundle.
	buf.Reset()
	if err := runReleaseBundle(ctx, &buf, dir, "prod", written.Digest, releaseBundleOptions{Dir: t.TempDir()}); err != nil {
		t.Fatalf("release bundle by digest: %v", err)
	}
	if !strings.Contains(buf.String(), shortDigest(written.Digest)) {
		t.Errorf("by digest:\n%s", buf.String())
	}
}

func TestReleaseBundle_IsAReleaseSubcommand(t *testing.T) {
	cmd, _, err := newReleaseCmd().Find([]string{"bundle"})
	if err != nil || cmd == nil || cmd.Name() != "bundle" {
		t.Fatalf("forge release bundle is not registered: %v", err)
	}
	if cmd.Flags().Lookup("live-diff") == nil || cmd.Flags().Lookup("dir") == nil {
		t.Error("forge release bundle must take --dir and --live-diff")
	}
}
