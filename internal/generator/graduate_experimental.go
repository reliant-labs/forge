// One-time forge.yaml migration for the features that graduated out of
// `features.experimental:`.
//
// ingress and operators are prod-critical — ingress is how a deployed
// service is reachable at all, operators back real controllers — and both
// were sitting behind an opt-in-and-be-warned gate. The visible cost was
// that every forge invocation in a project using them printed a warning
// about that project's own production configuration, forever. They are now
// ordinary top-level feature flags.
//
// external_builds was DELETED rather than graduated. It had already been
// reduced to an inert key no code path consulted: `Service.build_cmd` is the
// build-side mirror of `External.deploy_cmd`, and since `forge env deploy`
// of an External target never required an opt-in, gating `forge build` of
// the same target left the pair with mismatched maturity gates
// (fr-da9a6614fb).
//
// WHY MIGRATE RATHER THAN ACCEPT-AND-IGNORE. Leaving the nested keys
// readable would mean a forge.yaml that says `features.experimental.ingress:
// true` while forge reads `features.ingress` — a file whose text disagrees
// with the behaviour it produces. The next person to edit that file would
// set the key that does nothing. We have no backwards-compatibility promise
// to protect here, so the honest move is to rewrite the file once and leave
// it saying what is true.
//
// The rewrite is TEXTUAL and surgical, for the same reason kclmigrate is:
// a load-and-reserialize would drop the user's comments and key order. It
// moves each surviving key up to `features:`, drops external_builds, and
// removes the `experimental:` block when nothing is left in it.
package generator

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// GraduatedFeatures are the keys that moved from features.experimental up to
// features, in the order they are reported.
var GraduatedFeatures = []string{"ingress", "operators"}

// DeletedExperimentalFeatures are keys that were removed outright: the flag
// gated nothing by the time it was deleted, so there is no destination to
// move the value to.
var DeletedExperimentalFeatures = []string{"external_builds"}

// GraduateExperimentalResult describes what the migration did, so the
// caller can report it rather than rewriting a file silently.
type GraduateExperimentalResult struct {
	// Promoted names the keys moved up to `features:`, with their values.
	Promoted []string
	// Dropped names keys removed outright.
	Dropped []string
}

// Changed reports whether anything was rewritten.
func (r GraduateExperimentalResult) Changed() bool {
	return len(r.Promoted) > 0 || len(r.Dropped) > 0
}

var (
	// experimentalKeyRe matches the `experimental:` line inside features.
	experimentalKeyRe = regexp.MustCompile(`^(\s*)experimental:\s*$`)
	// featuresKeyRe matches the top-level `features:` line.
	featuresKeyRe = regexp.MustCompile(`^features:\s*$`)
	// nestedFlagRe matches a `<name>: <bool>` line, capturing both.
	nestedFlagRe = regexp.MustCompile(`^(\s*)([a-z_]+):\s*(true|false)\s*$`)
)

// GraduateExperimentalFeatures rewrites the forge.yaml at path so the
// graduated keys sit under `features:` and the deleted ones are gone.
//
// A file with no `features.experimental:` block, or one holding only keys
// that are still experimental, is left byte-identical and reports no change
// — the migration must be a no-op on a project that never opted in, because
// it runs on every generate.
func GraduateExperimentalFeatures(path string) (GraduateExperimentalResult, error) {
	var res GraduateExperimentalResult

	raw, err := os.ReadFile(path)
	if err != nil {
		return res, fmt.Errorf("read project config: %w", err)
	}
	lines := strings.Split(string(raw), "\n")

	expStart, expIndent := findExperimentalBlock(lines)
	if expStart < 0 {
		return res, nil
	}

	// Collect the block's own entries. The block ends at the first line
	// that is non-blank and indented at or above the `experimental:` key
	// itself — a sibling key, or the end of the file.
	type flag struct {
		line  int
		name  string
		value string
	}
	var flags []flag
	end := len(lines)
	for i := expStart + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if indentOf(lines[i]) <= expIndent {
			end = i
			break
		}
		if m := nestedFlagRe.FindStringSubmatch(lines[i]); m != nil {
			flags = append(flags, flag{line: i, name: m[2], value: m[3]})
		}
	}

	promote := map[string]string{}
	remove := map[int]bool{}
	for _, f := range flags {
		switch {
		case contains(GraduatedFeatures, f.name):
			promote[f.name] = f.value
			remove[f.line] = true
			res.Promoted = append(res.Promoted, fmt.Sprintf("features.experimental.%s → features.%s: %s", f.name, f.name, f.value))
		case contains(DeletedExperimentalFeatures, f.name):
			remove[f.line] = true
			res.Dropped = append(res.Dropped, fmt.Sprintf("features.experimental.%s (the flag gated nothing)", f.name))
		}
	}
	if !res.Changed() {
		return res, nil
	}

	// If every entry in the block is going away, the `experimental:` key
	// goes with it — an empty mapping key is not valid config, and leaving
	// `experimental:` with a nil value would re-read as something the
	// schema has to tolerate for no reason.
	if len(remove) == len(flags) {
		remove[expStart] = true
	}

	out := make([]string, 0, len(lines)+len(promote))
	for i, line := range lines {
		if remove[i] {
			continue
		}
		out = append(out, line)
		// Promoted keys are inserted immediately after the `features:`
		// line, at one indent step in. Appending at the top of the block
		// rather than at its end keeps the insertion independent of
		// whatever else the block holds.
		if featuresKeyRe.MatchString(line) && len(promote) > 0 {
			indent := strings.Repeat(" ", promotedIndent(lines, i, expIndent))
			for _, name := range GraduatedFeatures {
				if v, ok := promote[name]; ok {
					out = append(out, indent+name+": "+v)
				}
			}
			promote = map[string]string{}
		}
	}
	_ = end

	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return res, fmt.Errorf("write project config: %w", err)
	}
	return res, nil
}

// findExperimentalBlock returns the line index of the `experimental:` key
// nested under `features:`, and its indentation. Returns (-1, 0) when the
// project has no such block.
//
// It requires the enclosing `features:` because `experimental:` is not a
// reserved word — a user's own unrelated key of that name elsewhere in the
// file must not be rewritten.
func findExperimentalBlock(lines []string) (int, int) {
	inFeatures := false
	for i, line := range lines {
		if featuresKeyRe.MatchString(line) {
			inFeatures = true
			continue
		}
		if !inFeatures {
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		// A non-indented line ends the features block.
		if indentOf(line) == 0 {
			inFeatures = false
			continue
		}
		if m := experimentalKeyRe.FindStringSubmatch(line); m != nil {
			return i, len(m[1])
		}
	}
	return -1, 0
}

// promotedIndent picks the indentation for a key inserted directly under
// `features:`. It copies whatever the file already uses for that block (the
// `experimental:` key's own indent) so the rewrite matches the surrounding
// style rather than imposing one.
func promotedIndent(lines []string, featuresLine, expIndent int) int {
	if expIndent > 0 {
		return expIndent
	}
	for i := featuresLine + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		if n := indentOf(lines[i]); n > 0 {
			return n
		}
		break
	}
	return 4
}

func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
