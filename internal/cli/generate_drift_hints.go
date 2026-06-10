// Per-file extension-point hints for Tier-1 drift.
//
// When the stomp guard catches a hand-edited Tier-1 file, the worst
// thing the error can do is lead with --accept: agents take the path
// of least resistance, fork the file, and permanently lose
// regeneration (the failure chain this whole subsystem exists to
// prevent). The right answer is almost always "your customization has
// a designated user-owned home" — these hints name that home, per
// file shape, so the error message teaches the extension point first
// and the fork escape hatch last.
//
// The same mapping feeds `forge unfork --merge`'s post-merge guidance:
// a cleanly merged file is about to be re-rendered by the next
// generate, so surviving customizations need to move to the same
// destinations.
package cli

import (
	"path"
	"strings"
)

// tier1ExtensionPointHint returns the designated user-owned extension
// point for a Tier-1 path, or "" when the path has no specific
// mapping. relPath is project-relative with forward slashes.
func tier1ExtensionPointHint(relPath string) string {
	rel := strings.TrimPrefix(relPath, "./")
	base := path.Base(rel)
	dir := path.Dir(rel)

	// pkg/app wiring set: custom wiring has three designated homes the
	// bootstrap codegen already cooperates with.
	if dir == "pkg/app" {
		switch base {
		case "bootstrap.go", "app_gen.go", "wire_gen.go":
			return "custom wiring belongs in pkg/app/setup.go / post_bootstrap.go / app_extras.go (user-owned)"
		}
	}

	// handlers/<svc>/: per-service generated files each have a
	// user-owned counterpart or an upstream source of truth.
	if strings.HasPrefix(dir, "handlers/") {
		switch {
		case base == "authorizer_gen.go":
			svc := path.Base(dir)
			return "override in handlers/" + svc + "/authorizer.go (user-owned), which wire_gen already calls via NewAuthorizer()"
		case base == "handlers_gen.go" || base == "mock_gen.go":
			return "regenerate from contract.go / proto instead of editing — this file is derived output"
		}
	}

	return ""
}
