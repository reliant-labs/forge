package codemod

import (
	"regexp"
	"strings"
)

// The Taskfile half of tests-capture. A scaffolded Taskfile is YAML written
// at a fixed shape — tasks at two spaces, their keys at four, cmds at six —
// and every rewrite here anchors on that shape and on the scaffold's own
// lines, so a task the project restructured is reported, never guessed at.

// tcLanes is what the Taskfile records once the run is done: the workflow
// edits depend on it (a Report step needs test:report, the release gate
// needs a recorded `test`).
type tcLanes struct {
	unit, integration, e2e bool
	report                 bool
	coverageFromRecord     bool
}

const tcTaskfile = "Taskfile.yml"

// tfTask is one task's block: its header line through the line before the
// next task's header (or the end of the file), as byte offsets.
type tfTask struct {
	name       string
	start, end int
}

var (
	tfTasksLineRe  = regexp.MustCompile(`(?m)^tasks:[ \t]*$`)
	tfTaskHeaderRe = regexp.MustCompile(`(?m)^  ([A-Za-z0-9_.:-]+):[ \t]*$`)
	tfVarRe        = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^  ` + regexp.QuoteMeta(name) + `:`)
	}

	// tfGoTestRe is a scaffolded lane's one go test command: a list item at
	// six spaces, quoted or plain, optionally behind {{.SLOT}}, ending in the
	// -timeout and package arguments. Groups 1 and 5 are the quotes (they
	// must be equal), 2 the SLOT prefix, 3 the flags before -timeout (the
	// project's edits to them are kept), 4 -timeout and everything after it.
	tfGoTestRe = regexp.MustCompile(`(?m)^      - (["']?)((?:\{\{\.SLOT\}\} )?)go test((?: [^ "'\\\n]+)*?)( -timeout[ =][^"'\\\n]*?)(["']?)[ \t]*\n`)
)

func tfTasks(s string) []tfTask {
	loc := tfTasksLineRe.FindStringIndex(s)
	if loc == nil {
		return nil
	}
	var out []tfTask
	for _, m := range tfTaskHeaderRe.FindAllStringSubmatchIndex(s[loc[1]:], -1) {
		out = append(out, tfTask{name: s[loc[1]+m[2] : loc[1]+m[3]], start: loc[1] + m[0]})
	}
	for i := range out {
		if i+1 < len(out) {
			out[i].end = out[i+1].start
		} else {
			out[i].end = len(s)
		}
		// The header match starts at the "\n" before it when one precedes;
		// keep the block starting at the header's first byte.
		if s[out[i].start] == '\n' {
			out[i].start++
		}
	}
	return out
}

func tfFind(s, name string) (tfTask, bool) {
	for _, t := range tfTasks(s) {
		if t.name == name {
			return t, true
		}
	}
	return tfTask{}, false
}

func tfReplace(s string, t tfTask, body string) string { return s[:t.start] + body + s[t.end:] }

// tfInsertBefore puts a task block (ending in one newline) before task name,
// followed by the blank line that separates tasks.
func tfInsertBefore(s, name, block string) (string, bool) {
	t, ok := tfFind(s, name)
	if !ok {
		return s, false
	}
	return s[:t.start] + block + "\n" + s[t.start:], true
}

// tfInsertAfter puts a task block after task t.
func tfInsertAfter(s string, t tfTask, block string) string {
	if t.end < len(s) {
		return s[:t.end] + block + "\n" + s[t.end:]
	}
	return strings.TrimRight(s, "\n") + "\n\n" + block
}

func tfSameBlock(a, b string) bool { return strings.TrimRight(a, "\n") == strings.TrimRight(b, "\n") }

// tfIsService reports whether the Taskfile has the service kind's lanes (a
// frontend or e2e lane); cli and library Taskfiles have neither.
func tfIsService(s string) bool {
	_, fe := tfFind(s, "test:frontend")
	_, e2e := tfFind(s, "test:e2e")
	return fe || e2e
}

func (r *tcRun) taskfile() tcLanes {
	var lanes tcLanes
	s, ok := r.read(tcTaskfile)
	if !ok || tfTasksLineRe.FindStringIndex(s) == nil {
		return lanes
	}
	service := tfIsService(s)
	kind := "cli"
	if service {
		kind = "service"
	}

	// The lanes need the scaffold's vars: FORGE (the pinned forge),
	// FORGE_YAML (read for COVERAGE), SLOT and GOTESTRACE.
	if !strings.Contains(s, "\n  TEST_BEGIN:") {
		var missing []string
		for _, v := range []string{"FORGE", "FORGE_YAML", "SLOT", "GOTESTRACE"} {
			if !tfVarRe(v).MatchString(s) {
				missing = append(missing, v)
			}
		}
		if len(missing) > 0 {
			r.next("%s has no %s var, which the recorded lanes run through, so no lane was rewritten. "+
				"Record them by hand: `forge skill load migrations/v1.9.0/tests`, section \"Taskfile lanes\"", tcTaskfile, strings.Join(missing, ", "))
			return lanes
		}
		next, ok := tfInsertVars(s, tcBlock("taskfile_vars_"+kind))
		if !ok {
			r.next("%s: the vars block has no `SLOT:` var at two spaces to put the Test records vars after; add TEST_BEGIN, TEST_SKIP, COVERAGE and COVERPKG by hand (forge skill load migrations/v1.9.0/tests)", tcTaskfile)
			return lanes
		}
		r.tfNote("the Test records vars (TEST_BEGIN, TEST_SKIP, COVERAGE, COVERPKG)")
		s = next
	}

	lanes.unit = r.taskfileLane(&s, "test", "unit", kind)
	lanes.integration = r.taskfileLane(&s, "test:integration", "integration", kind)
	if service {
		lanes.e2e = r.taskfileLane(&s, "test:e2e", "e2e", kind)
		r.taskfileFrontend(&s, lanes.unit)
	}
	if !lanes.unit && !lanes.integration && !lanes.e2e {
		// No lane records: the vars would be noise in a file the project
		// owns. Leave it as it was; Next says what to do.
		r.tfPending = nil
		return tcLanes{}
	}
	lanes.report = r.taskfileReport(&s, service)
	// `coverage` measured the unit tests; it renders the record only once
	// the unit lane records.
	lanes.coverageFromRecord = lanes.unit && r.taskfileCoverage(&s, kind)

	for i, why := range r.tfPending {
		if i == 0 {
			r.set(tcTaskfile, s, why)
			continue
		}
		r.rep.Rewrites = append(r.rep.Rewrites, tcTaskfile+": "+why)
	}
	r.tfPending = nil
	return lanes
}

// tfNote records one Taskfile edit; taskfile() writes them together, or
// none of them.
func (r *tcRun) tfNote(why string) { r.tfPending = append(r.tfPending, why) }

// tfInsertVars puts the Test records vars right after the SLOT var.
func tfInsertVars(s, vars string) (string, bool) {
	loc := tfVarRe("SLOT").FindStringIndex(s)
	if loc == nil {
		return s, false
	}
	i := loc[0]
	// Skip SLOT's own line and every deeper-indented line under it.
	j := strings.IndexByte(s[i:], '\n')
	if j < 0 {
		return s, false
	}
	i += j + 1
	for i < len(s) {
		k := strings.IndexByte(s[i:], '\n')
		line := s[i:]
		if k >= 0 {
			line = s[i : i+k]
		}
		if strings.TrimSpace(line) == "" || tcIndent(line) <= 2 {
			break
		}
		if k < 0 {
			i = len(s)
			break
		}
		i += k + 1
	}
	return s[:i] + vars + s[i:], true
}

// laneVars is the vars a recorded lane takes.
const laneVars = "    vars:\n      RUN: { sh: \"{{.TEST_BEGIN}}\" }\n      SKIP: { sh: \"{{.TEST_SKIP}}\" }\n"

// taskfileLane moves one lane through capture and reports whether it records
// once the run is done (it did already, or does now).
func (r *tcRun) taskfileLane(s *string, task, lane, kind string) bool {
	t, ok := tfFind(*s, task)
	if !ok {
		return false
	}
	body := (*s)[t.start:t.end]
	if strings.Contains(body, " test capture ") {
		return true
	}
	matches := tfGoTestRe.FindAllStringSubmatchIndex(body, -1)
	if len(matches) != 1 {
		if strings.Contains(body, "go test") {
			r.next("%s: task %s runs a go test command the scaffold never wrote, so it was left as is and records nothing. "+
				"Run it through capture by hand (`{{.FORGE}} test capture --run '{{.RUN}}' --lane %s -- go test -json …`, RUN from `{{.TEST_BEGIN}}`): "+
				"forge skill load migrations/v1.9.0/tests, section \"Taskfile lanes\"", tcTaskfile, task, lane)
		}
		return false
	}
	m := matches[0]
	quote, slot, flags, rest := body[m[2]:m[3]], body[m[4]:m[5]], body[m[6]:m[7]], body[m[8]:m[9]]
	if body[m[10]:m[11]] != quote || !tfLaneArgsPlain(flags+rest) {
		r.next("%s: task %s's go test passes flags capture cannot take as they are (-json, -cover*, -skip, a pipe or a redirect), so it was left as is. "+
			"Record it by hand: forge skill load migrations/v1.9.0/tests, section \"Taskfile lanes\"", tcTaskfile, task)
		return false
	}
	cmdsIdx := strings.Index(body, "\n    cmds:\n")
	if cmdsIdx < 0 || strings.Contains(body, "\n    vars:\n") {
		r.next("%s: task %s declares its own vars (or no cmds: at four spaces), so it was left as is. "+
			"Add RUN: { sh: \"{{.TEST_BEGIN}}\" } and SKIP: { sh: \"{{.TEST_SKIP}}\" } to its vars and run its go test through capture: "+
			"forge skill load migrations/v1.9.0/tests, section \"Taskfile lanes\"", tcTaskfile, task)
		return false
	}
	cmdsIdx++ // the line itself, not the newline before it
	cmdsEnd := cmdsIdx + len("    cmds:\n")

	cmd := tfCapturedCmd(slot, flags, rest, lane, lane != "e2e")
	comment := ""
	switch lane {
	case "unit":
		comment = tcBlock("taskfile_unit_comment_" + kind)
		if !tfLineBeforeIsComment(body, m[0]) {
			comment = strings.TrimPrefix(comment, "      #\n")
		}
	case "integration":
		old := tcBlock("taskfile_integration_comment_old_service")
		if i := strings.Index(body[:m[0]], old); i >= 0 && i+len(old) == m[0] {
			// The service scaffold's comment ends on the line this edit
			// continues: replace it rather than add a second paragraph.
			body = body[:i] + tcBlock("taskfile_integration_comment_new_service") + body[m[0]:]
			delta := len(tcBlock("taskfile_integration_comment_new_service")) - len(old)
			m = shift(m, delta)
		} else {
			comment = tcBlock("taskfile_integration_comment_cli")
		}
	}
	body = body[:m[0]] + comment + cmd + body[m[1]:]
	if lane == "e2e" {
		body = body[:cmdsEnd] + tcBlock("taskfile_e2e_collect_cmds") + body[cmdsEnd:]
		body = body[:cmdsIdx] + tcBlock("taskfile_e2e_vars") + body[cmdsIdx:]
		if d := strings.Index(body, "\n    desc:"); d >= 0 && d < cmdsIdx {
			c := tcBlock("taskfile_e2e_comment")
			if !tfLineBeforeIsComment(body, d+1) {
				c = strings.TrimPrefix(c, "    #\n")
			}
			body = body[:d+1] + c + body[d+1:]
		}
	} else {
		body = body[:cmdsIdx] + laneVars + body[cmdsIdx:]
	}
	next := tfReplace(*s, t, body)
	why := "task " + task + " records lane " + lane + " (forge test capture: results"
	if lane != "e2e" {
		why += ", coverage"
	}
	why += ", quarantined tests skipped)"
	if lane == "e2e" {
		if t2, ok := tfFind(next, "test:e2e"); ok {
			if _, has := tfFind(next, "test:e2e:collect"); !has {
				next = tfInsertAfter(next, t2, tcBlock("taskfile_e2e_collect_task"))
			}
		}
		why = "task test:e2e records lane e2e and collects the env's server coverage (forge test collect, ENV defaults to dev)"
	}
	r.tfNote(why)
	*s = next
	return true
}

func shift(m []int, delta int) []int {
	out := make([]int, len(m))
	for i, v := range m {
		out[i] = v + delta
	}
	return out
}

// tfLineBeforeIsComment reports whether the line before offset i (a line
// start) is a YAML comment.
func tfLineBeforeIsComment(s string, i int) bool { return tcLineBeforeHas(s, i, "#") }

// tcLineBeforeHas reports whether the line before offset i (a line start)
// begins, past its indent, with prefix.
func tcLineBeforeHas(s string, i int, prefix string) bool {
	if i <= 0 {
		return false
	}
	prev := s[:i-1]
	if j := strings.LastIndexByte(prev, '\n'); j >= 0 {
		prev = prev[j+1:]
	}
	return strings.HasPrefix(strings.TrimSpace(prev), prefix)
}

// tfLaneArgsPlain reports whether a lane's go test arguments can move into
// the captured command unchanged: nothing capture adds itself, nothing that
// reshapes the command's output.
func tfLaneArgsPlain(args string) bool {
	for _, tok := range strings.Fields(args) {
		for _, flag := range []string{"-json", "-cover", "-skip", "-test."} {
			if strings.HasPrefix(tok, flag) {
				return false
			}
		}
		if strings.ContainsAny(tok, "|<>&;`") || strings.Contains(tok, "$(") {
			return false
		}
	}
	return true
}

// tfCapturedCmd is the scaffold's captured form of one lane's go test.
func tfCapturedCmd(slot, flags, rest, lane string, coverage bool) string {
	const ind = "        "
	var b strings.Builder
	b.WriteString("      - >-\n")
	b.WriteString(ind + slot + "{{if .RUN}}{{.FORGE}} test capture --run '{{.RUN}}' --lane " + lane + " -- {{end}}go test" + flags + "\n")
	if coverage {
		b.WriteString(ind + `{{- if .RUN}} -json{{if eq .COVERAGE "true"}} -covermode=atomic '-coverprofile={{.RUN}}/` + lane + `/cover.out'{{.COVERPKG}}{{end}}{{end}}` + "\n")
	} else {
		b.WriteString(ind + "{{- if .RUN}} -json{{end}}\n")
	}
	b.WriteString(ind + "{{- if .SKIP}} -skip '{{.SKIP}}'{{end}}" + rest + "\n")
	return b.String()
}

// The frontend lanes: `test` and `test:frontend` hand their run to the
// internal test:frontend:lanes, and the frontends loop prints a LABEL (the
// captured SCRIPT is a shell fragment, not a readable name).
const (
	tfFrontendsOld   = "            echo \"==> {{.SCRIPT}} ($dir, $PM)\"\n            (cd \"$dir\" && {{.SCRIPT}}) || exit 1\n"
	tfFrontendsNew   = "            echo \"==> {{.LABEL | default .SCRIPT}} ($dir, $PM)\"\n            (cd \"$dir\" && { {{.SCRIPT}}; }) || exit 1\n"
	tfTestCallsFE    = "      - task: test:frontend\n"
	tfTestCallsLanes = "      - task: test:frontend:lanes\n        vars: { RUN: \"{{.RUN}}\" }\n"
)

func (r *tcRun) taskfileFrontend(s *string, unitRecorded bool) {
	fe, ok := tfFind(*s, "test:frontend")
	if !ok {
		return
	}
	if _, done := tfFind(*s, "test:frontend:lanes"); done {
		return
	}
	test, _ := tfFind(*s, "test")
	loop, hasLoop := tfFind(*s, "frontends")
	testBody := (*s)[test.start:test.end]
	scaffolded := unitRecorded &&
		tfSameBlock((*s)[fe.start:fe.end], tcBlock("taskfile_frontend_old")) &&
		hasLoop && strings.Count((*s)[loop.start:loop.end], tfFrontendsOld) == 1 &&
		strings.Count(testBody, tfTestCallsFE) == 1
	if !scaffolded {
		r.next("%s: test:frontend (or the frontends loop, or `task test`'s call to it) is not the scaffolded task, so the frontend tests run unrecorded. "+
			"To record them, run each frontend's test through `{{.FORGE}} test capture --lane frontend-<dir>` as a new scaffold's test:frontend:lanes does: "+
			"forge skill load migrations/v1.9.0/tests, section \"Frontend lanes\"", tcTaskfile)
		return
	}
	next := tfReplace(*s, loop, strings.Replace((*s)[loop.start:loop.end], tfFrontendsOld, tfFrontendsNew, 1))
	fe, _ = tfFind(next, "test:frontend")
	gap := next[fe.start:fe.end][len(strings.TrimRight(next[fe.start:fe.end], "\n")):]
	next = tfReplace(next, fe, strings.TrimRight(tcBlock("taskfile_frontend_new"), "\n")+gap)
	test, _ = tfFind(next, "test")
	next = tfReplace(next, test, strings.Replace(next[test.start:test.end], tfTestCallsFE, tfTestCallsLanes, 1))
	r.tfNote("each frontend's tests record as lane frontend-<dir> (Vitest results and v8 coverage), from `task test` and `task test:frontend`")
	*s = next
}

const (
	tfHeaderAll    = "# │   task test:all          Unit + frontend + integration  │\n"
	tfHeaderReport = "# │   task test:report       What those lanes recorded      │\n"
)

// taskfileReport adds test:report where a new scaffold has it: before
// test:all (service) or coverage (cli, library), else at the end.
func (r *tcRun) taskfileReport(s *string, service bool) bool {
	if _, ok := tfFind(*s, "test:report"); ok {
		return true
	}
	before := "coverage"
	if service {
		before = "test:all"
	}
	next, ok := tfInsertBefore(*s, before, tcBlock("taskfile_test_report"))
	if !ok {
		last := tfTasks(*s)
		next = tfInsertAfter(*s, last[len(last)-1], tcBlock("taskfile_test_report"))
	}
	if strings.Count(next, tfHeaderAll) == 1 && !strings.Contains(next, tfHeaderReport) {
		next = strings.Replace(next, tfHeaderAll, tfHeaderAll+tfHeaderReport, 1)
	}
	r.tfNote("task test:report summarises what the lanes recorded (forge test report)")
	*s = next
	return true
}

// taskfileCoverage moves the scaffolded `coverage` task, which ran go test
// itself, onto the record: `forge test report --format html`. It reports
// whether coverage renders the record once the run is done.
func (r *tcRun) taskfileCoverage(s *string, kind string) bool {
	t, ok := tfFind(*s, "coverage")
	if !ok {
		return false
	}
	body := (*s)[t.start:t.end]
	if tfSameBlock(body, tcBlock("taskfile_coverage_new_"+kind)) || strings.Contains(body, "test report --format html") {
		return true
	}
	if !tfSameBlock(body, tcBlock("taskfile_coverage_old_"+kind)) {
		return false
	}
	if _, recorded := tfFind(*s, "test:report"); !recorded {
		return false
	}
	gap := body[len(strings.TrimRight(body, "\n")):]
	next := tfReplace(*s, t, strings.TrimRight(tcBlock("taskfile_coverage_new_"+kind), "\n")+gap)
	r.tfNote("task coverage writes coverage.html from the recorded run (forge test report --format html) instead of running go test again")
	*s = next
	return true
}
