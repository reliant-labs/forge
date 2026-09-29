package shellbuildtokens

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ScanKCLTree walks dir for `.k` files and reports retired tokens in every
// ShellBuild `cmd` it can read. Findings carry the file and line so `forge
// lint` can point at the declaration.
//
// Scanning SOURCE rather than a render is the right surface here, not a
// compromise. In an ordinary KCL string `${TARGETARCH}` is already a compile
// error ("name 'TARGETARCH' is not defined"), so KCL itself refuses it and
// there is nothing for this rule to add. A token can only survive to the shell
// from a RAW string (r"...") or a backslash-escaped one, where KCL passes it
// through deliberately. Both of those are visible in the text, and a render
// would have to succeed first to show them — which is the wrong ordering for a
// lint that wants to explain the problem rather than report a KCL error.
//
// Directories that hold no hand-written project KCL are skipped: a vendored
// module copy would otherwise report findings the user cannot edit.
//
// A missing dir yields no findings and no error — a CLI or library project has
// no deploy/kcl tree and has nothing to check, which is not a failure.
func ScanKCLTree(dir string) ([]FileFinding, error) {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil, nil
	}
	var out []FileFinding
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".forge", "vendor", ".forge-pkg":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".k" {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			rel = path
		}
		out = append(out, ScanSource(rel, string(raw))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Token < out[j].Token
	})
	return out, nil
}

// FileFinding is a Finding located in a source file.
type FileFinding struct {
	Finding
	File string
	Line int
}

var (
	// shellBuildLiteral matches the opening of a ShellBuild literal, however
	// the module was imported (`forge.ShellBuild {`, `f.ShellBuild {`).
	shellBuildLiteral = regexp.MustCompile(`\bShellBuild\s*\{`)
	// cmdField captures a ShellBuild's cmd string, including the raw-string
	// (r"...") and triple-quoted forms — which are the ones that can carry a
	// token through KCL, so they are the ones that must be read.
	cmdField = regexp.MustCompile(`(?s)\bcmd\s*=\s*(r?"""(.*?)"""|r?"((?:[^"\\]|\\.)*)")`)
	// envKeyField captures the keys a build's env map declares, so a token the
	// user wired up deliberately is not reported.
	envKeyField = regexp.MustCompile(`"([A-Za-z_][A-Za-z0-9_]*)"\s*=`)
	// nameField captures the enclosing workload's name, for the message.
	nameField = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"`)
)

// ScanSource reports retired tokens in every ShellBuild literal in one KCL
// source file. file is used only for the finding's File field.
//
// The workload name is taken from the nearest `name = "..."` field BEFORE the
// ShellBuild literal, which is where it sits in every real declaration. An
// unnamed build reports as "<unnamed>" rather than being skipped: the file and
// line still locate it.
func ScanSource(file, src string) []FileFinding {
	var out []FileFinding
	for _, loc := range shellBuildLiteral.FindAllStringIndex(src, -1) {
		body, ok := kclBlockBody(src, loc[1])
		if !ok {
			continue
		}
		m := cmdField.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		// Group 2 is the triple-quoted body, group 3 the single-quoted one.
		cmd := m[2]
		if cmd == "" {
			cmd = m[3]
		}
		env := map[string]string{}
		if e := envMapBody(body); e != "" {
			for _, k := range envKeyField.FindAllStringSubmatch(e, -1) {
				env[k[1]] = ""
			}
		}
		name := "<unnamed>"
		if n := nameField.FindAllStringSubmatch(src[:loc[0]], -1); len(n) > 0 {
			name = n[len(n)-1][1]
		}
		line := 1 + strings.Count(src[:loc[0]], "\n")
		for _, f := range Check(name, cmd, env) {
			out = append(out, FileFinding{Finding: f, File: file, Line: line})
		}
	}
	return out
}

// envMapBody returns the text of a ShellBuild's `env = { ... }` map, or "".
func envMapBody(body string) string {
	idx := regexp.MustCompile(`\benv\s*=\s*\{`).FindStringIndex(body)
	if idx == nil {
		return ""
	}
	inner, ok := kclBlockBody(body, idx[1])
	if !ok {
		return ""
	}
	return inner
}

// kclBlockBody returns the text between the `{` ending at open and its
// matching `}`, skipping braces inside string literals.
func kclBlockBody(src string, open int) (string, bool) {
	depth := 1
	inString := false
	for i := open; i < len(src); i++ {
		c := src[i]
		switch {
		case inString:
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return src[open:i], true
			}
		}
	}
	return "", false
}
