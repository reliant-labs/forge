package cigate

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Finding is one located problem a failed check reported: what a check run
// turns into an annotation.
type Finding struct {
	// Path is slash-separated and relative to the project root. For a test
	// failure the output gave no line for, it is the package's directory.
	Path string `json:"path"`
	// Line is 1-based; 0 means the output named no line.
	Line int `json:"line"`
	// Column is 1-based; absent when the output named none.
	Column int `json:"column,omitempty"`
	// Level is error, warning or notice.
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Finding levels.
const (
	LevelError   = "error"
	LevelWarning = "warning"
)

// MaxFindings caps the findings kept per check; FindingsInput callers report
// how many more there were.
const MaxFindings = 200

// maxFindingMessage caps one message, continuation lines included.
const maxFindingMessage = 2000

// FindingsInput is what ExtractFindings needs to place a check's output in the
// project.
type FindingsInput struct {
	// Root is the project root. When set, a path from a generic
	// `file:line: message` line counts only if that file exists under it,
	// so a URL or a timestamp is never mistaken for a location.
	Root string
	// Dir is the check's directory, project-relative ("" is the root): the
	// directory its tools print paths relative to.
	Dir string
	// Module is the Go module path, which go test's package lines are
	// relative to. Without it a test log line, which names only a file's base
	// name, cannot be placed and is dropped.
	Module string
}

var (
	// path/file.go:12:3: message — go build, go vet, golangci-lint, buf (no
	// space after the last colon), and most compilers and linters.
	reLocated = regexp.MustCompile(`^\s*([^\s:()][^\s:()]*\.[A-Za-z0-9]+):(\d+)(?::(\d+))?:\s*(\S.*)$`)
	// src/x.ts(12,5): error TS2322: message — tsc.
	reTSC = regexp.MustCompile(`^\s*([^\s()]+\.[cm]?[jt]sx?)\((\d+),(\d+)\): (error|warning) (.*)$`)

	reTestRun  = regexp.MustCompile(`^=== (?:RUN|CONT|NAME|PAUSE)\s+(\S+)`)
	reTestFail = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	reTestLog  = regexp.MustCompile(`^(\s+)([^\s/:]+\.go):(\d+): ?(.*)$`)
	rePkgFail  = regexp.MustCompile(`^FAIL\s+(\S+)(?:\s|$)`)
	rePkgOK    = regexp.MustCompile(`^ok\s+(\S+)\s`)
)

// ExtractFindings reads a check's output for located problems: generic
// `path:line[:col]: message` lines (go build, go vet, golangci-lint, buf),
// tsc's `path(line,col): error …`, and go test's failures, plain or -json,
// whose log lines name a file in the package a later `FAIL <pkg>` line names.
// A failed test with no log line is a finding on its package's directory, with
// line 0. The result is sorted and de-duplicated, so the same output always
// gives the same list, and holds at most MaxFindings; omitted counts the rest.
func ExtractFindings(output string, in FindingsInput) (findings []Finding, omitted int) {
	p := findingParser{in: in, seen: map[Finding]bool{}, last: -1}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "{") {
			p.jsonLine(line)
			continue
		}
		p.line(line)
	}
	p.flushPackage("")
	sort.Slice(p.out, func(i, j int) bool {
		a, b := p.out[i], p.out[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Message < b.Message
	})
	if len(p.out) > MaxFindings {
		return p.out[:MaxFindings], len(p.out) - MaxFindings
	}
	return p.out, 0
}

type findingParser struct {
	in   FindingsInput
	out  []Finding
	seen map[Finding]bool

	// go test state, for the package whose output is being read.
	test    string   // the test whose output is being read
	failed  []string // tests that failed, in order
	panics  map[string]string
	panic   string // the package's first panic, whichever test it was in
	pending []pendingLog
	// last indexes the pending log line continuation lines extend; -1 none.
	last     int
	indent   int
	compiled bool // a located compiler line since the last package line
}

// pendingLog is a test log line waiting for its package line, which names the
// directory its bare file name is in.
type pendingLog struct {
	test, file, msg string
	line            int
}

func (p *findingParser) add(f Finding) {
	if len(f.Message) > maxFindingMessage {
		f.Message = f.Message[:maxFindingMessage] + "…"
	}
	if p.seen[f] {
		return
	}
	p.seen[f] = true
	p.out = append(p.out, f)
}

func (p *findingParser) line(line string) {
	if p.goTestLine(line) {
		return
	}
	if f, ok := p.located(line); ok {
		p.add(f)
		p.compiled = true
	}
}

// located parses a generic `path:line[:col]: message` or tsc line.
func (p *findingParser) located(line string) (Finding, bool) {
	var file, msg, level string
	var ln, col int
	if m := reTSC.FindStringSubmatch(line); m != nil {
		file, msg, level = m[1], m[5], m[4]
		ln, _ = strconv.Atoi(m[2])
		col, _ = strconv.Atoi(m[3])
	} else if m := reLocated.FindStringSubmatch(line); m != nil {
		file, msg, level = m[1], m[4], LevelError
		ln, _ = strconv.Atoi(m[2])
		col, _ = strconv.Atoi(m[3])
		if strings.HasPrefix(strings.ToLower(msg), "warning:") {
			level, msg = LevelWarning, strings.TrimSpace(msg[len("warning:"):])
		}
	} else {
		return Finding{}, false
	}
	rel, ok := p.relPath(file)
	if !ok || ln < 1 {
		return Finding{}, false
	}
	return Finding{Path: rel, Line: ln, Column: col, Level: level, Message: strings.TrimSpace(msg)}, true
}

