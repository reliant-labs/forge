// Package cli — `forge experimental` cobra command + the
// startup-warning emitter shared with root's PersistentPreRun.
//
// The namespace itself is a placeholder for now — `forge experimental
// list` is the only subcommand, and it just prints which experimental
// features are enabled vs available. The real reason it exists is to
// give future spike commands a discoverable-but-noncommittal home:
// shipping `forge experimental foo` instead of `forge foo` signals
// "we're trying this; we may break or remove it" without users
// inferring stability from "it's a root-level command".

package cli

import (
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// experimentalWarningEmitted ensures the startup warning fires at most
// once per process. PersistentPreRun runs for every cobra command in
// the tree (root + subcommand), so without this guard `forge dev
// cluster up` would print the warning twice.
var experimentalWarningEmitted atomic.Bool

// emitExperimentalWarning prints the canonical "experimental features
// are on" line to stderr the first time it's called per process.
// Subsequent calls are no-ops. The exact wording is the public
// contract: humans grepping logs and sub-agents matching on the
// "warning: experimental features enabled:" prefix find one
// authoritative string.
func emitExperimentalWarning(w io.Writer, names []config.FeatureName) {
	if len(names) == 0 {
		return
	}
	if !experimentalWarningEmitted.CompareAndSwap(false, true) {
		return
	}
	// One short grep-friendly line. Long-form rationale lives in
	// `forge experimental` help. Stays "warning:" so log scrapers and
	// agents matching on that prefix keep working.
	fmt.Fprintf(w, "warning: experimental: %s (--silence-experimental to hide)\n",
		strings.Join(names, ", "))
}

// newExperimentalCmd is the `forge experimental` subcommand group.
// Today it only has `list`; the namespace is also the published place
// to ship future spike commands. Keeping the namespace empty-but-real
// matters: it documents "here's where experimental commands go" so
// agents that scaffold new commands have an obvious home rather than
// guessing root-level placement.
func newExperimentalCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "experimental",
		Short: "Inspect and manage experimental forge features",
		Long: `Experimental features are gated under ` + "`features.experimental:`" + ` in
forge.yaml. They default off, must be explicitly opted into, and may
change shape between forge versions without a deprecation cycle.

The list of experimental features today:

  ingress          Gateway API codegen + cert-manager / Traefik wiring
  external_builds  KCL Service.build_cmd shell escape hatch
  operators        controller-runtime operators + CRD codegen
  strict_wiring    diagnostics fail-fast (any Bootstrap diagnostic exits)

Subcommands:
  forge experimental list   Show enabled/available experimental features`,
	}
	cmd.AddCommand(newExperimentalListCmd())
	return cmd
}

// newExperimentalListCmd prints the experimental-feature menu plus the
// per-feature opt-in state. Output is intentionally narrow so a sub-
// agent piping it through grep gets two stable bullet styles to match.
func newExperimentalListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print enabled and available experimental features",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := loadProjectStore()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			enabled := store.Features().EnabledExperimentalFeatures()
			enabledSet := make(map[string]bool, len(enabled))
			for _, n := range enabled {
				enabledSet[n] = true
			}
			fmt.Fprintln(out, "Experimental features:")
			for _, n := range config.ExperimentalFeatureNames {
				marker := "[ ]"
				if enabledSet[n] {
					marker = "[x]"
				}
				fmt.Fprintf(out, "  %s %s\n", marker, n)
			}
			if len(enabled) == 0 {
				fmt.Fprintln(out, "\nNone enabled. Opt in by setting `features.experimental.<name>: true` in forge.yaml.")
			}
			return nil
		},
	}
}
