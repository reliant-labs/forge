package config

// DeriveProjectKind infers the project kind from its components — kind is no
// longer a forge.yaml field. The components themselves are derived from the
// project's real sources (proto descriptor, the pkg/app service registry, the
// deploy/kcl tree, internal/handlers, and cmd/ binaries), not an authored
// manifest. The rule:
//
//   - any server/worker/cron/operator component  → "service"
//     (server-shaped: they need handlers/bootstrap/deploy)
//   - only binary component(s), no server-shaped  → "cli"
//     (a binary-only project is a cobra CLI — the binary IS the cli main)
//   - no components, but a service marker present  → "service"
//     (the canonical "service shell": `forge new` with no --service. The
//     binary boots an empty appkit table and the user grows it with
//     `forge add service`.)
//   - no components at all                         → "library"
//     (a pure Go module with no buildable entrypoint)
//
// hasComponentsFile distinguishes the empty-service-shell (service marker
// present, zero entries → service) from a pure library (no marker → library).
// It is the ONE filesystem fact the kind decision needs that the slice alone
// can't carry; the loader supplies it.
//
// An unknown/empty component kind counts as server (EffectiveKind defaults
// to server), so it pulls the project toward "service".
func DeriveProjectKind(components []ComponentConfig, hasComponentsFile bool) string {
	if len(components) == 0 {
		if hasComponentsFile {
			return ProjectKindService
		}
		return ProjectKindLibrary
	}
	sawBinary := false
	for _, c := range components {
		switch c.EffectiveKind() {
		case ComponentKindBinary:
			sawBinary = true
		default:
			// server/worker/cron/operator — server-shaped → service.
			return ProjectKindService
		}
	}
	if sawBinary {
		return ProjectKindCLI
	}
	return ProjectKindService
}
