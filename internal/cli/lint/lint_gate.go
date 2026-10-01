package lint

// `forge lint --gate-json <file>` — lint's result as recordable evidence
// (control-plane docs/design/hosted-deploy-primitives.md §3.3).
//
// A FILE FLAG, NOT A STDOUT MODE, and that distinction is the design. lint's
// human output and its exit code are UNTOUCHED: CI keeps branching on the
// exit code, a human keeps reading the findings, and the gate document is
// written beside them. A `--json`-style stdout mode would make the report and
// the evidence mutually exclusive, so a pipeline wanting both would have to
// lint twice — and the second run is a different run, which makes the
// evidence about neither.
//
// ONE LINT RUN, AND THE EVIDENCE COMES FROM WHAT THAT RUN PROVED. The gate is
// derived from the LANE TALLY the text driver already computes — which gating
// lanes ran, which were skipped, and which could not run — not from a second
// structured collection. Two reasons, and the first is not the important one:
//
//  1. A second pass would double the wall clock, and lint's slowest lane
//     (golangci-lint) takes an exclusive lock on one shared path, so two
//     passes can also serialize against each other.
//  2. The lane tally is the RIGHT verdict. The three outcomes lint
//     distinguishes — did not apply, ran, could NOT run — are exactly the
//     distinction §3.3 needs, and the one the `-unavailable` rule family
//     exists to preserve. A run where every lane failed to execute must
//     record as `skipped`, never `passed`: "could not run" is not "found
//     nothing wrong", and a gate claiming a pass over zero executed lanes is
//     the silent-green this whole primitive is about.
//
// So the gate says what the run PROVED, in the same terms the final human
// line says it.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/config"
)

// lintGateName is what the recorded check is called. The verb's own
// vocabulary, so an evidence trail reads "lint" rather than a command line.
const lintGateName = "lint"

// gateVerdict is the verdict lint reports, in the three values it can
// produce.
//
// Deliberately NOT release.GateStatus. That type lives behind pkg/release,
// and importing the release vocabulary into the linter for three constants
// would make the gate format part of lint's contract — lint is a leaf the CLI
// composes, and the dependency runs that way. The CLI maps these onto the
// closed set at the boundary (one switch, in gate_producers.go).
type gateVerdict string

const (
	gateVerdictPassed gateVerdict = "passed"
	gateVerdictFailed gateVerdict = "failed"
	// gateVerdictSkipped is a run that PROVED NOTHING — no gating lane
	// executed. Not a pass; see the file header.
	gateVerdictSkipped gateVerdict = "skipped"
)

// GateRequest is one completed check, as the CLI needs it to write a gate
// document. A struct rather than six parameters because it crosses a package
// boundary and every field is optional-ish detail about the same event.
type GateRequest struct {
	// Path is where the document is written.
	Path string
	// Name is the check: "lint".
	Name string
	// Status is the verdict.
	Status string
	// Summary is the one line a reader sees.
	Summary string
	// Details is small and structured — counts, never logs.
	Details map[string]any
	// StartedAt and FinishedAt are the CHECK's own window, which is what
	// makes a gate a run stage with a duration.
	StartedAt  time.Time
	FinishedAt time.Time
}

// GateWriter writes a gate document.
//
// Declared HERE, at the consumer, rather than exported from the CLI: this
// package needs "something that can write a gate", and that one-method
// contract is all it is told about the gate vocabulary.
type GateWriter func(req GateRequest) error

// writeGate is installed by the CLI package. nil means a build with no gate
// support wired, which only happens when this package is tested alone.
var writeGate GateWriter

// errGateWriterNotInstalled is the answer when --gate-json is asked for in a
// build where nothing installed a writer.
var errGateWriterNotInstalled = errors.New(
	"--gate-json is not wired in this build: no gate writer was installed")

// SetGateWriter installs the gate writer. Called once, from the CLI package.
func SetGateWriter(w GateWriter) { writeGate = w }

