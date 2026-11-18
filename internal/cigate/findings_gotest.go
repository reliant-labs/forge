package cigate

import (
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// go test's failures, from its plain output or its -json event stream.
//
// A test log line names only a file's base name (`orders_test.go:9: msg`), so
// it waits in its package's testState until the package is known to have
// failed: in plain output that is the `FAIL <pkg>` summary line that ends the
// package's block; in -json it is the package's own terminal `fail` event,
// and every event carries its Package and Test, so interleaved packages (and
// parallel tests) are attributed by the event, never by what came before it.

var (
	reTestRun     = regexp.MustCompile(`^=== (?:RUN|CONT|NAME|PAUSE)\s+(\S+)`)
	reTestFail    = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	reTestLog     = regexp.MustCompile(`^(\s+)([^\s/:]+\.go):(\d+): ?(.*)$`)
	rePkgFail     = regexp.MustCompile(`^FAIL\s+(\S+)(?:\s|$)`)
	rePkgOK       = regexp.MustCompile(`^ok\s+(\S+)\s`)
	reBuildHeader = regexp.MustCompile(`^# (\S+)`)
)

// testState is one package's go test output read so far.
type testState struct {
	test      string   // the test whose output is being read
	failed    []string // tests that failed, in order
	failedSet map[string]bool
	panics    map[string]string // each test's first panic line
	panic     string            // the package's first panic, whichever test it was in
	pending   []pendingLog
	// last indexes the pending log line continuation lines extend; -1 none.
	last   int
	indent int
	// running is the -json tests started (a run event) and not yet finished
	// (pass, fail or skip), in start order.
	running []string
}

// pendingLog is a test log line waiting for its package to fail, which names
// the directory its bare file name is in.
type pendingLog struct {
	test, file, msg string
	line            int
}

func newTestState() *testState { return &testState{last: -1} }

func (s *testState) start(test string) {
	if !slices.Contains(s.running, test) {
		s.running = append(s.running, test)
	}
}

func (s *testState) finish(test string) {
	s.running = slices.DeleteFunc(s.running, func(t string) bool { return t == test })
}

// owner is the test a -json output line with no Test belongs to: the one test
// still running in the package, or "" — the package itself — when none is,
// or when several are and the line could be any of theirs.
func (s *testState) owner() string {
	if len(s.running) == 1 {
		return s.running[0]
	}
	return ""
}

func (s *testState) setTest(name string) {
	if name != s.test {
		s.test, s.last = name, -1
	}
}

func (s *testState) fail(test string) {
	if s.failedSet == nil {
		s.failedSet = map[string]bool{}
	}
	if !s.failedSet[test] {
		s.failedSet[test] = true
		s.failed = append(s.failed, test)
	}
}

func (s *testState) recordPanic(line string) {
	if s.panics == nil {
		s.panics = map[string]string{}
	}
	if _, ok := s.panics[s.test]; !ok {
		s.panics[s.test] = line
	}
	if s.panic == "" {
		s.panic = line
	}
	s.last = -1
}

// consume reads one line of a package's test output, reporting whether it was
// go test's own (a test header, a test log line or its continuation, a panic).
func (s *testState) consume(line string) bool {
	if m := reTestRun.FindStringSubmatch(line); m != nil {
		s.setTest(m[1])
		return true
	}
	if m := reTestFail.FindStringSubmatch(line); m != nil {
		s.setTest(m[1])
		s.fail(m[1])
		return true
	}
	if strings.HasPrefix(line, "panic: ") {
		s.recordPanic(strings.TrimSpace(line))
		return true
	}
	if s.test == "" {
		return false
	}
	if m := reTestLog.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[3])
		s.pending = append(s.pending, pendingLog{test: s.test, file: m[2], line: n, msg: strings.TrimSpace(m[4])})
		s.last, s.indent = len(s.pending)-1, len(m[1])
		return true
	}
	// A continuation of a multi-line t.Error message: indented deeper than
	// the log line it belongs to.
	if s.last >= 0 && strings.TrimSpace(line) != "" && leadingSpace(line) > s.indent {
		s.pending[s.last].msg += "\n" + strings.TrimSpace(line)
		return true
	}
	if strings.TrimSpace(line) != "" && leadingSpace(line) == 0 {
		s.last = -1
	}
	return false
}

// plainSummary ends the plain-output package block a `FAIL <pkg>` or
// `ok <pkg>` line closes, placing a failed package's findings.
func (p *findingParser) plainSummary(line string) bool {
	if m := rePkgFail.FindStringSubmatch(line); m != nil {
		p.flushPackage(m[1], p.plain, true)
	} else if !rePkgOK.MatchString(line) {
		return false
	}
	p.plain, p.header = newTestState(), ""
	return true
}

// testEvent is the part of a `go test -json` event the parser reads.
type testEvent struct {
	Action      string `json:"Action"`
	Package     string `json:"Package"`
	Test        string `json:"Test"`
	Output      string `json:"Output"`
	ImportPath  string `json:"ImportPath"`  // build-output, build-fail
	FailedBuild string `json:"FailedBuild"` // a package's fail: the package whose build failed
}

