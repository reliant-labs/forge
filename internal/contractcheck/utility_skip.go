// File: internal/contractcheck/utility_skip.go
//
// Auto-skip for utility-shaped packages in the strict-contracts rule.
//
// The internal-package-contract-names rule (see
// internal_pkg_contract.go) enforces the canonical Service / Deps /
// New(Deps) Service trio on every internal package that has a
// contract.go. Pre-2026-06-04 it had ONE early-out — the
// "interface-catalogue" shape: >= 2 interfaces, no Deps, no New.
//
// FRICTION 2026-06-02 / 2026-06-03: the cp-forge migration shipped at
// least eight utility-shaped packages with a contract.go but no
// service-shape surface: internal/config, internal/metrics,
// internal/billing/provideradapters, internal/db, internal/planlimits,
// internal/ratelimit, internal/natsio, internal/daemonstate.
// The porter had to remember to add each path to forge.yaml's
// contracts.exclude. Two of those (natsio, daemonstate) are already
// covered by the interface-catalogue early-out; the rest are not, and
// added recurring friction to every migration.
//
// This file adds a second, more conservative early-out: when a
// package's non-test, non-gen .go files declare ZERO interfaces, the
// package cannot possibly have a `Service interface` and is treated
// as a utility (constants / structs / top-level funcs only). The
// strict-contracts rule skips it silently. No contracts.exclude entry
// required.
//
// The criterion is intentionally narrow:
//   - zero interfaces (the strongest "this is not a Service-shape
//     package" signal we can read off the AST without semantic
//     analysis)
//   - we DO NOT also key off "no Deps" / "no New" — a package that
//     forgot to declare a Service interface but DID declare Deps + New
//     is more likely an incomplete Service scaffold than a utility,
//     and the existing finding ("found no interface — rename to
//     'Service'") is the right surfaced action there.
//
// Together the two early-outs ("zero interfaces" here, ">= 2
// interfaces + no Deps/New" in internal_pkg_contract.go) cover the
// observed utility-package shapes without false-positiving on
// genuine incomplete Service packages (one interface, no Deps/New,
// where the missing Deps/New IS the bug we want to surface).
//
// Migrated from internal/linter/forgeconv/strict_contract_utility_skip.go
// on 2026-06-04 as part of the three-entry-point collapse. The
// helper signature is unchanged; only the package it lives in moved.

package contractcheck

import (
	"go/ast"
	"strings"
)

// isUtilityPackage reports whether a package's contract surface is
// "utility-shaped" — i.e. it has no interface declarations at all.
// Such packages cannot be Service-shaped by construction and should
// be auto-skipped by the strict-contracts lint.
//
// The single boolean flag is wired so the helper composes cleanly with
// the existing interface-catalogue early-out: a future rule that wants
// to broaden the auto-skip (say, "zero interfaces OR zero
// type-with-method declarations") only has to add another check to
// this helper.
func isUtilityPackage(interfaceCount int) bool {
	// Zero interfaces → cannot possibly satisfy `type Service interface`.
	// Honors the design note in the file header: we don't ALSO require
	// "no Deps / no New" — a package that has Deps + New but no
	// interface is most likely an incomplete Service, not a utility,
	// and the canonical "no interface — rename to 'Service'" finding
	// is the right action there.
	return interfaceCount == 0
}

// fileHasStrategyDirective reports whether f carries the
// `//forge:strategy` marker as a top-level comment. The directive
// opts the package out of the canonical Service/Deps/New enforcement
// for strategy-registry packages (one interface, many independently-
// constructed impls).
//
// Both the bare form (`//forge:strategy`) and the gofmt'd form
// (`// forge:strategy`) are accepted, matching the existing convention
// for `//forge:allow` and `//forge:optional-dep`.
func fileHasStrategyDirective(f *ast.File) bool {
	for _, group := range f.Comments {
		for _, c := range group.List {
			line := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
			if line == "forge:strategy" {
				return true
			}
		}
	}
	return false
}
