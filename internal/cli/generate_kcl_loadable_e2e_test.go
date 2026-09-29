//go:build e2e

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EGenerateFailsOnUnrenderableDeployKCL is the end-to-end half of the
// #322 incident: a deploy tree that does not compile must make `forge generate`
// exit NON-ZERO.
//
// It used to exit 0. The KCL load error was downgraded to a warning, so a
// project whose env declared a field the closed schema had removed got a clean
// generate and discovered the break later, at `forge env render` — an error
// with no connection to the command that had already reported success. The
// silently-no-opping registry migration put projects into exactly that state.
//
// The fault is injected the way the incident produced it: a field the schema
// does not accept, added to a real scaffolded env. The assertion is on the exit
// code and on the error naming the env, because a non-zero exit that does not
// say WHICH env or WHAT kcl said has moved the discovery rather than prevented
// it.
func TestE2EGenerateFailsOnUnrenderableDeployKCL(t *testing.T) {
	requirePublishedForgePkg(t)
	t.Parallel()
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "project", "new", "kclbreak", "--mod", "example.com/kclbreak", "--service", "api")
	projectDir := filepath.Join(dir, "kclbreak")

	// A clean scaffold must generate cleanly — otherwise the assertion below
	// proves nothing about the fault we are about to inject.
	runCmd(t, projectDir, forgeBin, "generate")

	// Inject the fault: a member the closed ClusterTarget schema does not
	// have. This is the shape of #322's break (`registry` was removed from
	// this exact schema), without depending on that particular field name.
	mainK := filepath.Join(projectDir, "deploy", "kcl", "prod", "main.k")
	src, err := os.ReadFile(mainK)
	if err != nil {
		t.Fatalf("read prod main.k: %v", err)
	}
	broken := strings.Replace(string(src), "forge.ClusterTarget {",
		"forge.ClusterTarget {\n    no_such_field_on_this_schema = \"x\"", 1)
	if broken == string(src) {
		t.Fatal("prod/main.k declares no forge.ClusterTarget; the fault could not be injected")
	}
	if err := os.WriteFile(mainK, []byte(broken), 0o644); err != nil {
		t.Fatalf("write prod main.k: %v", err)
	}

	cmd := exec.Command(forgeBin, "generate")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOPROXY=https://proxy.golang.org,direct")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("forge generate EXITED 0 on a deploy tree that does not compile — the break then surfaces at render, with nothing pointing back here:\n%s", out)
	}
	got := string(out)
	for _, want := range []string{"deploy KCL", "prod"} {
		if !strings.Contains(got, want) {
			t.Errorf("the failure must name %q so the user knows what to fix:\n%s", want, got)
		}
	}
}
