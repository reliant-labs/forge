package cli

import (
	"fmt"
	"strings"
)

// forgeVersionMismatchWarning returns a warning string when the project's
// pinned forge_version (yamlVersion) doesn't match the running binary's
// version (binaryVersion), or empty if they agree / can't be compared.
//
// Three cases:
//
//  1. Project is missing forge_version entirely (legacy / pre-baseline) —
//     emit the "no forge_version declared" nudge so the user knows to run
//     `forge upgrade` to set a baseline.
//  2. Project has a forge_version that doesn't equal the binary version —
//     emit the migration warning.
//  3. Either side is "dev" / unset / empty in a way we can't compare —
//     stay silent. We don't want to spam during local development against
//     a tip-of-tree forge build.
func forgeVersionMismatchWarning(yamlVersion, binaryVersion string) string {
	yamlVersion = strings.TrimSpace(yamlVersion)
	binaryVersion = strings.TrimSpace(binaryVersion)

	// Case 3: silence when the binary version isn't a real release.
	// During local forge development the binary reports "dev" / "(devel)";
	// pestering every dogfood project owner over that is noise.
	if binaryVersion == "" || binaryVersion == "dev" || binaryVersion == "(devel)" {
		return ""
	}

	// Case 1: legacy project, no forge_version pinned. Treat as "0.0.0"
	// per EffectiveForgeVersion semantics.
	if yamlVersion == "" {
		return fmt.Sprintf("⚠️  no forge_version declared in forge.yaml — run '%s upgrade' to set baseline (binary is %s).", CLIName(), binaryVersion)
	}

	if yamlVersion == binaryVersion {
		return ""
	}

	return fmt.Sprintf("⚠️  forge.yaml declares forge_version: %s but binary is %s. Run '%s upgrade' to migrate.", yamlVersion, binaryVersion, CLIName())
}
