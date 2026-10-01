package lint

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// recordGateWriter captures what was written, so the verdict can be asserted
// without a filesystem or the CLI package's document format.
type recordGateWriter struct {
	got  GateRequest
	hits int
	err  error
}

func (w *recordGateWriter) write(req GateRequest) error {
	w.hits++
	w.got = req
	return w.err
}

func withGateWriter(t *testing.T, w *recordGateWriter) {
	t.Helper()
	previous := writeGate
	writeGate = w.write
	t.Cleanup(func() { writeGate = previous })
}

// THE VERDICT COMES FROM WHAT THE RUN PROVED, in lanes — and the case that
// matters most is the last one: a run where nothing executed is `skipped`,
// never `passed`. A gate claiming a pass over zero executed lanes is exactly
// the silent-green this primitive exists to prevent, and it is the same
// judgement the text driver's final line already makes.
func TestEmitLintGate_VerdictFromTheLaneTally(t *testing.T) {
	cases := []struct {
		name       string
		tally      lintLaneTally
		wantStatus gateVerdict
		wantInSum  string
	}{
		{
			name:       "every lane passed",
			tally:      lintLaneTally{ran: []string{"golangci-lint", "buf", "contract"}},
			wantStatus: gateVerdictPassed,
			wantInSum:  "3 gating linter(s) passed",
		},
		{
			name:       "a lane reported errors",
			tally:      lintLaneTally{ran: []string{"golangci-lint"}, failed: true},
			wantStatus: gateVerdictFailed,
			wantInSum:  "reported errors",
		},
		{
			name:       "NO lane ran — this proved nothing",
			tally:      lintLaneTally{unavailable: []string{"golangci-lint", "buf"}},
			wantStatus: gateVerdictSkipped,
			wantInSum:  "proved nothing",
		},
		{
			// A lane that could not run is a HOLE in the coverage, and
			// it must be named: a bare "2 passed" would read as a
			// clean gate while a check was silently absent.
			name: "a passing run still names the lanes that could not run",
			tally: lintLaneTally{
				ran: []string{"buf", "contract"}, unavailable: []string{"golangci-lint"},
			},
			wantStatus: gateVerdictPassed,
			wantInSum:  "1 could NOT run (golangci-lint)",
		},
		{
			// A SKIP is the project saying "this does not apply",
			// which is compatible with a pass — but still reported,
			// so a reader knows what was not covered.
			name: "skips are named but do not demote the verdict",
			tally: lintLaneTally{
				ran: []string{"golangci-lint"}, skipped: []string{"buf"},
			},
			wantStatus: gateVerdictPassed,
			wantInSum:  "1 skipped (buf)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &recordGateWriter{}
			withGateWriter(t, w)

			if err := emitLintGate("lint.json", tc.tally, time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if w.hits != 1 {
				t.Fatalf("wrote %d document(s), want 1", w.hits)
			}
			if w.got.Status != string(tc.wantStatus) {
				t.Errorf("status = %q, want %q", w.got.Status, tc.wantStatus)
			}
			if !strings.Contains(w.got.Summary, tc.wantInSum) {
				t.Errorf("summary = %q, want it to contain %q", w.got.Summary, tc.wantInSum)
			}
			if w.got.Name != lintGateName {
				t.Errorf("name = %q, want %q", w.got.Name, lintGateName)
			}
			if w.got.StartedAt.IsZero() || w.got.FinishedAt.IsZero() {
				t.Error("the check's window must be recorded")
			}
		})
	}
}

