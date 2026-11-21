package codemod

import (
	"regexp"
	"strings"
)

// The CI half of tests-capture: the scaffolded workflows under
// .github/workflows. A workflow a project wrote itself is the playbook's to
// describe, not this codemod's to edit.

const (
	tcCIWorkflow      = ".github/workflows/ci.yml"
	tcE2EWorkflow     = ".github/workflows/e2e.yml"
	tcReleaseWorkflow = ".github/workflows/release.yml"
)

var (
	ghStepsRe = regexp.MustCompile(`^( *)steps:[ \t]*$`)
	ghItemRe  = regexp.MustCompile(`^( *)- `)
	// ghLaneRunRe is a step line that runs a recorded lane: `run: task test`
	// (or test:integration, test:e2e) with any arguments, or that command as
	// a line of a `run: |` block. test:short and test:report are not lanes.
	ghLaneRunRe = regexp.MustCompile(`^\s*(?:run:\s*)?task (?:test|test:integration|test:e2e)(?:\s.*)?$`)
	ghJobRe     = regexp.MustCompile(`^  [A-Za-z0-9_-]+:[ \t]*$`)
	ghE2ERunRe  = regexp.MustCompile(`^(\s*)(run: )?task test:e2e[ \t]*$`)
)

func (r *tcRun) workflows(lanes tcLanes) {
	if lanes.report {
		r.reportSteps(tcCIWorkflow, tcBlock("ci_report"))
		r.reportSteps(tcE2EWorkflow, tcBlock("e2e_report"))
	}
	if lanes.e2e {
		r.k3dCollects(tcCIWorkflow)
		r.k3dCollects(tcE2EWorkflow)
	}
	if lanes.coverageFromRecord {
		r.coverageArtifact()
	}
	r.releaseGate(lanes.unit)
}

// reportSteps adds a `Report tests` step after every step that runs a lane.
func (r *tcRun) reportSteps(rel, block string) {
	src, ok := r.read(rel)
	if !ok || strings.Contains(src, "task test:report") {
		return
	}
	lines := strings.SplitAfter(src, "\n")
	type insert struct {
		at   int
		text string
	}
	var inserts []insert
	for i := 0; i < len(lines); i++ {
		m := ghStepsRe.FindStringSubmatch(strings.TrimRight(lines[i], "\n"))
		if m == nil {
			continue
		}
		stepsIndent := len(m[1])
		// The steps' list items, and where the list ends.
		item := -1
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			l := strings.TrimRight(lines[j], "\n")
			if strings.TrimSpace(l) == "" {
				continue
			}
			if tcIndent(l) <= stepsIndent {
				end = j
				break
			}
			if item < 0 {
				if im := ghItemRe.FindStringSubmatch(l); im != nil {
					item = len(im[1])
				}
			}
		}
		if item < 0 {
			continue
		}
		for start := i + 1; start < end; {
			l := strings.TrimRight(lines[start], "\n")
			if tcIndent(l) != item || !strings.HasPrefix(strings.TrimSpace(l), "- ") {
				start++
				continue
			}
			// The step runs to the next line at or left of its dash: the next
			// step, a comment above it, or the end of the list.
			stop := end
			for k := start + 1; k < end; k++ {
				kl := strings.TrimRight(lines[k], "\n")
				if strings.TrimSpace(kl) != "" && tcIndent(kl) <= item {
					stop = k
					break
				}
			}
			last := start
			runsLane := false
			for k := start; k < stop; k++ {
				kl := strings.TrimRight(lines[k], "\n")
				if strings.TrimSpace(kl) != "" {
					last = k
				}
				runsLane = runsLane || ghLaneRunRe.MatchString(kl)
			}
			if runsLane {
				text := tcReindent(block, 6, item)
				if last+1 < stop && stop < end {
					// Steps separated by a blank line: keep the separation.
					inserts = append(inserts, insert{at: stop, text: text + "\n"})
				} else {
					inserts = append(inserts, insert{at: last + 1, text: text})
				}
			}
			start = stop
		}
		i = end - 1
	}
	if len(inserts) == 0 {
		return
	}
	for n := len(inserts) - 1; n >= 0; n-- {
		in := inserts[n]
		lines = append(lines[:in.at], append([]string{in.text}, lines[in.at:]...)...)
	}
	r.set(rel, strings.Join(lines, ""), "a Report tests step after each test lane (task test:report -- --upload; continue-on-error, so it never fails the job)")
}

