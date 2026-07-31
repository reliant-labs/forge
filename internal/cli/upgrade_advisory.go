// `forge project upgrade`'s scaffold-once report.
//
// The engine — which files are in the tier, and how a difference is
// classified — lives in internal/generator/upgrade_advisory.go. What lives
// here is the report, because "here is what you are missing, and here is
// what taking it would cost you" is a human-facing judgement rather than a
// generator concern.
//
// The whole section is advisory: nothing in it is written unless the user
// named that exact path after --force.
package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// runAdvisoryPass inspects every scaffold-once row and prints the report.
// Returns the results so callers can act on them; the printing is the
// product.
func runAdvisoryPass(projectDir string, cfg *config.ProjectConfig, selection generator.ForceSelection, check bool) ([]generator.AdvisoryResult, error) {
	files, err := generator.AdvisoryFilesFor(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := generator.LoadChecksums(projectDir)
	if err != nil {
		return nil, fmt.Errorf("load .forge ownership state: %w", err)
	}
	results, err := generator.InspectAdvisories(projectDir, cs, files, selection, check)
	if err != nil {
		return nil, err
	}
	printAdvisoryReport(results, check)
	return results, nil
}

// printAdvisoryReport prints the scaffold-once section: one line per file
// that has something to say, the diff beneath it, and the remedies.
//
// Silence is the normal outcome and is a real answer — a project whose
// mechanism files match their templates should be told nothing, not
// reassured at length.
func printAdvisoryReport(results []generator.AdvisoryResult, check bool) {
	var behind []generator.AdvisoryResult
	for _, r := range results {
		if r.Behind() {
			behind = append(behind, r)
		}
	}
	if len(behind) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("Files you own, whose forge templates have moved on:")
	for _, r := range behind {
		fmt.Fprintf(os.Stdout, "  %-55s %s\n", r.Path, advisoryLabel(r, check))
		if r.Diff != "" {
			for _, line := range splitLines(r.Diff) {
				fmt.Fprintf(os.Stdout, "    %s\n", line)
			}
		}
	}

	printAdvisoryRemedies(behind)
}

// advisoryLabel names, in one phrase, what forge found and how sure it is.
//
// The distinction the wording has to carry: a file that is merely OLD and
// a file someone deliberately changed are different situations with
// different remedies, and calling the second one "outdated" writes off
// whoever did the work. So the label reports the evidence — lines the
// template has that this copy does not, lines this copy has that no
// template line accounts for — and lets that speak.
func advisoryLabel(r generator.AdvisoryResult, check bool) string {
	switch {
	case r.Adopted:
		return "adopted the current template"
	case r.Selected && check:
		return "would adopt the current template"
	case r.Status == generator.AdvisoryAbsent:
		return fmt.Sprintf("not in this project — the template ships %s you don't have", lineCount(r.Missing))
	case r.Status == generator.AdvisoryDiverged:
		return fmt.Sprintf("behind by %s, and %s here %s yours alone",
			lineCount(r.Missing), lineCount(r.Local), plural(r.Local, "is", "are"))
	case r.Proven:
		return fmt.Sprintf("behind by %s (untouched since forge wrote it)", lineCount(r.Missing))
	default:
		return fmt.Sprintf("behind by %s (nothing here is unaccounted for)", lineCount(r.Missing))
	}
}

// lineCount renders a line tally in English.
func lineCount(n int) string { return fmt.Sprintf("%d %s", n, plural(n, "line", "lines")) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// printAdvisoryRemedies closes the section with what the user can do.
//
// Two audiences, and the split is the point. A file with no unaccounted
// lines can be adopted wholesale — forge can name the exact command. A
// file carrying lines of its own cannot: taking the template there is a
// MERGE, and forge is not the one who knows which of those lines matter.
// Saying so plainly is the difference between a report that respects the
// work in the file and one that reads as "you are out of date".
//
// No per-file migration-skill pointer is printed, because none would be
// true: nothing in skills/forge/migrations covers a frontend mechanism
// template today, and a pointer that is usually absent teaches readers to
// stop looking for it. When a template change IS too big to read off a
// diff, the place that says so is the migration catalogue this same
// command already surfaces — so the merge case names it.
func printAdvisoryRemedies(behind []generator.AdvisoryResult) {
	var adoptable, merge []string
	for _, r := range behind {
		if r.Adopted || r.Selected {
			continue
		}
		if r.Status == generator.AdvisoryDiverged {
			merge = append(merge, r.Path)
			continue
		}
		adoptable = append(adoptable, r.Path)
	}
	if len(adoptable) == 0 && len(merge) == 0 {
		return
	}

	fmt.Println()
	fmt.Println("  These files are yours: forge wrote them once and has not touched them since.")
	fmt.Println("  Nothing above was changed — this section only reports.")
	if len(adoptable) > 0 {
		fmt.Println("  Take the current template for a file (its contents are replaced):")
		fmt.Printf("    %s project upgrade --force %s\n", Name(), strings.Join(adoptable, " "))
	}
	if len(merge) > 0 {
		fmt.Printf("  %d file(s) carry lines no forge template has — %s\n", len(merge), strings.Join(merge, ", "))
		fmt.Println("  Those are a merge you make, not an overwrite forge can make for you: read the")
		fmt.Println("  diff, take the template's change, keep your own. Forcing one discards your lines.")
		fmt.Printf("    %s project upgrade list   # migrations that explain a change a diff can't\n", Name())
	}
}
