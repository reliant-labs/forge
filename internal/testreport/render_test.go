package testreport_test

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/testreport"
)

func render(a testreport.Analysis) string {
	var buf bytes.Buffer
	testreport.Render(&buf, a)
	return buf.String()
}

func TestRender_StatusHeadlines(t *testing.T) {
	policy := testreport.Policy{MaxSkipRatio: 0.5, MinTests: 5}
	totals := testreport.Totals{Packages: 2, Tests: 30, Passed: 24, Skipped: 6, SkipRatio: 0.2}
	skip := testreport.Finding{Package: "ex.com/a", Kind: testreport.KindMassSkip, Tests: 10, Skipped: 8, Ratio: 0.8}
	failed := testreport.Finding{Package: "ex.com/b", Kind: testreport.KindFailed, Detail: "2 test(s) failed"}

	tests := []struct {
		name     string
		a        testreport.Analysis
		want     []string
		wantNone []string
	}{
		{
			name:     "pass says how much ran and that nothing skipped heavily",
			a:        testreport.Analysis{Policy: policy, Totals: totals},
			want:     []string{"✅ 2 package(s), 30 test(s): 24 passed, 6 skipped (20.0%).", "No package skipped more than 50% of its tests.\n"},
			wantNone: []string{"declared below"},
		},
		{
			name: "a pass over excused packages does not claim nothing skipped heavily",
			a: testreport.Analysis{Policy: policy, Totals: totals, Suppressed: []testreport.Suppressed{
				{Package: "ex.com/slow", Tests: 10, Skipped: 9, Exemption: testreport.Exemption{Package: "ex.com/slow", Reason: "needs docker"}},
			}},
			want: []string{
				"apart from the 1 declared below.",
				"1 package(s) skipped heavily as DECLARED in forge.yaml (ci.test_skips.allow)",
				"ex.com/slow (9/10 skipped) — needs docker",
			},
			wantNone: []string{"No package skipped more than 50% of its tests.\n"},
		},
		{
			name: "not applicable",
			a:    testreport.Analysis{Policy: policy},
			want: []string{"No tests in this run — nothing to judge."},
		},
		{
			name:     "undetermined input is never called a pass",
			a:        testreport.Analysis{Policy: policy, Undetermined: []testreport.Unknown{{Reason: "the input carried no `go test -json` events"}}},
			want:     []string{"UNDETERMINED — forge could not obtain the facts (this is NOT a pass):", "the input carried no `go test -json` events"},
			wantNone: []string{"✅"},
		},
		{
			name: "skip findings only",
			a:    testreport.Analysis{Policy: policy, Totals: totals, Findings: []testreport.Finding{skip}},
			want: []string{"❌ 1 package(s) skipped so much that their pass proves nothing:", " 80%  ex.com/a — 8 of 10 tests skipped (mass-skip)"},
		},
		{
			name: "failures only",
			a:    testreport.Analysis{Policy: policy, Totals: totals, Findings: []testreport.Finding{failed}},
			want: []string{"❌ 1 package(s) failed:", "FAIL  ex.com/b — 2 test(s) failed"},
		},
		{
			name: "both",
			a:    testreport.Analysis{Policy: policy, Totals: totals, Findings: []testreport.Finding{skip, failed}},
			want: []string{"❌ 1 package(s) skipped most of their tests, and 1 failed:"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := render(tt.a)
			for _, w := range tt.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tt.wantNone {
				if strings.Contains(out, w) {
					t.Errorf("output must not contain %q:\n%s", w, out)
				}
			}
		})
	}
}

func TestRender_FindingDetails(t *testing.T) {
	zero := testreport.Finding{
		Package: "ex.com/z", Kind: testreport.KindZeroEvidence, Tests: 12, Skipped: 12, Ratio: 1,
		Reasons: []testreport.ReasonCount{{Reason: "DATABASE_URL not set", Count: 12}},
	}
	named := testreport.Finding{
		Package: "ex.com/n", Kind: testreport.KindMassSkip, Tests: 10, Skipped: 9, Ratio: 0.9,
		Examples: []string{"TestA", "TestB"},
	}
	out := render(testreport.Analysis{
		Policy:   testreport.DefaultPolicy(),
		Totals:   testreport.Totals{Packages: 2, Tests: 22},
		Findings: []testreport.Finding{zero, named},
	})
	for _, want := range []string{
		"100%  ex.com/z — 12 of 12 tests skipped (zero-evidence: nothing in this package ran, so its pass proves nothing)",
		`12× "DATABASE_URL not set"`,
		"e.g. TestA, TestB",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "e.g. ") && strings.Index(out, "e.g. ") < strings.Index(out, "ex.com/n") {
		t.Errorf("examples must follow the package they belong to:\n%s", out)
	}
}