// relPath places a path a tool printed under the project root.
func (p *findingParser) relPath(file string) (string, bool) {
	file = filepath.ToSlash(file)
	var rel string
	switch {
	case path.IsAbs(file) || filepath.IsAbs(file):
		if p.in.Root == "" {
			return "", false
		}
		r, err := filepath.Rel(p.in.Root, filepath.FromSlash(file))
		if err != nil || strings.HasPrefix(filepath.ToSlash(r), "../") {
			return "", false
		}
		rel = filepath.ToSlash(r)
	default:
		rel = path.Clean(path.Join(filepath.ToSlash(p.in.Dir), file))
	}
	if strings.HasPrefix(rel, "../") || rel == ".." {
		return "", false
	}
	if p.in.Root != "" {
		if fi, err := os.Stat(filepath.Join(p.in.Root, filepath.FromSlash(rel))); err != nil || fi.IsDir() {
			return "", false
		}
	}
	return rel, true
}

// goTestLine consumes one line of go test output, reporting whether it was
// go test's own (a test header, a test log line, a package line).
func (p *findingParser) goTestLine(line string) bool {
	if m := reTestRun.FindStringSubmatch(line); m != nil {
		p.test, p.last = m[1], -1
		return true
	}
	switch m := reTestFail.FindStringSubmatch(line); {
	case m != nil:
		p.test, p.last = m[1], -1
		p.failed = append(p.failed, m[1])
		return true
	case rePkgOK.MatchString(line):
		p.reset()
		return true
	case strings.HasPrefix(line, "panic: "):
		p.recordPanic(strings.TrimSpace(line))
		return true
	}
	if m := rePkgFail.FindStringSubmatch(line); m != nil {
		p.flushPackage(m[1])
		return true
	}
	if p.test == "" {
		return false
	}
	if m := reTestLog.FindStringSubmatch(line); m != nil {
		n, _ := strconv.Atoi(m[3])
		p.pending = append(p.pending, pendingLog{test: p.test, file: m[2], line: n, msg: strings.TrimSpace(m[4])})
		p.last, p.indent = len(p.pending)-1, len(m[1])
		return true
	}
	// A continuation of a multi-line t.Error message: indented deeper than
	// the log line it belongs to.
	if p.last >= 0 && strings.TrimSpace(line) != "" && leadingSpace(line) > p.indent {
		p.pending[p.last].msg += "\n" + strings.TrimSpace(line)
		return true
	}
	if strings.TrimSpace(line) != "" && leadingSpace(line) == 0 {
		p.last = -1
	}
	return false
}

// flushPackage places the pending test findings in pkg's directory. A test
// that failed with no log line of its own (nor a subtest's) becomes a finding
// on the package directory, carrying its panic when there was one, and a
// package that failed with nothing else located (a timeout, a TestMain exit)
// becomes one too. pkg "" is the end of the output: whatever is pending has
// no package line, so it cannot be placed.
func (p *findingParser) flushPackage(pkg string) {
	defer p.reset()
	if pkg == "" {
		return
	}
	dir, ok := p.pkgDir(pkg)
	if !ok {
		return
	}
	failed := map[string]bool{}
	for _, t := range p.failed {
		failed[t] = true
	}
	located := map[string]bool{}
	for _, l := range p.pending {
		// Under -v (and -json) a passing test's log is printed too.
		if !failed[l.test] {
			continue
		}
		located[l.test] = true
		p.add(Finding{Path: path.Join(dir, l.file), Line: l.line, Level: LevelError, Message: l.test + ": " + l.msg})
	}
	for _, t := range p.failed {
		if located[t] || hasLocatedSubtest(located, t) {
			continue
		}
		msg := t + " failed"
		if pm := p.panics[t]; pm != "" {
			msg = t + ": " + pm
		}
		located[t] = true
		p.add(Finding{Path: dir, Level: LevelError, Message: msg})
	}
	if len(p.failed) == 0 && !p.compiled {
		msg := pkg + " failed"
		if p.panic != "" {
			msg += ": " + p.panic
		}
		p.add(Finding{Path: dir, Level: LevelError, Message: msg})
	}
}

// recordPanic keeps the first panic line of the test being read, and of the
// package (a timeout panics outside any one test's failure).
func (p *findingParser) recordPanic(line string) {
	if p.panics == nil {
		p.panics = map[string]string{}
	}
	if _, ok := p.panics[p.test]; !ok {
		p.panics[p.test] = line
	}
	if p.panic == "" {
		p.panic = line
	}
	p.last = -1
}

func hasLocatedSubtest(located map[string]bool, test string) bool {
	for t := range located {
		if strings.HasPrefix(t, test+"/") {
			return true
		}
	}
	return false
}

func (p *findingParser) reset() {
	p.test, p.failed, p.pending, p.last, p.panics, p.panic, p.compiled = "", nil, nil, -1, nil, "", false
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

// jsonLine reads one `go test -json` event: its output is go test's plain
// output, line by line, and its Test names the test it belongs to even when
// parallel tests interleave.
func (p *findingParser) jsonLine(line string) {
	var ev struct {
		Action string `json:"Action"`
		Test   string `json:"Test"`
		Output string `json:"Output"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil || ev.Action == "" {
		p.line(line)
		return
	}
	if ev.Action != "output" {
		return
	}
	for _, l := range strings.Split(strings.TrimRight(ev.Output, "\n"), "\n") {
		if ev.Test != "" && ev.Test != p.test {
			p.test, p.last = ev.Test, -1
		}
		p.line(l)
	}
}

func leadingSpace(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}
