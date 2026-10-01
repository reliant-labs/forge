package cli

// `promote --from <env> [--from-promotion <id>]`: promote exactly what another
// environment is running (control-plane docs/design/hosted-deploy-primitives.md
// §3.4).
//
// OWNED BY F5. F2 declares the flags and the hook so promote.go is never
// edited again; F5 replaces the body below, in this file.

import "errors"

// promoteFromOptions names the source of a promote.
type promoteFromOptions struct {
	// Env is the source environment (--from).
	Env string
	// PromotionID pins the source promotion captured earlier
	// (--from-promotion); the server refuses `source_moved` if the source
	// has moved past it.
	PromotionID string
}

func (o promoteFromOptions) requested() bool { return o.Env != "" || o.PromotionID != "" }

// promoteSource is what a resolved --from contributes to the write.
//
// F5 adds the provenance it records (the source env and promotion) as fields
// here AND sets them, and promote.go then copies them onto promoteWrite.
// They are deliberately absent until something can set them: a field that
// can only ever be zero is a claim nothing backs (deadcodeguard).
type promoteSource struct {
	// Version is the release to promote: the positional argument, or —
	// once F5 lands — the one the source environment runs.
	Version string
}

// errPromoteFromUnsupported is what --from returns until F5 wires it.
var errPromoteFromUnsupported = errors.New(
	"--from and --from-promotion are not supported by this forge build yet " +
		"(hosted-deploy-primitives task F5); promote by version")

// resolvePromoteFrom resolves the source before the plan is computed. version
// is the positional argument ("" when --from supplies it).
func resolvePromoteFrom(version string, o promoteFromOptions) (promoteSource, error) {
	if o.requested() {
		return promoteSource{}, errPromoteFromUnsupported
	}
	return promoteSource{Version: version}, nil
}