// The structured details carry the lane counts, and the NAMES only of the
// lanes whose absence changes what the gate means. Listing every lane that
// ran would be the bulk of an 8 KiB-capped payload and tells a reader nothing
// they would act on.
func TestEmitLintGate_DetailsNameOnlyWhatMatters(t *testing.T) {
	w := &recordGateWriter{}
	withGateWriter(t, w)

	tally := lintLaneTally{
		ran:         []string{"golangci-lint", "buf"},
		skipped:     []string{"frontend-lint"},
		unavailable: []string{"typed-config-guardrail"},
	}
	if err := emitLintGate("lint.json", tally, time.Now()); err != nil {
		t.Fatal(err)
	}
	d := w.got.Details
	if d["lanes_ran"] != 2 || d["lanes_skipped"] != 1 || d["lanes_unavailable"] != 1 {
		t.Fatalf("details = %v", d)
	}
	if _, named := d["unavailable"]; !named {
		t.Error("the unavailable lanes must be named")
	}
	if _, named := d["ran"]; named {
		t.Error("the lanes that ran should NOT be enumerated — it is bulk with no action attached")
	}
}

// A run with no gate writer installed says so rather than silently producing
// nothing: a pipeline that asked for evidence and got none must find out.
func TestEmitLintGate_UnwiredWriterIsAnError(t *testing.T) {
	previous := writeGate
	writeGate = nil
	t.Cleanup(func() { writeGate = previous })

	err := emitLintGate("lint.json", lintLaneTally{ran: []string{"buf"}}, time.Now())
	if !errors.Is(err, errGateWriterNotInstalled) {
		t.Fatalf("want errGateWriterNotInstalled, got %v", err)
	}
}

// THE EXIT CODE IS UNCHANGED BY --gate-json. This is §3.3's explicit
// requirement and the reason the flag is a file rather than a stdout mode:
// CI keeps branching on the exit code, so the lint result must pass through
// untouched even when the gate is written.
func TestRunLintWithGate_LintErrorOutranksTheGateWrite(t *testing.T) {
	// A gate writer that FAILS, over a lint run that also fails. The lint
	// error must win: a caller acting on the exit code has to see the lint
	// verdict, not a problem with the evidence file.
	w := &recordGateWriter{err: errors.New("disk full")}
	withGateWriter(t, w)

	lintErr := errors.New("one or more linters reported errors")
	got := resolveGateRunResult(lintErr, w.err)
	if got != lintErr {
		t.Fatalf("got %v, want the lint error to outrank the gate-write failure", got)
	}

	// With lint clean, a gate-write failure DOES surface — silently not
	// producing the evidence a pipeline asked for is the failure mode this
	// whole primitive exists to remove.
	if got := resolveGateRunResult(nil, w.err); got != w.err {
		t.Fatalf("got %v, want the gate-write failure to surface on a clean lint", got)
	}
	// Both clean is clean.
	if got := resolveGateRunResult(nil, nil); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
}

// A targeted lane (`--contract`, `--conventions`, …) is one check, not the
// pipeline, so its tally is itself — and the gate records a real verdict for
// it rather than `skipped`.
func TestTargetedLaneName(t *testing.T) {
	t.Parallel()
	if name, targeted := targetedLaneName(lintFlags{contract: true}); !targeted || name != "contract" {
		t.Errorf("contract: got %q, %v", name, targeted)
	}
	if name, targeted := targetedLaneName(lintFlags{conventions: true}); !targeted || name != "conventions" {
		t.Errorf("conventions: got %q, %v", name, targeted)
	}
	// No targeted flag means the full sweep, whose tally comes from the
	// pipeline itself.
	if _, targeted := targetedLaneName(lintFlags{strict: true}); targeted {
		t.Error("no --<linter> flag must report the full sweep")
	}
}

// --gate-json is declared, and it takes a FILE.
func TestLintCmd_DeclaresGateJSONAsAFileFlag(t *testing.T) {
	t.Parallel()
	cmd := newCmd(nil)
	flag := cmd.Flags().Lookup("gate-json")
	if flag == nil {
		t.Fatal("--gate-json is not declared on `forge lint`")
	}
	if flag.Value.Type() != "string" {
		t.Errorf("--gate-json must take a file path, got type %q", flag.Value.Type())
	}
	if !strings.Contains(flag.Usage, "not a stdout mode") {
		t.Errorf("the help must say it is a file, not a stdout mode: %q", flag.Usage)
	}
}
