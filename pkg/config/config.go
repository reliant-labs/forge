// Package config holds runtime configuration for the CLI.
//
// This file is a stub. Add fields here and load them from environment
// variables, command-line flags, or a config file as your CLI grows.
// If you add proto/config/v1/*.proto and run `forge generate`,
// this file will be regenerated automatically into a richer,
// proto-driven Config with a full RegisterFlags/Load/Validate surface.
//
// Until then the stub below gives every binary kind — server, CLI
// command, standalone binary — the SAME server-independent typed config
// accessor: call RegisterFlags(cmd) when building a cobra command, then
// Load(cmd) inside RunE. The precedence is the canonical forge order
// (explicit flag > environment variable > default), matching what the
// generated loader emits, so code written against the stub keeps working
// after you adopt a config proto.
package config

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Config is the runtime configuration value passed to internal packages.
// Extend this struct (or replace it with a proto-driven generator) as
// your CLI grows. The three baseline fields below are the ones every
// binary kind needs — logging shape and a database URL — so cmdkit
// helpers (cmdkit.Logger, cmdkit.OpenDB) can be fed from config without
// the project first having to author a config proto.
type Config struct {
	LogLevel    string // Log level: debug, info, warn, error
	LogFormat   string // Log output format: json or text
	DatabaseURL string // Database connection string (empty if unused)
}

// RegisterFlags registers the baseline config fields as CLI flags on cmd.
// Call it once when constructing any cobra command (server, CLI tool, or
// standalone binary). Safe to call on multiple commands.
func RegisterFlags(cmd *cobra.Command) {
	cmd.Flags().String("log-level", "info", "Log level (debug, info, warn, error)")
	cmd.Flags().String("log-format", "json", "Log output format (json or text)")
	cmd.Flags().String("database-url", "", "Database connection string")
}

// Load resolves a Config from CLI flags, environment variables, and
// defaults, with precedence flag > env > default. cmd may be nil (env +
// defaults only), so Load works from non-cobra entrypoints too.
func Load(cmd *cobra.Command) (*Config, error) {
	return &Config{
		LogLevel:    resolve(cmd, "log-level", "LOG_LEVEL", "info"),
		LogFormat:   resolve(cmd, "log-format", "LOG_FORMAT", "json"),
		DatabaseURL: resolve(cmd, "database-url", "DATABASE_URL", ""),
	}, nil
}

// resolve applies flag > env > default precedence for one string field.
func resolve(cmd *cobra.Command, flagName, envVar, defaultVal string) string {
	if cmd != nil && flagName != "" {
		if f := cmd.Flags().Lookup(flagName); f != nil && cmd.Flags().Changed(flagName) {
			return f.Value.String()
		}
	}
	if v, ok := os.LookupEnv(envVar); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return defaultVal
}
