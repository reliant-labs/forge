package opindex

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// FileChange is one file a diff touches, with the line ranges it changed on
// each side. Paths are project-relative and slash-separated.
type FileChange struct {
	// Path is the file on the head side; for a deletion, the deleted file.
	Path string `json:"path"`
	// OldPath is the base-side path of a rename or copy.
	OldPath string `json:"old_path,omitempty"`
	// Status is git's change letter: A, M, D, R, C or T.
	Status string `json:"status"`
	// NewRanges are the head-side lines the diff added or replaced. A range
	// with Count 0 is a pure deletion: lines were removed just after Start.
	NewRanges []LineRange `json:"new_ranges,omitempty"`
	// OldRanges are the base-side lines the diff removed or replaced. A
	// range with Count 0 is a pure insertion, after Start.
	OldRanges []LineRange `json:"old_ranges,omitempty"`
}

// LineRange is Count lines starting at Start (1-based). Count 0 marks a
// point between line Start and Start+1, where the other side had lines.
type LineRange struct {
	Start int `json:"start"`
	Count int `json:"count"`
}

// touches reports whether r changes a declaration spanning lines
// [start, end]. A point (Count 0) touches it only strictly inside: lines
// removed right after a function's closing brace are not in it.
func (r LineRange) touches(start, end int) bool {
	if r.Count == 0 {
		return start <= r.Start && r.Start < end
	}
	return r.Start <= end && r.Start+r.Count-1 >= start
}

