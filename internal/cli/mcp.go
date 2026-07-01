// Package cli — `forge mcp` command surface.
//
// `forge mcp serve` hosts gen/mcp/manifest.json as a live Model Context
// Protocol stdio server: every Connect RPC in the project becomes an MCP
// tool an agent can call. The manifest is a generated artifact (one tool
// per RPC) that, before this command existed, had no host — `forge mcp
// serve` is that host.
//
// Transport: tools/call dispatches to the running Connect service over
// plain HTTP+JSON — exactly the shape `forge api curl` prints. The service
// port is resolved the same way `forge api curl` resolves it (forge.yaml
// services[].port, with the same fallbacks), so `--addr` only needs to be
// supplied when the default host/port don't match the running server (e.g.
// behind an ingress).
//
// Auth: a forge dev server that is NOT in AUTH_DEV_MODE runs the auth
// interceptor, so a tokenless tools/call returns a clean Connect
// "unauthenticated" error (which itself proves the wiring). Pass a bearer
// token via --token or $FORGE_MCP_TOKEN to authenticate real calls. The
// same env var is the shared dev-token seam `forge api curl` can adopt for
// its own --token in future.
package cli

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/mcpbridge"
)

// mcpTokenEnv is the shared env var for the dev bearer token. `forge mcp
// serve` reads it when --token is not passed; `forge api curl` can read the
// same var so a developer exports one value and both tools authenticate.
const mcpTokenEnv = "FORGE_MCP_TOKEN"

// newMCPCmd is the parent for `forge mcp ...`. Today the only verb is
// `serve`; future verbs (e.g. `forge mcp tools` to dump the manifest)
// hang off the same parent.
func newMCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Host the project's RPCs as Model Context Protocol tools",
		Long: `forge generate writes gen/mcp/manifest.json — one MCP tool per Connect
RPC. ` + "`forge mcp serve`" + ` is the stdio server that hosts that manifest so an
MCP client (Claude Code, Claude Desktop, the MCP Inspector) can list and
call every RPC as a tool.`,
	}
	cmd.AddCommand(newMCPServeCmd())
	return cmd
}

// newMCPServeCmd implements `forge mcp serve`. It speaks MCP over stdio:
// stdin/stdout carry the JSON-RPC protocol, stderr carries diagnostics.
func newMCPServeCmd() *cobra.Command {
	var (
		addr         string
		manifestPath string
		token        string
		authHeader   string
		timeoutSec   int
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run a stdio MCP server hosting gen/mcp/manifest.json",
		Long: `Run a Model Context Protocol stdio server that exposes every Connect RPC
in gen/mcp/manifest.json as an MCP tool.

  - tools/list enumerates the project's unary RPCs (streaming RPCs are
    excluded — MCP tools are unary).
  - tools/call dispatches to the running Connect service over HTTP+JSON
    (the same transport ` + "`forge api curl`" + ` prints) and returns the response.

The server reads stdin/stdout for the MCP protocol and logs to stderr, so
it is launched by an MCP client, not run interactively. Wire it into
.mcp.json:

  {"mcpServers": {"forge": {"command": "forge", "args": ["mcp", "serve"]}}}

Auth: a dev server NOT in AUTH_DEV_MODE runs the auth interceptor, so a
tokenless call returns a clean Connect "unauthenticated" error. Pass a
bearer token to authenticate:

  forge mcp serve --token "$(your-token-command)"
  FORGE_MCP_TOKEN=<token> forge mcp serve

Address: --addr defaults to http://localhost:<port>, where <port> is
resolved from forge.yaml the same way ` + "`forge api curl`" + ` resolves it. Pass
--addr explicitly when the server is reachable elsewhere (e.g. behind an
ingress: --addr http://controller.localhost:28080).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectDir, err := findProjectRoot()
			if err != nil || projectDir == "" {
				return cliutil.UserErr("forge mcp serve",
					"could not find forge.yaml in current directory or any parent",
					"",
					"run from inside a forge project, or `cd` to one first")
			}

			if manifestPath == "" {
				manifestPath = filepath.Join(projectDir, "gen", "mcp", "manifest.json")
			}
			manifest, err := mcpbridge.LoadManifest(manifestPath)
			if err != nil {
				return cliutil.UserErr("forge mcp serve",
					fmt.Sprintf("could not read %s: %v", manifestPath, err),
					manifestPath,
					"run `forge generate` to produce gen/mcp/manifest.json, then retry")
			}

			// Resolve the Connect base URL. Explicit --addr wins; otherwise
			// reuse the forge.yaml port resolution `forge api curl` uses.
			resolvedAddr := strings.TrimRight(addr, "/")
			if resolvedAddr == "" {
				resolvedAddr = defaultMCPAddr(projectDir)
			}

			// Auth header: --auth (verbatim header) wins over --token
			// (bearer); --token wins over $FORGE_MCP_TOKEN. We warn on
			// conflict rather than silently preferring — loud-by-default.
			finalAuth := authHeader
			switch {
			case authHeader != "" && (token != "" || os.Getenv(mcpTokenEnv) != ""):
				fmt.Fprintln(os.Stderr, "[forge mcp] warning: --auth set; ignoring --token/$"+mcpTokenEnv)
			case token != "":
				finalAuth = "Bearer " + token
			case os.Getenv(mcpTokenEnv) != "":
				finalAuth = "Bearer " + os.Getenv(mcpTokenEnv)
			}

			fmt.Fprintf(os.Stderr,
				"[forge mcp] project=%s tools=%d addr=%s auth=%v\n",
				manifest.Project, len(manifest.Tools), resolvedAddr, finalAuth != "")

			srv := &mcpbridge.Server{
				Manifest:   manifest,
				Addr:       resolvedAddr,
				AuthHeader: finalAuth,
				HTTP:       &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
				Logf: func(format string, a ...any) {
					fmt.Fprintf(os.Stderr, "[forge mcp] "+format+"\n", a...)
				},
			}
			if err := srv.Run(cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				return fmt.Errorf("mcp server: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "Connect base URL (default: http://localhost:<port> resolved from forge.yaml)")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Path to the MCP manifest (default: <project>/gen/mcp/manifest.json)")
	cmd.Flags().StringVar(&token, "token", "", "Bearer token for authenticated calls (default: $"+mcpTokenEnv+")")
	cmd.Flags().StringVar(&authHeader, "auth", "", "Verbatim Authorization header value (overrides --token)")
	cmd.Flags().IntVar(&timeoutSec, "timeout", 30, "Per-RPC HTTP timeout in seconds")
	return cmd
}

// defaultMCPAddr derives the Connect base URL from the project's forge.yaml
// using the same port resolution `forge api curl` uses. We resolve against
// the first service in the descriptor (in a binary=shared project every
// service shares one listener, so any service resolves the same port); if
// no descriptor or no service port is found, we fall back to the
// conventional default. The user can always override with --addr.
func defaultMCPAddr(projectDir string) string {
	const fallback = "http://localhost:8080"
	desc, err := loadForgeDescriptor(projectDir)
	if err != nil || desc == nil || len(desc.Services) == 0 {
		return fallback
	}
	port := lookupServicePort(projectDir, desc.Services[0])
	if port == 0 {
		return fallback
	}
	return fmt.Sprintf("http://localhost:%d", port)
}
