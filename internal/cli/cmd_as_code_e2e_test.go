//go:build e2e

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestE2ECmdAsCodeSubcommands drives the M6 cmd-as-code surface end to
// end on a real scaffold:
//
//  1. The generated per-service subcommand file (cmd/services_gen.go)
//     is a projection of the RegisteredServices rows — present after
//     scaffold + generate, byte-stable, and `./<bin> <svc>` boots only
//     that service through the canonical server pipeline.
//  2. The user-owned cmd/commands.go extension point: a second binary
//     registered AS CODE (userCommands()) shows up on the root command,
//     runs, and SURVIVES regeneration (Tier-2: forge never overwrites).
//  3. `forge audit --json` reports no phantom service: everything the
//     binary's cmd surface advertises is backed by a registration row.
func TestE2ECmdAsCodeSubcommands(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin, "new", "cmdcode", "--mod", "example.com/cmdcode", "--service", "api")
	projectDir := filepath.Join(dir, "cmdcode")

	// Local-replace harness (same as the fixture corpus): appkit/
	// serverkit revisions are newer than any published forge/pkg
	// snapshot, so wire the scaffold to the in-repo sources.
	addCorpusForgePkgReplace(t, projectDir)

	runCmd(t, projectDir, forgeBin, "generate")

	// (1) The subcommand projection + extension point both exist.
	subcmds := readFileE2E(t, filepath.Join(projectDir, "cmd", "services_gen.go"))
	if !strings.Contains(subcmds, "var serviceCmdAPI = &cobra.Command{") {
		t.Fatalf("cmd/services_gen.go must project the api registration row into a subcommand:\n%s", subcmds)
	}
	if !strings.Contains(subcmds, `return runServer(cmd, []string{"api"})`) {
		t.Fatalf("the api subcommand must delegate to runServer with the single-name filter:\n%s", subcmds)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, "cmd", "commands.go"))

	// (2) Register a second binary AS CODE: replace the scaffolded
	// userCommands with a self-contained subcommand (the workspace-proxy
	// shape — a process that is not a Connect service and needs no
	// parallel main()).
	customCommands := `// What extra subcommands this binary ships is code, not config.
package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func proxyToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "proxy-tool",
		Short: "Run the proxy tool (second binary registered as code)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Println("proxy-tool: code-registered second binary ran")
			return nil
		},
	}
}

func userCommands() []*cobra.Command {
	return []*cobra.Command{proxyToolCmd()}
}
`
	commandsPath := filepath.Join(projectDir, "cmd", "commands.go")
	if err := os.WriteFile(commandsPath, []byte(customCommands), 0644); err != nil {
		t.Fatal(err)
	}

	// Regenerate: the user-owned extension point must survive verbatim
	// and the projection must be byte-stable.
	runCmd(t, projectDir, forgeBin, "generate")
	if got := readFileE2E(t, commandsPath); got != customCommands {
		t.Fatalf("forge generate overwrote the user-owned cmd/commands.go:\n%s", got)
	}
	if again := readFileE2E(t, filepath.Join(projectDir, "cmd", "services_gen.go")); again != subcmds {
		t.Errorf("cmd/services_gen.go must be byte-stable across generates with an unchanged row list")
	}

	bin := filepath.Join(projectDir, "cmdcode-bin")
	runCmd(t, projectDir, "go", "build", "-o", bin, "./cmd/...")

	// Root help advertises both the projected service subcommand and the
	// code-registered second binary.
	helpOut := runCmdOutput(t, projectDir, bin, "--help")
	for _, want := range []string{"api", "proxy-tool", "server"} {
		if !strings.Contains(helpOut, want) {
			t.Errorf("root --help missing subcommand %q:\n%s", want, helpOut)
		}
	}

	// The second binary runs through the shared root.
	toolOut := runCmdOutput(t, projectDir, bin, "proxy-tool")
	if !strings.Contains(toolOut, "proxy-tool: code-registered second binary ran") {
		t.Errorf("proxy-tool subcommand did not run its body:\n%s", toolOut)
	}

	// The per-service subcommand boots the canonical server pipeline.
	port := freePortE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	srv := exec.CommandContext(ctx, bin, "api")
	srv.Dir = projectDir
	srv.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL=",
		// Dev posture: production refuses to start without an auth
		// provider (the refusal contract has its own tests).
		"ENVIRONMENT=development",
	)
	var srvOut strings.Builder
	srv.Stdout = &srvOut
	srv.Stderr = &srvOut
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start `cmdcode-bin api`: %v", err)
	}
	defer func() {
		_ = srv.Process.Kill()
		_ = srv.Wait()
	}()
	addr := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !waitForServer(t, addr+"/healthz", 10*time.Second) {
		t.Fatalf("`cmdcode-bin api` did not become ready\noutput:\n%s", srvOut.String())
	}
	resp, err := http.Get(addr + "/healthz")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from /healthz, got %d", resp.StatusCode)
	}

	// (3) No phantom service: audit's codegen category must not carry an
	// unregistered_services finding — every subcommand the binary
	// advertises is backed by a registration row.
	auditOut := runCmdOutput(t, projectDir, forgeBin, "audit", "--json")
	var report struct {
		Categories map[string]struct {
			Details map[string]any `json:"details"`
		} `json:"categories"`
	}
	if err := json.Unmarshal([]byte(auditOut), &report); err != nil {
		t.Fatalf("parse audit JSON: %v\n%s", err, auditOut)
	}
	if findings, ok := report.Categories["codegen"].Details["unregistered_services"]; ok {
		t.Errorf("audit reports phantom (unregistered) services on a fully code-registered binary: %v", findings)
	}
}
