package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// upgrade_codemod.go — generic per-version codemod runner.
//
// The plugin shape is deliberately small: each version-hop migration
// implements `func(projectDir string) (CodemodReport, error)` and
// registers itself by `from` version into the package-level
// `codemodRegistry`. `forge upgrade --to vX.Y` looks up the matching
// codemod, runs it, and folds the report into `UPGRADE_NOTES.md`.
//
// Why a registry rather than a directory walk + per-skill plugin?
// Codemods are Go AST rewrites — they can't be plain markdown files
// like skills are. Keeping them in-binary means one `go install` ships
// the upgrade story for every version forge knows about. Future
// codemods (v0.2 → v0.3, etc.) just append to the registry.
//
// Per-skill markdown for the LLM-assisted parts of a migration lives
// alongside in templates/project/skills/forge/migration/<vX-to-vY>/SKILL.md.
// The codemod handles the deterministic mechanics; the skill describes
// the intent-bearing parts the LLM/user should review.

// CodemodReport summarizes one upgrade codemod run. Auto entries are
// the deterministic rewrites the codemod applied; Manual entries are
// observations the codemod made about the project that need
// LLM/user review (e.g. ambiguous nil-checks it left in place).
type CodemodReport struct {
	// Auto is the list of mechanical rewrites the codemod applied,
	// each formatted as a single human-readable line ("removed
	// ApplyDeps in pkg/app/setup.go:42-48", etc.). Order matters for
	// the UPGRADE_NOTES.md output — keep insertion order.
	Auto []string

	// Manual is the list of items the codemod identified but didn't
	// rewrite, each with a file:line reference so the LLM can land
	// on the right spot. Reasons range from "pattern didn't match
	// the conservative shape we auto-rewrite" to "needs intent
	// inspection".
	Manual []ManualItem

	// VerifyCommands are the commands the user should run after the
	// codemod completes. Defaults to the triple-gate when the
	// codemod doesn't override.
	VerifyCommands []string
}

// ManualItem is one entry in CodemodReport.Manual — file:line pairs
// the LLM/user should look at after the codemod completes.
type ManualItem struct {
	File   string // relative to projectDir
	Line   int    // 1-based; 0 means file-level (no specific line)
	Reason string // short, paste-into-an-LLM-prompt-friendly
}

// CodemodFn is the contract every per-version codemod implements.
// projectDir is the absolute path to the project root (the dir
// containing forge.yaml).
type CodemodFn func(projectDir string) (CodemodReport, error)

// codemodRegistry maps "<from>->vX.Y" hop identifier to the codemod
// that performs it. Keys are normalized via codemodKey() so callers
// don't have to worry about "v" prefixes / patch versions.
//
// At init() time, each codemod registers itself by calling
// registerCodemod. Adding a new migration is one new file under
// internal/cli/upgrade_<from>_to_<to>.go.
var codemodRegistry = map[string]CodemodFn{}

// registerCodemod adds a codemod for the named hop. Called from each
// codemod file's init().
func registerCodemod(fromMinor, toMinor string, fn CodemodFn) {
	codemodRegistry[codemodKey(fromMinor, toMinor)] = fn
}

// codemodKey normalizes a (from, to) version pair into the registry
// key. Strips "v" prefix and any patch component so v0.1.3 and v0.1
// both resolve to the v0.1→v0.2 codemod.
func codemodKey(from, to string) string {
	return normalizeMinor(from) + "->" + normalizeMinor(to)
}

