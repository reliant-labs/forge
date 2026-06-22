package cmdkit

import (
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Resolve returns the value of a string config field with the canonical
// forge precedence: an explicit CLI flag beats an environment variable,
// which beats the supplied default. It is the small, command-local twin
// of the generated config.Load precedence, for ad-hoc per-command inputs
// that don't live in the typed config proto (a --dst Postgres URL, a
// --since cutoff) — replacing the repeated
//
//	if flagVal == "" { flagVal = os.Getenv("X") }
//	if flagVal == "" { return errors.New("X required") }
//
// dance. Pass envVar == "" to skip the env lookup; pass flagName == ""
// to skip the flag lookup.
//
// Precedence rationale matches the generated loader: a flag is typed by
// an operator on this invocation; an env var is ambient process state a
// wrapper script may have set. The more deliberate, more local intent
// wins.
func Resolve(cmd *cobra.Command, flagName, envVar, defaultVal string) string {
	if cmd != nil && flagName != "" {
		if f := cmd.Flags().Lookup(flagName); f != nil && cmd.Flags().Changed(flagName) {
			return f.Value.String()
		}
	}
	if envVar != "" {
		if v, ok := os.LookupEnv(envVar); ok && v != "" {
			return v
		}
	}
	return defaultVal
}

// FirstNonEmpty returns the first non-empty, non-whitespace string from
// vals, or "" if all are empty. Handy for "flag value, then env, then
// fallback" chains the caller has already read into locals:
//
//	dsn := cmdkit.FirstNonEmpty(flagDST, os.Getenv("KALSHI_TRADER_DATABASE_URL"))
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