// Changes lists what changed between base and head in the git checkout
// around dir, with paths relative to dir (files outside it are left out).
// head "" is the working tree: staged and unstaged edits, plus untracked .go
// and .proto files (as additions) that .gitignore does not exclude.
func Changes(ctx context.Context, dir, base, head string) ([]FileChange, error) {
	args := append(diffArgs("diff"), base)
	if head != "" {
		args = append(args, head)
	}
	patch, err := gitOut(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var pp patchParser
	for _, line := range strings.Split(string(patch), "\n") {
		if err := pp.feed(line); err != nil {
			return nil, err
		}
	}
	changes := pp.finish()
	if head == "" {
		untracked, err := untrackedSources(ctx, dir)
		if err != nil {
			return nil, err
		}
		changes = append(changes, untracked...)
	}
	return changes, nil
}

// diffArgs are the arguments every diff this package reads is produced
// with: a zero-context patch, renames detected, paths relative to the
// working directory, and none of the user's diff configuration (external
// drivers, colour, prefixes) able to change its shape.
func diffArgs(verb string) []string {
	return []string{verb, "--no-ext-diff", "--no-color", "--no-textconv", "-M", "--relative", "-U0", "--src-prefix=a/", "--dst-prefix=b/"}
}

var reHunk = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

// patchParser reads a `git diff -U0` patch, line by line, into FileChanges:
// the status from the extended header (new file, deleted file, rename), the
// paths from the rename lines and the ---/+++ lines (the `diff --git` line
// when a change has neither, as a mode change or binary file does), and the
// ranges from the hunk headers. Hunk bodies are consumed by their header's
// line counts, so a removed line that itself starts with "--" is never read
// as a file header — and the caller can interleave its own marker lines
// between files, as `git log -p` does with commit headers.
type patchParser struct {
	changes        []FileChange
	cur            *FileChange
	remOld, remNew int
}

// inHunk reports whether the next line belongs to a hunk body.
func (p *patchParser) inHunk() bool { return p.remOld > 0 || p.remNew > 0 }

func (p *patchParser) feed(line string) error {
	if p.inHunk() {
		switch {
		case strings.HasPrefix(line, "-"):
			p.remOld--
		case strings.HasPrefix(line, "+"):
			p.remNew--
		}
		return nil // "\ No newline at end of file" and anything else
	}
	if strings.HasPrefix(line, "diff --git ") {
		p.flush()
		oldPath, newPath := splitDiffHeader(line[len("diff --git "):])
		p.cur = &FileChange{Status: "M", OldPath: oldPath, Path: newPath}
		return nil
	}
	if p.cur == nil {
		return nil
	}
	switch {
	case strings.HasPrefix(line, "new file mode"):
		p.cur.Status = "A"
	case strings.HasPrefix(line, "deleted file mode"):
		p.cur.Status = "D"
	case strings.HasPrefix(line, "rename from "):
		p.cur.Status, p.cur.OldPath = "R", unquotePath(line[len("rename from "):])
	case strings.HasPrefix(line, "rename to "):
		p.cur.Path = unquotePath(line[len("rename to "):])
	case strings.HasPrefix(line, "--- "):
		if path := patchPath(line[4:], "a/"); path != "" {
			p.cur.OldPath = path
		}
	case strings.HasPrefix(line, "+++ "):
		if path := patchPath(line[4:], "b/"); path != "" {
			p.cur.Path = path
		}
	case strings.HasPrefix(line, "@@"):
		m := reHunk.FindStringSubmatch(line)
		if m == nil {
			return fmt.Errorf("opindex: malformed hunk header %q", line)
		}
		oStart, oc := atoiDefault(m[1], 0), atoiDefault(m[2], 1)
		nStart, nc := atoiDefault(m[3], 0), atoiDefault(m[4], 1)
		p.cur.OldRanges = append(p.cur.OldRanges, LineRange{Start: oStart, Count: oc})
		p.cur.NewRanges = append(p.cur.NewRanges, LineRange{Start: nStart, Count: nc})
		p.remOld, p.remNew = oc, nc
	}
	return nil
}

// flush completes the current file: a deletion is reported at its old path,
// and only a rename keeps an OldPath.
func (p *patchParser) flush() {
	if p.cur == nil {
		return
	}
	fc := *p.cur
	p.cur = nil
	if fc.Status == "D" {
		fc.Path = fc.OldPath
	}
	if fc.Status != "R" {
		fc.OldPath = ""
	}
	p.changes = append(p.changes, fc)
}

// finish returns every file read, in patch order, and resets the parser.
func (p *patchParser) finish() []FileChange {
	p.flush()
	out := p.changes
	p.changes, p.remOld, p.remNew = nil, 0, 0
	return out
}

// splitDiffHeader reads the two paths of a `diff --git a/X b/Y` line. Each
// may be quoted. Unquoted, a path may contain spaces, so the split is only
// certain when both sides name the same file — the case that matters, since
// a rename's paths come from its own header lines.
func splitDiffHeader(s string) (string, string) {
	if strings.HasPrefix(s, `"`) {
		q, err := strconv.QuotedPrefix(s)
		if err != nil {
			return "", ""
		}
		old := patchPath(q, "a/")
		return old, patchPath(strings.TrimSpace(s[len(q):]), "b/")
	}
	if n := len(s); n%2 == 1 {
		half := n / 2
		if s[half] == ' ' && strings.HasPrefix(s, "a/") && s[half+1:half+3] == "b/" && s[2:half] == s[half+3:] {
			return s[2:half], s[half+3:]
		}
	}
	if i := strings.Index(s, " b/"); i >= 0 {
		return patchPath(s[:i], "a/"), patchPath(s[i+1:], "b/")
	}
	return "", ""
}

func unquotePath(p string) string {
	if strings.HasPrefix(p, `"`) {
		if u, err := strconv.Unquote(p); err == nil {
			return u
		}
	}
	return p
}

// patchPath strips a patch header path's prefix; /dev/null is "". A path
// git had to quote is unquoted.
func patchPath(p, prefix string) string {
	p = unquotePath(strings.TrimRight(p, "\t"))
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(p, prefix)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// untrackedSources lists untracked, unignored .go and .proto files under dir
// as whole-file additions. Other untracked files (scratch notes, build
// output) say nothing about ops and are left out.
func untrackedSources(ctx context.Context, dir string) ([]FileChange, error) {
	out, err := gitOut(ctx, dir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var changes []FileChange
	for _, p := range strings.Split(string(out), "\x00") {
		if !strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, ".proto") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if rerr != nil {
			continue // removed since git listed it
		}
		n := bytes.Count(src, []byte("\n"))
		if len(src) > 0 && src[len(src)-1] != '\n' {
			n++
		}
		fc := FileChange{Path: p, Status: "A"}
		if n > 0 {
			fc.NewRanges = []LineRange{{Start: 1, Count: n}}
		}
		changes = append(changes, fc)
	}
	return changes, nil
}
