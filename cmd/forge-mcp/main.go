// Command forge-mcp is a standalone Model Context Protocol stdio server
// that hosts a forge project's gen/mcp/manifest.json — every Connect RPC
// becomes an MCP tool. It is a thin wrapper over internal/mcpbridge; the
// `forge mcp serve` subcommand wraps the same package and is the preferred
// entry point (it resolves the server address from forge.yaml). This
// standalone binary exists for clients that prefer a dedicated executable
// or want to point at an arbitrary manifest/address without a forge project
// on disk.
//
// Usage:
//
//	forge-mcp --addr http://localhost:8080                 # call a running Connect server
//	forge-mcp --addr http://... --auth "Bearer xxx"        # propagate an Authorization header
//	forge-mcp --addr http://... --auth-env FORGE_MCP_TOKEN # ... or read it from env
//	forge-mcp --addr http://... --manifest <path>          # explicit manifest
//	forge-mcp --addr http://... --project <dir>            # resolves <dir>/gen/mcp/manifest.json
//
// stdout is reserved for the MCP protocol; diagnostics go to stderr.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/reliant-labs/forge/internal/mcpbridge"
)

func main() {
	var (
		manifestPath string
		projectDir   string
		addr         string
		authHeader   string
		authEnv      string
		timeoutSec   int
	)
	flag.StringVar(&manifestPath, "manifest", "", "Explicit path to gen/mcp/manifest.json (overrides --project)")
	flag.StringVar(&projectDir, "project", ".", "Project root; manifest is resolved at <project>/gen/mcp/manifest.json")
	flag.StringVar(&addr, "addr", "", "Connect server base URL (e.g. http://localhost:8080). REQUIRED — without it, tools/call errors.")
	flag.StringVar(&authHeader, "auth", "", "Verbatim Authorization header value (e.g. 'Bearer eyJ...'). Mutually exclusive with --auth-env.")
	flag.StringVar(&authEnv, "auth-env", "", "Env var name to read the Authorization header from at startup (e.g. FORGE_MCP_TOKEN).")
	flag.IntVar(&timeoutSec, "timeout", 30, "Per-RPC HTTP timeout in seconds")
	flag.Parse()

	// stderr is the only safe channel for diagnostics — stdout carries the
	// MCP protocol framing.
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
	log.SetPrefix("[forge-mcp] ")

	if manifestPath == "" {
		manifestPath = filepath.Join(projectDir, "gen", "mcp", "manifest.json")
	}

	// --auth wins over --auth-env when both are set; warn so the conflict
	// is visible rather than silently resolved.
	if authEnv != "" {
		v := os.Getenv(authEnv)
		if authHeader != "" {
			log.Printf("warning: --auth and --auth-env both set; using --auth verbatim, ignoring $%s", authEnv)
		} else {
			authHeader = v
		}
	}

	manifest, err := mcpbridge.LoadManifest(manifestPath)
	if err != nil {
		log.Fatalf("load manifest %s: %v", manifestPath, err)
	}
	log.Printf("loaded manifest: project=%s tools=%d addr=%s auth=%v",
		manifest.Project, len(manifest.Tools), addr, authHeader != "")

	srv := &mcpbridge.Server{
		Manifest:   manifest,
		Addr:       addr,
		AuthHeader: authHeader,
		HTTP:       &http.Client{Timeout: time.Duration(timeoutSec) * time.Second},
	}
	if err := srv.Run(os.Stdin, os.Stdout); err != nil && err != io.EOF {
		log.Fatalf("server: %v", err)
	}
}
