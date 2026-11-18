package cigate

import (
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
)

// ExtractFindings reads a check's output for located problems: generic
// `path:line[:col]: message` lines (go build, go vet, golangci-lint, buf),
// tsc's `path(line,col): error …`, and go test's failures, plain or -json
// (findings_gotest.go). A failed test with no log line is a finding on its
// package's directory, with line 0. The result is sorted and de-duplicated,
// so the same output always gives the same list, and holds at most MaxFindings; omitted counts the rest.
func ExtractFindings(output string, in FindingsInput) (findings []Finding, omitted int) {
	p := findingParser{in: in, seen: map[Finding]bool{}, plain: newPlainState(), buildErr: map[string]bool{}, pkgs: map[string]*testState{}}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "{") {
			p.jsonLine(line)
			continue
		}
		p.line(line)
	}
	p.flushUnfinished()
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

	// plain is the go test state of the package block being read in plain
	// output; a `FAIL <pkg>` or `ok <pkg>` line ends the block.
	plain *testState
	// header is the package a `# <pkg>` build header named, until the next
	// package line. buildErr marks each package whose build output located a
	// line, so its failure is not reported a second time; only a build error
	// does that, never a line a test or the package printed.
	header   string
	buildErr map[string]bool
	// pkgs is the go test -json state of each package, keyed by the events'
	// Package, so interleaved packages never share a test or a failure.
	pkgs map[string]*testState
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

// line reads one line of plain output.
func (p *findingParser) line(line string) {
	if m := reBuildHeader.FindStringSubmatch(line); m != nil {
		p.header = m[1]
		return
	}
	if p.plainSummary(line) || p.plain.consume(line) {
		return
	}
	p.addLocated(line, p.header)
}

// addLocated adds a generic located line at once. buildPkg, when set, is the
// package whose build printed it: that package's build failed.
func (p *findingParser) addLocated(line, buildPkg string) {
	if f, ok := p.located(line); ok {
		p.add(f)
		if buildPkg != "" {
			p.buildErr[buildPkg] = true
		}
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
