// Package cli — `forge project benefits`: every benefit forge gives a
// project, each with the evidence that backs it.
//
// The list is internal/benefits/benefits.yaml, the same registry
// docs/benefits.md and the README's "Everything included" table render from,
// so this command, the page and the README cannot disagree. Like
// `capabilities` it is forge's own surface rather than project state: it
// reads no project files and runs anywhere.
//
// Every evidence item an entry names is resolved against this binary and the
// forge repo by TestBenefitsEvidenceExists, so an entry whose backing code is
// removed fails the build instead of staying in the list.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/benefits"
	"github.com/reliant-labs/forge/internal/cliutil"
)

func newBenefitsCmd() *cobra.Command {
	var (
		asJSON       bool
		category     string
		headlineOnly bool
		format       string
	)
	cmd := &cobra.Command{
		Use:   "benefits",
		Short: "Print every benefit forge gives a project, each with the evidence that backs it",
		Long: `Print every benefit forge gives a project, each with the evidence that backs it.

The list is the registry docs/benefits.md and the README's "Everything
included" table are generated from. Every entry names its evidence — a
command, a lint lane, a marker, a skill, a library or a file — and forge's
own tests fail an entry whose evidence no longer exists.

Formats:
  text       grouped by category, one "title — claim" line per entry (default)
  markdown   the whole catalog page, as docs/benefits.md renders it
  json       the registry, evidence and lint rules included (same as --json)

Examples:
  forge project benefits
  forge project benefits --headline
  forge project benefits --category guardrails --format markdown
  forge project benefits --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if asJSON {
				if cmd.Flags().Changed("format") && format != "json" {
					return cliutil.UserErr("forge project benefits",
						fmt.Sprintf("--json conflicts with --format %s", format), "",
						"pass one of them")
				}
				format = "json"
			}
			return runBenefits(cmd.OutOrStdout(), category, headlineOnly, format)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the registry as JSON (same as --format json)")
	cmd.Flags().StringVar(&category, "category", "", "only this category (build, data, test, dev, guardrails, security, ship, host, run, agents)")
	cmd.Flags().BoolVar(&headlineOnly, "headline", false, "only the headline entries (the README table)")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text, markdown or json")
	return cmd
}

func runBenefits(w io.Writer, category string, headlineOnly bool, format string) error {
	reg, err := benefits.Load()
	if err != nil {
		return err
	}
	if category != "" && !reg.HasCategory(category) {
		var ids []string
		for _, c := range reg.Categories {
			ids = append(ids, c.ID)
		}
		return cliutil.UserErr("forge project benefits",
			fmt.Sprintf("unknown category %q", category), "",
			fmt.Sprintf("use one of: %v", ids))
	}
	reg = reg.Filter(category, headlineOnly)

	switch format {
	case "json":
		b, err := benefits.JSON(reg)
		if err != nil {
			return err
		}
		_, err = w.Write(b)
		return err
	case "markdown":
		_, err := w.Write(benefits.RenderMarkdown(reg))
		return err
	case "text":
		return writeBenefitsText(w, reg)
	default:
		return cliutil.UserErr("forge project benefits",
			fmt.Sprintf("unknown --format %q", format), "", "use text, markdown or json")
	}
}

// writeBenefitsText prints one block per category with entries. An entry that
// is not enforced, or needs more than open-source forge, says so after its
// claim; the defaults are left unsaid so the common line stays short.
func writeBenefitsText(w io.Writer, reg *benefits.Registry) error {
	p := func(format string, args ...any) error {
		_, err := fmt.Fprintf(w, format, args...)
		return err
	}
	for _, c := range reg.Categories {
		entries := reg.InCategory(c.ID)
		if len(entries) == 0 {
			continue
		}
		if err := p("%s\n", c.Title); err != nil {
			return err
		}
		for _, b := range entries {
			note := ""
			if b.Status != benefits.StatusEnforced {
				note += " [" + string(b.Status) + "]"
			}
			if b.Availability != benefits.AvailabilityForge {
				note += " [" + string(b.Availability) + "]"
			}
			if err := p("  %s — %s%s\n", b.Title, strings.Join(strings.Fields(b.Claim), " "), note); err != nil {
				return err
			}
		}
		if err := p("\n"); err != nil {
			return err
		}
	}
	noun := "benefits"
	if len(reg.Benefits) == 1 {
		noun = "benefit"
	}
	return p("%d %s. Evidence and how to use each: `forge project benefits --format markdown`, or docs/benefits.md.\n",
		len(reg.Benefits), noun)
}
