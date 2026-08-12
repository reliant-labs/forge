package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// upgrade_codemod.go — generic per-version codemod runner.
//
// The plugin shape is deliberately small: each version-hop migration
// implements `func(projectDir string) (CodemodReport, error)` and
// registers itself by `from` version into the package-level
// `codemodRegistry`. `forge project upgrade --to vX.Y` looks up the matching
// codemod, runs it, and folds the report into `UPGRADE_NOTES.md`.
//
// Why a registry rather than a directory walk + per-skill plugin?
// Codemods are Go AST rewrites — they can't be plain markdown files
// like skills are. Keeping them in-binary means one `go install` ships
// the upgrade story for every version forge knows about. Future
// codemods (v0.2 → v0.3, etc.) just append to the registry.
//
// Per-skill markdown for the LLM-assisted parts of a migration lives
// alongside in templates/project/skills/forge/migrations/<vX-to-vY>/SKILL.md.
// The codemod handles the deterministic mechanics; the skill describes
// the intent-bearing parts the LLM/user should review.

// CodemodReport summarizes one upgrade codemod run: the deterministic
// rewrites the codemod applied, plus the commands to verify them.
//
// There was once a parallel `Manual []ManualItem` list for observations a
// codemod made but declined to rewrite. It went out with the last
// codemod that produced one, because nothing left could write it and a
// field that can only hold its zero value makes every branch downstream
// decoration. The intent-bearing half of an upgrade lives in the
// per-release migration skill now (see upgrade_migrations.go), which is
// prose an agent reads rather than a struct a codemod fills in. If a
// future codemod genuinely needs to hand back file:line observations,
// reintroduce the field together with the code that writes it.
type CodemodReport struct {
	// Auto is the list of mechanical rewrites the codemod applied,
	// each formatted as a single human-readable line ("removed
	// ApplyDeps in pkg/app/setup.go:42-48", etc.). Order matters for
	// the UPGRADE_NOTES.md output — keep insertion order.
	Auto []string

	// VerifyCommands are the commands the user should run after the
	// codemod completes. Defaults to the triple-gate when the
	// codemod doesn't override.
	VerifyCommands []string
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
//
// Currently no hop needs a codemod, so the registry is empty and nothing
// calls this — but the registry is live: runCodemodChain reads it on every
// `forge project upgrade`, and this is the only way to populate it. Deleting
// the registrar would leave a map nothing can ever fill and make the chain
// dead by construction; the next version hop that needs an AST rewrite calls
// this from its init().
//
//nolint:unused // registration seam for codemodRegistry, which runCodemodChain reads on every upgrade.
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
	minor, err := atoiSafe(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return maj, minor, true
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
// The registry is sparse on purpose: most minor hops change templates and
// documentation, not the AST of user-owned files, so having no codemod for a
// hop is the common case and means "nothing deterministic to rewrite" — not a
// gap. Treating absence as an error would fail every minor bump until someone
// registered a no-op, which is a toll booth, not a safety property. The real
// safety property is upstream: the minor-hop guard in runUpgrade keeps a
// single run from spanning more than one minor, so each codemod that DOES
// exist still sees the clean baseline it was written against.
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
			continue
		}
		report, err := fn(projectDir)
		if err != nil {
			return merged, fmt.Errorf("codemod v%s -> v%s: %w", hopFrom, hopTo, err)
		}
		merged.Auto = append(merged.Auto, report.Auto...)
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
	fmt.Fprintf(&sb, "_Generated by `forge project upgrade` at %s. Review, then delete this file once the migration lands._\n\n",
		time.Now().UTC().Format(time.RFC3339))

	if len(report.Auto) > 0 {
		sb.WriteString("## Auto-applied changes\n\n")
		sb.WriteString("These changes were applied by `forge project upgrade`'s codemod runner. They are deterministic — re-running the upgrade is idempotent.\n\n")
		for _, line := range report.Auto {
			fmt.Fprintf(&sb, "- %s\n", line)
		}
		sb.WriteString("\n")
	} else {
		sb.WriteString("## Auto-applied changes\n\n")
		sb.WriteString("_None — the codemod found no patterns to rewrite. (Either the project was already in v" + normalizeMinor(toVersion) + " shape, or the codemod for this hop is informational-only.)_\n\n")
	}

	sb.WriteString("## Verification\n\n")
	sb.WriteString("Run the following from the project root after applying any manual changes above:\n\n")
	sb.WriteString("```bash\n")
	for _, c := range verify {
		fmt.Fprintf(&sb, "%s\n", c)
	}
	sb.WriteString("```\n\n")
	// Point at the worklist command rather than a constructed skill path:
	// migration skills are per-RELEASE and pruned once a release ages out
	// of the supported window, so a path built from a version pair names a
	// skill that may never have existed. `upgrade list` always answers
	// correctly, including when the answer is "none".
	sb.WriteString("Clean compile + green tests + clean lint = upgrade done. Run `" + Name() +
		" project upgrade list` for the per-release migration skills this project still needs — they carry the intent-level story the codemod cannot.\n")

	return os.WriteFile(path, []byte(sb.String()), 0o644)
}