// jsonLine reads one `go test -json` event. A line that is not one is plain
// output.
func (p *findingParser) jsonLine(line string) {
	var ev testEvent
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" {
		p.line(line)
		return
	}
	switch ev.Action {
	case "build-output":
		for _, l := range eventLines(ev.Output) {
			p.locatedLine(l, buildPackage(ev.ImportPath))
		}
	case "run":
		if ev.Test != "" {
			p.state(ev.Package).start(ev.Test)
		}
	case "output":
		p.jsonOutput(ev)
	case "fail":
		p.jsonFail(ev)
	case "pass", "skip":
		if ev.Test == "" {
			delete(p.pkgs, ev.Package)
		} else {
			p.state(ev.Package).finish(ev.Test)
		}
	}
}

// jsonOutput reads an output event's lines. A line whose event names no Test
// is the package's own output (a TestMain or init panic, `FAIL`, exit status)
// or a test's that test2json could not attribute: it belongs to the one test
// still running in the package when there is exactly one, and to the package
// otherwise — never to whichever test printed last.
func (p *findingParser) jsonOutput(ev testEvent) {
	st := p.state(ev.Package)
	test := ev.Test
	if test == "" {
		test = st.owner()
	}
	for _, l := range eventLines(ev.Output) {
		st.setTest(test)
		if !st.consume(l) {
			// A build error printed as test output (Go before 1.24).
			p.locatedLine(l, ev.Package)
		}
	}
}

// jsonFail records a failed test, or ends a failed package: its terminal fail
// event is what places its findings, whatever other packages printed between
// its events and whether or not a `FAIL <pkg>` line was printed.
func (p *findingParser) jsonFail(ev testEvent) {
	if ev.Package == "" {
		return
	}
	st := p.state(ev.Package)
	if ev.Test != "" {
		st.fail(ev.Test)
		st.finish(ev.Test)
		return
	}
	if ev.FailedBuild != "" && p.compiled[buildPackage(ev.FailedBuild)] {
		p.compiled[ev.Package] = true
	}
	// A test still running when its package failed was cut short by that
	// failure (a panic, os.Exit, a timeout): it failed too.
	for _, t := range st.running {
		st.fail(t)
	}
	p.flushPackage(ev.Package, st, true)
	delete(p.pkgs, ev.Package)
}

func (p *findingParser) state(pkg string) *testState {
	st, ok := p.pkgs[pkg]
	if !ok {
		st = newTestState()
		p.pkgs[pkg] = st
	}
	return st
}

// flushUnfinished places what a -json stream cut short (a killed run, a
// truncated log) left: the failed tests of packages with no terminal event.
// Without that event nothing says the package itself failed, so no
// package-level finding is added. Plain output's pending block has no package
// line, so it cannot be placed.
func (p *findingParser) flushUnfinished() {
	pkgs := make([]string, 0, len(p.pkgs))
	for pkg := range p.pkgs {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	for _, pkg := range pkgs {
		p.flushPackage(pkg, p.pkgs[pkg], false)
	}
}

// flushPackage places st's failed tests in pkg's directory. A test that failed
// with no log line of its own (nor a subtest's) becomes a finding on the
// package directory, carrying its panic when there was one. A package that
// failed (pkgFailed) with no failed test and no located build error (a
// timeout, a TestMain exit) becomes one too.
func (p *findingParser) flushPackage(pkg string, st *testState, pkgFailed bool) {
	dir, ok := p.pkgDir(pkg)
	if !ok {
		return
	}
	located := map[string]bool{}
	for _, l := range st.pending {
		// Under -v (and -json) a passing test's log is printed too.
		if !st.failedSet[l.test] {
			continue
		}
		located[l.test] = true
		p.add(Finding{Path: path.Join(dir, l.file), Line: l.line, Level: LevelError, Message: l.test + ": " + l.msg})
	}
	for _, t := range st.failed {
		if located[t] || hasLocatedSubtest(located, t) {
			continue
		}
		msg := t + " failed"
		if pm := st.panics[t]; pm != "" {
			msg = t + ": " + pm
		}
		located[t] = true
		p.add(Finding{Path: dir, Level: LevelError, Message: msg})
	}
	switch {
	case pkgFailed && len(st.failed) == 0 && !p.compiled[pkg]:
		msg := pkg + " failed"
		if st.panic != "" {
			msg += ": " + st.panic
		}
		p.add(Finding{Path: dir, Level: LevelError, Message: msg})
	case pkgFailed && st.panics[""] != "":
		// Tests failed, and the package also panicked where no one test can
		// own the panic (several were running): the package carries it.
		p.add(Finding{Path: dir, Level: LevelError, Message: pkg + ": " + st.panics[""]})
	}
}

func hasLocatedSubtest(located map[string]bool, test string) bool {
	for t := range located {
		if strings.HasPrefix(t, test+"/") {
			return true
		}
	}
	return false
}

// pkgDir is the project-relative directory of an import path in the module.
func (p *findingParser) pkgDir(pkg string) (string, bool) {
	mod := p.in.Module
	switch {
	case mod == "":
		return "", false
	case pkg == mod:
		return ".", true
	case strings.HasPrefix(pkg, mod+"/"):
		return strings.TrimPrefix(pkg, mod+"/"), true
	}
	return "", false
}

// buildPackage is the package a build names: `pkg [pkg.test]` is pkg's test
// build.
func buildPackage(importPath string) string {
	if i := strings.IndexByte(importPath, ' '); i >= 0 {
		return importPath[:i]
	}
	return importPath
}

func eventLines(output string) []string {
	return strings.Split(strings.TrimRight(output, "\n"), "\n")
}