// normalizeMinor strips a leading "v" and any trailing patch component
// from a version string. "v0.1.3" → "0.1", "0.1" → "0.1", "v0.2" →
// "0.2", "" → "" (caller treats empty as "no migration declared").
func normalizeMinor(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	if v == "" {
		return ""
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return parts[0]
}

// minorHopDistance returns the number of minor versions between from
// and to, or -1 if either side can't be parsed cleanly. "0.1" → "0.2"
// returns 1; "0.1" → "0.5" returns 4; "0.1" → "1.0" returns -1
// (major hops are not supported by the codemod chain).
func minorHopDistance(from, to string) int {
	fMaj, fMin, ok1 := splitMinor(from)
	tMaj, tMin, ok2 := splitMinor(to)
	if !ok1 || !ok2 {
		return -1
	}
	if fMaj != tMaj {
		// Different majors — not a minor hop. Caller decides whether
		// to error out or fall through to "unknown hop".
		return -1
	}
	d := tMin - fMin
	if d < 0 {
		// Downgrades aren't auto-migrated. -1 to surface "no chain".
		return -1
	}
	return d
}

// splitMinor returns (major, minor, ok) for a SemVer-ish "vMaj.Min(.patch)"
// string. ok=false on any parse hiccup so callers fall through cleanly.
func splitMinor(v string) (int, int, bool) {
	v = normalizeMinor(v)
	if v == "" {
		return 0, 0, false
	}
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	maj, err := atoiSafe(parts[0])
	if err != nil {
		return 0, 0, false
	}
	min, err := atoiSafe(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// atoiSafe is strconv.Atoi with the import dependency hidden. Kept
// inline to avoid yet another import line in this file.
func atoiSafe(s string) (int, error) {
	n := 0
	if s == "" {
		return 0, fmt.Errorf("empty number")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("non-digit %q", c)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}

// runCodemodChain executes the codemod for each minor hop between from
// and to (in order). Returns the merged CodemodReport so the caller
// can write a single UPGRADE_NOTES.md spanning the whole chain.
//
// If --to skips minor versions and any intermediate codemod is missing,
// returns an error pointing at the gap. The caller should bail without
// writing anything else (the project is left in its starting shape).
func runCodemodChain(projectDir, from, to string) (CodemodReport, error) {
	merged := CodemodReport{}

	fMaj, fMin, ok1 := splitMinor(from)
	tMaj, tMin, ok2 := splitMinor(to)
	if !ok1 || !ok2 || fMaj != tMaj {
		// No chain possible. Caller decides whether this is an error
		// or just "no codemod for this hop, run the regular upgrade".
		return merged, nil
	}

	for cur := fMin; cur < tMin; cur++ {
		hopFrom := fmt.Sprintf("%d.%d", fMaj, cur)
		hopTo := fmt.Sprintf("%d.%d", fMaj, cur+1)
		key := codemodKey(hopFrom, hopTo)
		fn, ok := codemodRegistry[key]
		if !ok {
			return merged, fmt.Errorf("no codemod registered for v%s -> v%s; expected one to be registered before forge upgrade can hop this minor", hopFrom, hopTo)
		}
		report, err := fn(projectDir)
		if err != nil {
			return merged, fmt.Errorf("codemod v%s -> v%s: %w", hopFrom, hopTo, err)
		}
		merged.Auto = append(merged.Auto, report.Auto...)
		merged.Manual = append(merged.Manual, report.Manual...)
		if len(report.VerifyCommands) > 0 {
			merged.VerifyCommands = report.VerifyCommands
		}
	}
	return merged, nil
}

// writeUpgradeNotes serializes a CodemodReport to UPGRADE_NOTES.md
// at the project root. The file is overwritten on each run — it's
// meant as a per-upgrade scratch pad, not a long-term log. Users
// review and delete after the upgrade lands.
func writeUpgradeNotes(projectDir, fromVersion, toVersion string, report CodemodReport) error {
	path := filepath.Join(projectDir, "UPGRADE_NOTES.md")

	// Sort manual items by file then line for stable output regardless
	// of the order codemods discovered them.
	sort.SliceStable(report.Manual, func(i, j int) bool {
		if report.Manual[i].File != report.Manual[j].File {
			return report.Manual[i].File < report.Manual[j].File
		}
		return report.Manual[i].Line < report.Manual[j].Line
	})

	verify := report.VerifyCommands
	if len(verify) == 0 {
		verify = []string{
			"go build ./...",
			"go test -count=1 ./...",
			"forge lint",
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "# Forge upgrade notes — %s → %s\n\n", fromVersion, toVersion)
	fmt.Fprintf(&sb, "_Generated by `forge upgrade` at %s. Review, then delete this file once the migration lands._\n\n",
		time.Now().UTC().Format(time.RFC3339))

	if len(report.Auto) > 0 {
		sb.WriteString("## Auto-applied changes\n\n")
		sb.WriteString("These changes were applied by `forge upgrade`'s codemod runner. They are deterministic — re-running the upgrade is idempotent.\n\n")
		for _, line := range report.Auto {
			fmt.Fprintf(&sb, "- %s\n", line)
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("## Auto-applied changes\n\n")
		sb.WriteString("_None — the codemod found no patterns to rewrite. (Either the project was already in v" + normalizeMinor(toVersion) + " shape, or the codemod for this hop is informational-only.)_\n\n")
	}

	if len(report.Manual) > 0 {
		sb.WriteString("## Needs LLM / manual attention\n\n")
		sb.WriteString("These items were identified but **not rewritten** — they need human (or LLM-with-context) judgement. Each entry includes a file:line reference for direct navigation.\n\n")
		for _, m := range report.Manual {
			loc := m.File
			if m.Line > 0 {
				loc = fmt.Sprintf("%s:%d", m.File, m.Line)
			}
			fmt.Fprintf(&sb, "- **%s** — %s\n", loc, m.Reason)
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Verification\n\n")
	sb.WriteString("Run the following from the project root after applying any manual changes above:\n\n")
	sb.WriteString("```bash\n")
	for _, c := range verify {
		fmt.Fprintf(&sb, "%s\n", c)
	}
	sb.WriteString("```\n\n")
	sb.WriteString("Clean compile + green tests + clean lint = upgrade done. See the per-version migration skill (`forge skill load migration/v" + normalizeMinor(fromVersion) + "-to-v" + normalizeMinor(toVersion) + "`) for the full intent-level migration story.\n")

	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