// k3dCollects points the e2e lane of a job that deploys the e2e env at that
// env (ENV=e2e), so it zeroes and collects the env's server coverage.
func (r *tcRun) k3dCollects(rel string) {
	src, ok := r.read(rel)
	if !ok {
		return
	}
	lines := strings.SplitAfter(src, "\n")
	var jobs []int
	inJobs := false
	for i, l := range lines {
		t := strings.TrimRight(l, "\n")
		if t == "jobs:" {
			inJobs = true
			continue
		}
		if inJobs && ghJobRe.MatchString(t) {
			jobs = append(jobs, i)
		}
	}
	changed := false
	// Last job first: an inserted comment line shifts every job after it.
	for n := len(jobs) - 1; n >= 0; n-- {
		start := jobs[n]
		stop := len(lines)
		if n+1 < len(jobs) {
			stop = jobs[n+1]
		}
		if !strings.Contains(strings.Join(lines[start:stop], ""), "forge env deploy e2e") {
			continue
		}
		for i := stop - 1; i >= start; i-- {
			m := ghE2ERunRe.FindStringSubmatch(strings.TrimRight(lines[i], "\n"))
			if m == nil {
				continue
			}
			lines[i] = strings.TrimRight(lines[i], " \t\n") + " ENV=e2e\n"
			changed = true
			if m[2] != "" {
				// `run: task test:e2e`: say why above it, as a new scaffold does.
				if i > 0 && !strings.HasPrefix(strings.TrimSpace(lines[i-1]), "#") {
					lines = append(lines[:i], append([]string{tcReindent(tcBlock("e2e_k3d_comment"), 8, len(m[1]))}, lines[i:]...)...)
				}
				continue
			}
			// A line of `run: |`: the scaffold's comment above that block
			// gains the reason.
			for k := i - 1; k >= start && k >= i-3; k-- {
				if lines[k] == tcBlock("ci_k3d_comment_old") {
					lines[k] = tcBlock("ci_k3d_comment_new")
					break
				}
			}
		}
	}
	if changed {
		r.set(rel, strings.Join(lines, ""), "the k3d e2e lane runs task test:e2e ENV=e2e, collecting the e2e env's server coverage")
	}
}

// coverageArtifact follows `task coverage` onto coverage.html: it no longer
// writes coverage.out.
func (r *tcRun) coverageArtifact() {
	src, ok := r.read(tcCIWorkflow)
	if !ok || !strings.Contains(src, "path: coverage.out") {
		return
	}
	old := tcBlock("ci_coverage_old")
	if strings.Count(src, old) != 1 {
		r.next("%s uploads coverage.out, which `task coverage` no longer writes (it renders coverage.html from the recorded run): point the coverage artifact's path at coverage.html", tcCIWorkflow)
		return
	}
	r.set(tcCIWorkflow, strings.Replace(src, old, tcBlock("ci_coverage_new"), 1), "the coverage artifact is the coverage.html task coverage renders")
}

// releaseGate moves the release's tests gate onto the record: `forge test
// report --gate-json`, the document `forge ci verify-test-run` wrote.
func (r *tcRun) releaseGate(unitRecorded bool) {
	src, ok := r.read(tcReleaseWorkflow)
	if !ok || !strings.Contains(src, "verify-test-run") || strings.Contains(src, "forge test report --gate-json") {
		return
	}
	old := tcBlock("release_test_old")
	if strings.Count(src, old) != 1 {
		r.next("%s gates on `forge ci verify-test-run` in a step that is not the scaffolded one, so it was left as is (it keeps working). "+
			"To gate on the record instead: run `task test`, then `forge test report --gate-json gates/test.json`, and exit with the test's status (forge skill load migrations/v1.9.0/tests, section \"Release gate\")", tcReleaseWorkflow)
		return
	}
	if !unitRecorded {
		r.next("%s still gates on `forge ci verify-test-run`: `task test` records nothing, and a gate from an empty record is undetermined. Record the lane first, then swap the step (forge skill load migrations/v1.9.0/tests, section \"Release gate\")", tcReleaseWorkflow)
		return
	}
	if pin, old := r.pinPredatesForgeTest(); old {
		r.next("%s still gates on `forge ci verify-test-run`: forge.yaml pins forge %s, which has no `forge test report`. Move the pin to %s or later (`forge project upgrade`), then re-run `forge fix tests-capture`", tcReleaseWorkflow, pin, testsCaptureRelease)
		return
	}
	r.set(tcReleaseWorkflow, strings.Replace(src, old, tcBlock("release_test_new"), 1), "the tests gate comes from the recorded run (forge test report --gate-json) instead of re-reading go test -json")
}