// lintLaneTally is what a lint run proved, in lanes.
//
// This is the whole input to the verdict, and it is the same data the final
// human line is rendered from (reportLintVerdict), so the document and the
// last line on screen cannot disagree.
type lintLaneTally struct {
	// ran are the gating lanes that executed — whether or not they found
	// problems. Finding problems IS coverage.
	ran []string
	// skipped are gating lanes that did not APPLY here: no proto tree, a
	// disabled feature. Compatible with a pass.
	skipped []string
	// unavailable are lanes that were supposed to execute and could not.
	// A hole in the coverage, and the reason `skipped` and `passed` are
	// different values.
	unavailable []string
	// failed reports whether any lane reported problems (or, under
	// --strict, was unavailable) — the same bool that decides the exit
	// code.
	failed bool
}

// emitLintGate writes the gate document for a completed lint run.
//
// The verdict is derived from the tally, and `failed` is the same bool the
// exit code is derived from, so the file and the process status agree by
// construction rather than by two switches someone has to keep in step.
func emitLintGate(path string, tally lintLaneTally, startedAt time.Time) error {
	if writeGate == nil {
		return errGateWriterNotInstalled
	}

	status := gateVerdictPassed
	switch {
	case tally.failed:
		status = gateVerdictFailed
	case len(tally.ran) == 0:
		// NO GATING LANE RAN. The text driver already refuses to call
		// this a pass ("this run proved nothing"), and the gate must
		// not either — recording `passed` here would put a green check
		// in the evidence trail for a run that checked nothing at all.
		status = gateVerdictSkipped
	}

	return writeGate(GateRequest{
		Path:       path,
		Name:       lintGateName,
		Status:     string(status),
		Summary:    lintGateSummary(status, tally),
		Details:    lintGateDetails(tally),
		StartedAt:  startedAt,
		FinishedAt: time.Now(),
	})
}

// lintGateSummary is the one line. It NAMES the lanes that did not run,
// because that is the part a reader of the evidence cannot reconstruct and
// the part a bare count would hide.
func lintGateSummary(status gateVerdict, tally lintLaneTally) string {
	var b strings.Builder
	switch status {
	case gateVerdictSkipped:
		b.WriteString("no gating linter ran — this run proved nothing")
	case gateVerdictFailed:
		fmt.Fprintf(&b, "%d gating linter(s) ran; one or more reported errors", len(tally.ran))
	default:
		fmt.Fprintf(&b, "%d gating linter(s) passed", len(tally.ran))
	}
	if len(tally.unavailable) > 0 {
		fmt.Fprintf(&b, "; %d could NOT run (%s)", len(tally.unavailable), strings.Join(tally.unavailable, ", "))
	}
	if len(tally.skipped) > 0 {
		fmt.Fprintf(&b, "; %d skipped (%s)", len(tally.skipped), strings.Join(tally.skipped, ", "))
	}
	return b.String()
}

// runLintWithGate runs lint exactly as it would have run, and writes the
// gate document from what that ONE run proved.
//
// The exit code is the run's own error, returned unchanged — the flag adds a
// file and changes nothing else. A gate-write failure is reported, because
// silently not producing the evidence a pipeline asked for is the failure
// mode this whole primitive exists to remove; but it never masks a lint
// error, which is the more important of the two.
func runLintWithGate(ctx context.Context, flags lintFlags, paths []string, gatePath string) error {
	startedAt := time.Now()
	tally, lintErr := runLintTallied(ctx, flags, paths)

	return resolveGateRunResult(lintErr, emitLintGate(gatePath, tally, startedAt))
}

// resolveGateRunResult decides which of the two failures becomes the exit
// status.
//
// THE LINT RESULT OUTRANKS A GATE-WRITE PROBLEM. A caller branching on the
// exit code is asking "is the code clean", and answering with "the evidence
// file could not be written" would be a different question — one that, worse,
// is indistinguishable from a lint failure at exit 1. So a lint error wins.
//
// But a gate-write failure on an otherwise clean run DOES surface, rather
// than being swallowed: a pipeline that asked for evidence and silently got
// none is the failure mode this primitive exists to remove, and it would show
// up much later as an unexplained hole in a promotion's trail.
//
// A named function rather than two lines inline because it is a RULE, and the
// test that pins it should be naming the rule rather than running a whole
// lint.
func resolveGateRunResult(lintErr, gateErr error) error {
	if lintErr != nil {
		return lintErr
	}
	return gateErr
}

