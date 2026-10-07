package cli

import (
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// 2026-10-07: ~/go/bin/forge was 3b4499b3 while every repo pinned d6b5d722,
// and nothing said so. A build/release/deploy command under a forge that is
// not the pinned one is refused, naming both.
func TestForgePinSkew_DeployUnderAnotherForgeIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeForgeYAML(t, dir, "name: demo\nmodule_path: github.com/example/demo\n"+
		"forge_version: v0.1.44-0.20261007082641-d6b5d722b839\n")
	t.Chdir(dir)
	prevVersion, prevCommit := buildinfo.Version(), buildinfo.GitCommit()
	buildinfo.Set("v0.1.44-0.20261007053821-3b4499b384de", "", "3b4499b384de")
	t.Cleanup(func() { buildinfo.Set(prevVersion, "", prevCommit) })

	root := NewRootCmd()
	root.SetArgs([]string{"env", "deploy", "prod", "--explain"})
	err := root.Execute()
	if err == nil {
		t.Fatal("a deploy under a forge other than the pinned one ran")
	}
	for _, want := range []string{"v0.1.44-0.20261007053821-3b4499b384de", "forge_version v0.1.44-0.20261007082641-d6b5d722b839"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q:\n%v", want, err)
		}
	}
}
