package cli

// `promote --gate`: attach pre-promote evidence to the promotion entry
// (control-plane docs/design/hosted-deploy-primitives.md §3.3).
//
// OWNED BY F4. F2 declares the flag and the hook so promote.go is never
// edited again; F4 replaces the body below, in this file.

import (
	"errors"

	"github.com/reliant-labs/forge/pkg/release"
)

// errPromoteGatesUnsupported is what --gate returns until F4 wires it.
var errPromoteGatesUnsupported = errors.New(
	"--gate is not supported by this forge build yet (hosted-deploy-primitives task F4)")

// resolvePromoteGates turns the repeatable --gate values (a file path, or the
// inline name=…,status=… form) into the gates frozen onto the promotion.
// Runs before the plan, so a bad gate refuses the promote before any write.
func resolvePromoteGates(specs []string) ([]release.Gate, error) {
	if len(specs) > 0 {
		return nil, errPromoteGatesUnsupported
	}
	return nil, nil
}