// runLintTallied is runLint, plus the lane tally when the run was the full
// sweep.
//
// A TARGETED LANE (`--contract`, `--db`, …) HAS NO LANE TALLY, because it is
// not the pipeline — it is one check, and `ran` is itself. Synthesising a
// tally of one keeps the verdict honest: the gate then records a pass or a
// failure for that lane, never `skipped`, because a targeted lane that was
// asked for and executed did prove something.
func runLintTallied(ctx context.Context, flags lintFlags, paths []string) (lintLaneTally, error) {
	if lane, targeted := targetedLaneName(flags); targeted {
		err := runLint(ctx, flags, paths)
		return lintLaneTally{ran: []string{lane}, failed: err != nil}, err
	}
	return runAllLinters(ctx, lintRunOptions{
		fix:           !flags.noFix,
		strict:        flags.strict,
		skipFrontends: flags.skipFrontends,
		paths:         paths,
		cfg:           lintConfigOrNil(),
	})
}

// targetedLaneName names the single lane a `--<linter>` flag selected, or
// reports targeted=false for the full sweep.
//
// A table rather than a chain of ifs, so adding a lane is adding a row — and
// so the set stays visibly the same set runLint dispatches on. A flag missing
// from here is not a correctness bug: it degrades to the full-sweep path,
// where the tally comes from the pipeline itself.
func targetedLaneName(flags lintFlags) (string, bool) {
	for _, lane := range []struct {
		on   bool
		name string
	}{
		{flags.contract, "contract"},
		{flags.exportedVars, "exported-vars"},
		{flags.migrationSafety, "migration-safety"},
		{flags.conventions, "conventions"},
		{flags.generatedDrift, "generated-drift"},
		{flags.frontendStores, "frontend-stores"},
		{flags.scaffolds, "scaffolds"},
		{flags.tests, "tests"},
		{flags.banners, "banners"},
		{flags.checkWorkarounds, "check-workarounds"},
		{flags.optionalDepsGuard, "optional-deps-guard"},
		{flags.configDeps, "config-deps"},
		{flags.columnMarkers, "column-markers"},
		{flags.crudFixtures, "crud-fixtures"},
		{flags.fixtureDrift, "fixture-drift"},
		{flags.timeBucketing, "time-bucketing"},
		{flags.protoMarkers, "proto-markers"},
		{flags.protoOptions, "proto-options"},
		{flags.createNullability, "create-nullability"},
		{flags.computedFields, "computed-fields"},
		{flags.readOnlyFields, "read-only-fields"},
		{flags.guardedFields, "guarded-fields"},
		{flags.vendoredProtos, "vendored-protos"},
		{flags.configReach, "config-reach"},
	} {
		if lane.on {
			return lane.name, true
		}
	}
	return "", false
}

// lintConfigOrNil loads the project config the way runLint's full-sweep path
// does, tolerating absence. A parse error is not swallowed: it surfaces from
// the sweep itself, which loads the config again and fails hard — so this
// returning nil cannot turn a broken forge.yaml into a silent default.
func lintConfigOrNil() *config.ProjectConfig {
	_, cfg, err := loadLintConfig()
	if err != nil {
		return nil
	}
	return cfg
}

// lintGateDetails is the structured payload: counts and the lane names.
// Small and bounded (the server caps details at 8 KiB) — the findings
// themselves stay in the human output and in `--json`.
func lintGateDetails(tally lintLaneTally) map[string]any {
	details := map[string]any{
		"lanes_ran":         len(tally.ran),
		"lanes_skipped":     len(tally.skipped),
		"lanes_unavailable": len(tally.unavailable),
	}
	// The NAMES only for the lanes whose absence changes what the gate
	// means. Listing every lane that ran would be the bulk of the payload
	// and tells a reader nothing they would act on.
	if len(tally.unavailable) > 0 {
		details["unavailable"] = tally.unavailable
	}
	if len(tally.skipped) > 0 {
		details["skipped"] = tally.skipped
	}
	return details
}
