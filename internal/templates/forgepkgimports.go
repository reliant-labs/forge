package templates

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// forgePkgImportRe matches a `github.com/reliant-labs/forge/pkg/<name>`
// import path inside a template body. The trailing boundary keeps a
// subpackage's parent out of the set: `pkg/orm/x` yields `orm/x`, and a
// bare `pkg/` prefix with nothing after it yields nothing.
var forgePkgImportRe = regexp.MustCompile(`github\.com/reliant-labs/forge/pkg/([a-z][a-z0-9]*(?:/[a-z][a-z0-9]*)*)`)

// ForgePkgImports returns every `forge/pkg/*` subpackage the embedded
// templates import, as module-relative paths ("validate", "svcerr", ...),
// sorted and de-duplicated.
//
// This is the set a scaffolded project actually resolves against the
// PUBLISHED forge/pkg module, so it is the honest subject for any check
// that asks "can a fresh scaffold build without go.work?". Deriving it
// from the templates rather than naming packages by hand is the point:
// an assertion that NAMES one package dies the day the templates stop
// importing it — or, worse, keeps passing while naming a package they
// never imported at all.
func ForgePkgImports() []string {
	seen := map[string]bool{}
	_ = fs.WalkDir(templateFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		body, rerr := templateFS.ReadFile(p)
		if rerr != nil {
			return nil
		}
		for _, m := range forgePkgImportRe.FindAllStringSubmatch(string(body), -1) {
			// Template bodies carry Go import lines and doc prose alike;
			// both are equally good evidence that the scaffold names the
			// package. Trim any trailing punctuation the prose adds.
			name := strings.TrimRight(m[1], ".,;:")
			if name != "" {
				seen[name] = true
			}
		}
		return nil
	})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
