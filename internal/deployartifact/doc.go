// Package deployartifact fetches and unpacks the DESIRED-STATE ARTIFACT a
// reconcile loop compares an environment against.
//
// It is the read half of the same machinery control-plane's
// internal/buildservice writes, and it inherits that package's central
// discipline: AN ARTIFACT IS BYTES FROM OUTSIDE. buildservice treats the
// OCI layout its build pods produce as untrusted input even though it
// wrote it, and pushes it with a separate, hardened parser process. This
// package is the mirror image — it parses a layout it did NOT write — so
// the same caution applies with less excuse to skip it.
//
// # Why an artifact and not a live render
//
// The obvious design is to re-render the environment's KCL whenever the
// loop needs desired state. It is wrong, and the reason is drift:
//
//	rendered bytes = f(forge's version, the KCL schema, platform config, plugins)
//
// Every one of those inputs moves independently of the environment. A
// render performed today can therefore differ from the render that passed
// staging last week, with no change to the environment's own files — so a
// reconciler built on live renders would report drift caused by its own
// toolchain, and, worse, would CONVERGE the environment onto bytes nobody
// reviewed.
//
// A content-addressed artifact removes the function entirely. "What is
// this environment supposed to be running" has one immutable answer,
// fixed at the moment the release was cut, and the digest is the proof.
// This is the same argument migration 00070 makes for pinning promotions
// by digest rather than tag, and the same one StaticSiteProvider's
// per-digest release prefix makes for rollback: a mutable pointer cannot
// carry a guarantee.
//
// # Typed items, dispatched on kind
//
// An artifact carries ITEMS, each with a `kind`. The vocabulary is
// forge's existing one — oci | npm | gomod | file, open to more, with
// empty meaning oci — declared once in internal/cli's ReleaseArtifact and
// mirrored by control-plane migration 00075's kind column. This package
// spells it the same way rather than inventing a second set of tokens:
// three spellings of one vocabulary is how a ledger and its consumer
// drift into disagreeing about what an artifact IS.
//
// Unknown kinds round-trip untouched, deliberately, so an older forge
// reading a newer artifact does not corrupt it — and an unrecognised kind
// FAILS CLOSED (it is carried, not deployed) rather than being guessed at
// as oci.
//
// # Defensive unpack
//
// Everything under Unpack assumes the tar is hostile. See unpack.go for
// the specific measures and what each one closes; the short version is
// that a tar archive is a sequence of ATTACKER-CHOSEN PATHS and
// ATTACKER-CHOSEN LENGTHS, and both must be bounded before anything
// touches the filesystem.
//
// # Caching
//
// Cache by digest. An unchanged digest costs ONE HEAD request, not a
// pull. See cache.go — the property that makes this sound is that a
// digest names bytes, so a cache hit needs no freshness check beyond
// confirming the tag still resolves to the digest already held.
//
// forge:exclude-contract
// deployartifact is an inbound registry adapter (fetch + verify + unpack
// an OCI artifact), the same shape as the outbound internal/deploytarget
// providers that are already excluded. Its entry points are package-level
// funcs over a Fetcher interface declared at the consumer; there is no
// contract-shaped service here.
package deployartifact
