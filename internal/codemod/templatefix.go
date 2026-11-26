package codemod

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/atomicfile"
	"github.com/reliant-labs/forge/internal/checksums"
)

// templatefix.go — the codemods that carry a fix to a template whose output
// the project OWNS.
//
// A scaffold-once file (an internal package's observe_chain.go, the command
// tree's serve.go) is the project's from birth: forge writes it once and
// never again. A Tier-2 starter (Taskfile.yml, .golangci.yml) is updated by
// `forge project upgrade` only while it is an unedited render; once edited it
// is the project's too. So when forge fixes one of those templates, every
// project scaffolded before the fix keeps the old shape forever, and before
// these codemods nothing told it.
//
// A template fix therefore ships as a codemod here, keyed to the release the
// fix ships in. It reaches existing projects three ways:
//
//   - `forge project upgrade` runs it on the crossing (Upgrade), and `forge
//     generate` holds the pin back until it has (pinHopNeedsUpgrade);
//   - `forge generate` dry-runs it on every run, whatever the pin, and names
//     the files that still have the old shape (Advise), so a project that
//     moved its pin some other way still hears about it;
//   - `forge fix <name>` applies it on request.
//
// Each rewrite matches the old template output exactly and replaces it with
// the new template output. A file whose old shape was edited past
// recognition is not guessed at: the run lists it under Next with the edit to
// make. A disowned file (`forge project disown`) is the project's decision and
// is left alone; a Tier-2 file that is still an unedited render is left to
// upgrade's template phase, which replaces it whole.

// templateFix is one fix to an owned template.
type templateFix struct {
	// name, version, short and long are the Codemod's.
	name, version, short, long string
	// candidates lists the project-relative files that may carry the old
	// shape, in lexical order.
	candidates func(dir string) ([]string, error)
	// tier2 marks a forge-managed starter `forge project upgrade` replaces
	// whole while it is an unedited render (a verifying forge:hash marker).
	tier2 bool
	// rewrite returns src in the new shape and a one-line note of what
	// changed; out nil means src does not need this fix. next, when set,
	// says src has the old shape in a form rewrite cannot place, and the
	// edit to make by hand.
	rewrite func(rel string, src []byte) (out []byte, note, next string)
}

// registerTemplateFix registers f as an Upgrade, Advise, Scoped codemod.
func registerTemplateFix(f templateFix) {
	register(Codemod{
		Name:    f.name,
		Version: f.version,
		Short:   f.short,
		Long:    f.long,
		Upgrade: true,
		Advise:  true,
		Scoped:  true,
		Run: func(dir string, opts Options) (Report, error) {
			return runTemplateFix(dir, opts, f)
		},
	})
}

func runTemplateFix(dir string, opts Options, f templateFix) (Report, error) {
	var rep Report
	files, err := f.candidates(dir)
	if err != nil {
		return rep, err
	}
	cs, err := checksums.Load(dir)
	if err != nil {
		return rep, fmt.Errorf("read forge's ownership state: %w", err)
	}
	for _, rel := range files {
		if !opts.Scope.Contains(rel) || cs.IsDisowned(rel) {
			continue
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		src, err := os.ReadFile(abs)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return rep, err
		}
		if f.tier2 && checksums.Verify(src) == checksums.Pristine {
			continue
		}
		out, note, next := f.rewrite(rel, src)
		if next != "" {
			rep.Next = append(rep.Next, rel+": "+next)
		}
		if out == nil || bytes.Equal(out, src) {
			continue
		}
		rep.Rewrites = append(rep.Rewrites, rel+": "+note)
		if opts.Write {
			info, err := os.Stat(abs)
			if err != nil {
				return rep, err
			}
			if err := atomicfile.WriteFile(abs, out, info.Mode().Perm()); err != nil {
				return rep, err
			}
		}
	}
	return rep, nil
}

// globCandidates returns the project-relative files matching each pattern
// (filepath.Glob syntax, slash-separated, relative to dir), sorted.
func globCandidates(patterns ...string) func(dir string) ([]string, error) {
	return func(dir string) ([]string, error) {
		seen := map[string]bool{}
		var out []string
		for _, p := range patterns {
			matches, err := filepath.Glob(filepath.Join(dir, filepath.FromSlash(p)))
			if err != nil {
				return nil, err
			}
			for _, m := range matches {
				rel, err := filepath.Rel(dir, m)
				if err != nil {
					return nil, err
				}
				rel = filepath.ToSlash(rel)
				if !seen[rel] {
					seen[rel] = true
					out = append(out, rel)
				}
			}
		}
		sort.Strings(out)
		return out, nil
	}
}

// ownedGoFilesNamed returns every owned Go file whose base name is name.
func ownedGoFilesNamed(name string) func(dir string) ([]string, error) {
	return func(dir string) ([]string, error) {
		var out []string
		err := ownedGoFiles(dir, func(rel string, _ []byte) error {
			if filepath.Base(rel) == name {
				out = append(out, rel)
			}
			return nil
		})
		return out, err
	}
}

// replaceOnce replaces the single occurrence of old in src with repl. ok is
// false when old occurs zero times or more than once: the old shape is then
// not the template's, and the caller does not guess.
func replaceOnce(src []byte, old, repl string) (out []byte, ok bool) {
	if strings.Count(string(src), old) != 1 {
		return nil, false
	}
	return []byte(strings.Replace(string(src), old, repl, 1)), true
}