func TestRender_Notes(t *testing.T) {
	out := render(testreport.Analysis{
		Policy:    testreport.DefaultPolicy(),
		Totals:    testreport.Totals{Packages: 1, Tests: 10, Passed: 10, NoTestFiles: 3, NoTestsRan: 2},
		Stale:     []testreport.Exemption{{Package: "ex.com/healed", Reason: "was slow"}},
		Malformed: 4,
	})
	for _, want := range []string{
		"3 package(s) have no test files",
		"1 declared skip exemption(s) are no longer needed",
		`ex.com/healed ("was slow") — remove it from forge.yaml (ci.test_skips.allow)`,
		"4 input line(s) were not `go test -json` events and were ignored.",
		"2 package(s) compiled but ran no test",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRender_UndeterminedAlongsideFindings(t *testing.T) {
	out := render(testreport.Analysis{
		Policy:   testreport.DefaultPolicy(),
		Totals:   testreport.Totals{Packages: 1, Tests: 10},
		Findings: []testreport.Finding{{Package: "ex.com/a", Kind: testreport.KindFailed, Detail: "boom"}},
		Undetermined: []testreport.Unknown{
			{Package: "ex.com/cut", Reason: "stream ended before the package finished"},
			{Reason: "a whole-run problem"},
		},
		Malformed: 2,
	})
	for _, want := range []string{
		"UNDETERMINED — forge could not obtain these facts (not a pass):",
		"ex.com/cut: stream ended before the package finished",
		"  a whole-run problem",
		"2 input line(s) were not",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRender_ListsAreCapped(t *testing.T) {
	var supp []testreport.Suppressed
	var stale []testreport.Exemption
	var unknown []testreport.Unknown
	for i := range 23 {
		supp = append(supp, testreport.Suppressed{Package: fmt.Sprintf("ex.com/s%02d", i), Tests: 10, Skipped: 9})
		stale = append(stale, testreport.Exemption{Package: fmt.Sprintf("ex.com/t%02d", i), Reason: "r"})
		unknown = append(unknown, testreport.Unknown{Package: fmt.Sprintf("ex.com/u%02d", i), Reason: "cut"})
	}
	out := render(testreport.Analysis{
		Policy:       testreport.DefaultPolicy(),
		Totals:       testreport.Totals{Packages: 1, Tests: 10},
		Suppressed:   supp,
		Stale:        stale,
		Undetermined: unknown,
	})
	if got := strings.Count(out, "... and 3 more."); got != 3 {
		t.Errorf("want each of the three lists capped with %q, found %d:\n%s", "... and 3 more.", got, out)
	}
	for _, leaked := range []string{"ex.com/s20", "ex.com/t20", "ex.com/u20"} {
		if strings.Contains(out, leaked) {
			t.Errorf("%s is past the cap and must not be listed", leaked)
		}
	}
}

func TestPackageSkipRatio(t *testing.T) {
	tests := []struct {
		p    testreport.Package
		want float64
	}{
		{testreport.Package{}, 0},
		{testreport.Package{Tests: 4, Skipped: 1}, 0.25},
		{testreport.Package{Tests: 4, Skipped: 4}, 1},
	}
	for _, tt := range tests {
		if got := tt.p.SkipRatio(); got != tt.want {
			t.Errorf("SkipRatio(%+v) = %v, want %v", tt.p, got, tt.want)
		}
	}
}

func TestAnalyze_PartialPolicyGetsDefaults(t *testing.T) {
	// 6 of 10 skipped: reported at the default 0.5 ceiling. The zero MinTests
	// defaults to 5, so the 4-test package below the floor is judged only by
	// the zero-evidence rule (it passed nothing), and ex.com/p by the ceiling.
	in := stream("ex.com/p", nTests(4, 6), "pass") + stream("ex.com/tiny", nTests(0, 4), "pass") + stream("ex.com/ok", nTests(3, 1), "pass")
	a := analyze(t, in, testreport.Policy{})
	if d := testreport.DefaultPolicy(); a.Policy.MaxSkipRatio != d.MaxSkipRatio || a.Policy.MinTests != d.MinTests {
		t.Errorf("an empty policy must normalise to the defaults, got %+v", a.Policy)
	}
	kinds := map[string]testreport.Kind{}
	for _, f := range a.Findings {
		kinds[f.Package] = f.Kind
	}
	want := map[string]testreport.Kind{"ex.com/p": testreport.KindMassSkip, "ex.com/tiny": testreport.KindZeroEvidence}
	if len(kinds) != len(want) || kinds["ex.com/p"] != want["ex.com/p"] || kinds["ex.com/tiny"] != want["ex.com/tiny"] {
		t.Errorf("finding kinds = %v, want %v (ex.com/ok is below the ceiling and unreported)", kinds, want)
	}
}

func TestAnalyze_OnlyMaxSkipRatioSet(t *testing.T) {
	a := analyze(t, stream("ex.com/p", nTests(4, 6), "pass"), testreport.Policy{MaxSkipRatio: 0.9})
	if a.Policy.MaxSkipRatio != 0.9 || a.Policy.MinTests != testreport.DefaultPolicy().MinTests {
		t.Errorf("policy = %+v, want MaxSkipRatio kept and MinTests defaulted", a.Policy)
	}
	if len(a.Findings) != 0 {
		t.Errorf("60%% skipped is under a 90%% ceiling, got %+v", a.Findings)
	}
}

func TestAnalyze_FailedPackageWithNoFailingTestStillCountsOne(t *testing.T) {
	// A TestMain that exits non-zero fails the package with no failing test.
	in := stream("ex.com/main", nTests(3, 0), "fail")
	a := analyze(t, in, testreport.DefaultPolicy())
	if a.Status() != testreport.StatusFail {
		t.Fatalf("status = %q, want fail", a.Status())
	}
	if len(a.Findings) != 1 || a.Findings[0].Kind != testreport.KindFailed {
		t.Fatalf("findings = %+v", a.Findings)
	}
	if !strings.Contains(a.Findings[0].Detail, "1") {
		t.Errorf("detail %q must count at least one failure", a.Findings[0].Detail)
	}
}

func TestAnalyze_ExamplesAreCappedAndReasonsOrdered(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"Action":"start","Package":"ex.com/p"}` + "\n")
	for i := range 10 {
		name := fmt.Sprintf("TestS%02d", i)
		b.WriteString(fmt.Sprintf(`{"Action":"run","Package":"ex.com/p","Test":%q}`+"\n", name))
		b.WriteString(fmt.Sprintf(`{"Action":"skip","Package":"ex.com/p","Test":%q}`+"\n", name))
	}
	b.WriteString(`{"Action":"skip","Package":"ex.com/p","Elapsed":1}` + "\n")
	a := analyze(t, b.String(), testreport.DefaultPolicy())
	if len(a.Findings) != 1 {
		t.Fatalf("findings = %+v", a.Findings)
	}
	if n := len(a.Findings[0].Examples); n == 0 || n > 5 {
		t.Errorf("examples = %v, want a short non-empty list", a.Findings[0].Examples)
	}
}

func TestService_DelegatesToThePackageFunctions(t *testing.T) {
	svc := testreport.New(testreport.Deps{})
	in := stream("ex.com/ok", nTests(6, 0), "pass")

	run, err := svc.Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	a := svc.Analyze(run, testreport.Policy{})
	if a.Status() != testreport.StatusPass || a.Totals.Tests != 6 {
		t.Errorf("Analyze = status %q totals %+v", a.Status(), a.Totals)
	}
	var buf bytes.Buffer
	svc.Render(&buf, a)
	if !strings.Contains(buf.String(), "✅ 1 package(s), 6 test(s): 6 passed, 0 skipped (0.0%).") {
		t.Errorf("Render wrote %q", buf.String())
	}

	if _, err := svc.Parse(strings.NewReader("")); err == nil {
		t.Error("an empty stream must be an error through the service too")
	}
}
