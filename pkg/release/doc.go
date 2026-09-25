// Package release is forge's release ledger vocabulary: what a RELEASE is
// (an immutable, content-addressed set of artifacts under a version label),
// what a PROMOTION is (one append-only event binding an environment to a
// release), and the invariants both must hold no matter where they are
// stored.
//
// # One model, two backends
//
// forge records releases and promotions in two places, and they are the
// SAME model:
//
//   - the file backend, inside a project: .forge/releases/<version>.json and
//     one append-only .forge/promotions/<env>.jsonl per environment;
//   - a hosted control plane, over its DeployService RPCs, when an
//     environment declares forge.ControlPlane.
//
// Both backends read and write these types, and both enforce the rules in
// this package. The hosted side additionally enforces them structurally in
// its database (CHECKs, append-only triggers); the Go rules here are the
// ones a directory of files can hold, and they are the acceptance test for
// the hosted side, not a weaker copy of it.
//
// # Why it is public
//
// A hosted ledger is implemented OUTSIDE forge (Reliant's control plane is
// one), and it must speak exactly this vocabulary or the two backends drift
// into two products. Declaring the types once, in an importable package, is
// what makes "the hosted ledger stores what forge cut" a compile-time fact
// rather than a comment in two repos.
//
// # Closed enums, validated on read
//
// [Kind], [Mode] and [PromotionKind] are CLOSED. An unknown or empty value
// fails to decode rather than defaulting to something plausible — an
// artifact whose kind nobody recognises must not be read as an image, and a
// promotion whose kind is unknown must not be read as a forward move. The
// earlier "empty kind means OCI" convention is gone: every ledger states
// what it holds.
package release
