// Package bundle produces and consumes the artifact that answers "exactly
// what bytes, from exactly which source" for one environment.
//
// Today it holds the PROJECTION half: [ProjectShape] turns one env's render
// into the [release.Shape] a ledger indexes — what runs, where, which
// secrets it needs, which domains it binds, and one hash per object. The
// packaging half (the OCI manifest, its manifest layer, Fetch/Unpack/Cache)
// lands here next, which is why the projection lives in its own package
// rather than in internal/cli: the same function has to be callable from the
// bundle writer, and a bundle writer that re-derived the shape could disagree
// with the command that recorded it.
//
// THE DEPENDENCY RUNS ONE WAY: internal/cli → internal/bundle, never back.
// This package knows nothing about KCL, cobra, a project on disk or a control
// plane. It takes [ShapeInput] — a rendered manifest stream plus the few
// facts about the env that the stream does not carry — and returns a value.
// That is what makes the projection testable from a literal fixture, and it
// is what stops "which cluster does this document land on" from being
// answered twice: the routing stays in internal/cluster, and this package
// reads the answer off the stream.
package bundle
