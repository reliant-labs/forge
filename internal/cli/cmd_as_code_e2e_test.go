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

// TestE2ECmdAsCodeSubcommands drives the cmd-as-code surface end to end
// on a real scaffold:
//
//  1. `./<bin> <svc>` is a REAL cobra subcommand (its own `-h`, its own
//     identity) that boots only that service through the canonical server
//     pipeline. Mount selection lives in the cmd layer over the data-only
//     internal/app Inventory; the subcommand bakes its own name in as the
//     sole selection key. The multi-service `./<bin> server [svc...]`
//     form keeps working for run-all / arbitrary subsets.
//  2. The user-owned cmd/commands.go extension point: a second binary
//     registered AS CODE (userCommands()) shows up on the root command,
//     runs, and SURVIVES regeneration (Tier-2: forge never overwrites).
//  3. `forge audit --json` reports no phantom service.
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

	// (1) The REAL per-service subcommand file IS emitted, and the
	// user-owned extension point exists.
	assertPathExistsE2E(t, filepath.Join(projectDir, "cmd", "services_gen.go"))
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

	// Regenerate: the user-owned extension point must survive verbatim.
	runCmd(t, projectDir, forgeBin, "generate")
	if got := readFileE2E(t, commandsPath); got != customCommands {
		t.Fatalf("forge generate overwrote the user-owned cmd/commands.go:\n%s", got)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, "cmd", "services_gen.go"))

	bin := filepath.Join(projectDir, "cmdcode-bin")
	runCmd(t, projectDir, "go", "build", "-o", bin, "./cmd/...")

	// Root help advertises the canonical server command, the REAL
	// per-service subcommand (api), and the code-registered second binary.
	helpOut := runCmdOutput(t, projectDir, bin, "--help")
	for _, want := range []string{"proxy-tool", "server", "api"} {
		if !strings.Contains(helpOut, want) {
			t.Errorf("root --help missing subcommand %q:\n%s", want, helpOut)
		}
	}

	// `<bin> api -h` is a FIRST-CLASS subcommand with its own,
	// service-specific help — not a positional arg to `server`.
	svcHelpOut := runCmdOutput(t, projectDir, bin, "api", "-h")
	if !strings.Contains(svcHelpOut, "Run only the api service") {
		t.Errorf("`api -h` missing service-specific help:\n%s", svcHelpOut)
	}

	// The second binary runs through the shared root.
	toolOut := runCmdOutput(t, projectDir, bin, "proxy-tool")
	if !strings.Contains(toolOut, "proxy-tool: code-registered second binary ran") {
		t.Errorf("proxy-tool subcommand did not run its body:\n%s", toolOut)
	}

	// bootService starts `bin <args...>` (a real per-service subcommand or
	// the `server [svc...]` form), waits for /healthz, and asserts 200.
	bootService := func(label string, args ...string) {
		t.Helper()
		port := freePortE2E(t)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		srv := exec.CommandContext(ctx, bin, args...)
		srv.Dir = projectDir
		srv.Env = append(os.Environ(),
			fmt.Sprintf("PORT=%d", port),
			"DATABASE_URL=",
			// Dev posture: production refuses to start without an auth
			// provider (the refusal contract has its own tests). Auth bypass
			// is now EXPLICIT — ENVIRONMENT=development alone keeps auth on,
			// so a providerless scaffold needs AUTH_DEV_MODE=true to boot.
			"ENVIRONMENT=development",
			"AUTH_DEV_MODE=true",
		)
		var srvOut strings.Builder
		srv.Stdout = &srvOut
		srv.Stderr = &srvOut
		if err := srv.Start(); err != nil {
			t.Fatalf("failed to start `cmdcode-bin %s`: %v", label, err)
		}
		defer func() {
			_ = srv.Process.Kill()
			_ = srv.Wait()
		}()
		addr := fmt.Sprintf("http://127.0.0.1:%d", port)
		if !waitForServer(t, addr+"/healthz", 10*time.Second) {
			t.Fatalf("`cmdcode-bin %s` did not become ready\noutput:\n%s", label, srvOut.String())
		}
		resp, err := http.Get(addr + "/healthz")
		if err != nil {
			t.Fatalf("health check failed (%s): %v", label, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 from /healthz (%s), got %d", label, resp.StatusCode)
		}
	}

	// The REAL per-service subcommand boots only that service.
	bootService("api", "api")
	// The multi-service `server [svc...]` form still works.
	bootService("server api", "server", "api")

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

	// (4) Generated-auth honesty compiles end to end. Declare a provider
	// in forge.yaml, regenerate, and the build must link runServer's
	// InstallGeneratedAuth call against the regenerated auth_gen.go —
	// pre-M6 the generated interceptor had zero call sites, so this pair
	// could silently drift.
	forgeYamlPath := filepath.Join(projectDir, "forge.yaml")
	baseYaml := readFileE2E(t, forgeYamlPath)
	writeYaml := func(provider string) {
		t.Helper()
		content := baseYaml + "\nauth:\n  provider: " + provider + "\n"
		if err := os.WriteFile(forgeYamlPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// jwt: wired through the token-validator policy hook.
	writeYaml("jwt")
	runCmd(t, projectDir, forgeBin, "generate")
	serverGo := readFileE2E(t, filepath.Join(projectDir, "cmd", "server.go"))
	if !strings.Contains(serverGo, "middleware.InstallGeneratedAuth()") {
		t.Fatalf("cmd/server.go must call middleware.InstallGeneratedAuth() with auth.provider: jwt:\n%s", serverGo)
	}
	authGen := readFileE2E(t, filepath.Join(projectDir, "pkg", "middleware", "auth_gen.go"))
	if !strings.Contains(authGen, "SetTokenValidator(v.Validate)") {
		t.Fatalf("auth_gen.go (jwt) must install the validator into the policy surface:\n%s", authGen)
	}
	runCmd(t, projectDir, "go", "build", "./...")

	// api_key: header-carried — the generated interceptor joins the chain.
	writeYaml("api_key")
	runCmd(t, projectDir, forgeBin, "generate")
	serverGo = readFileE2E(t, filepath.Join(projectDir, "cmd", "server.go"))
	if !strings.Contains(serverGo, "middleware.GeneratedAuthInterceptor()") {
		t.Fatalf("cmd/server.go must mount the generated interceptor with auth.provider: api_key:\n%s", serverGo)
	}
	runCmd(t, projectDir, "go", "build", "./...")
}
