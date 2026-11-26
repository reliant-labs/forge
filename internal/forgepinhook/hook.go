// Package forgepinhook connects pkg/forgepin to the forge CLI that makes its
// decision, without pkg/forgepin importing the CLI.
//
// pkg/forgepin is the leaf a host imports for the forge a project pins. The
// decision behind it (which commands are handed to the pin, how a pin is
// compared with this forge, how the pinned forge is found or built) lives in
// internal/cli, whose import closure is the whole forge CLI: KCL, docker, the
// cloud SDKs, k8s. internal/cli sets Resolve when it is linked, so a host that
// embeds forge's CLI gets the real answer and a package that only names the
// types links none of that.
//
// This package must import only the standard library: pkg/forgepin's guard
// test walks into it.
package forgepinhook

import (
	"context"
	"io"
)

// Result is the part of a pinned forge Resolve answers: the executable, the
// cache key, the forge the project wants, and where the executable came from.
type Result struct {
	Path, Key, Want, Source string
}

// Resolve is internal/cli's ResolvePinnedForge, set by its init. Nil when the
// forge CLI is not linked into the binary.
var Resolve func(ctx context.Context, projectRoot, command string, log io.Writer) (Result, bool, error)
