package affected

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// packageGraph is what affected mode needs from `go list -deps -test`.
type packageGraph struct {
	root string
	// dirPkg maps a package directory (absolute) to its import path.
	dirPkg map[string]string
	// embedPkg maps a file a package's non-test code embeds (absolute) to
	// that package: a change to it changes the package for its importers.
	embedPkg map[string]string
	// testEmbedPkg maps a file only a package's tests embed to the package:
	// it reaches that test binary and nothing that merely links the package.
	testEmbedPkg map[string]string
	// hasEmbed marks packages with any //go:embed directive, so a deleted
	// file under one is judged as an embed change.
	hasEmbed map[string]bool
	// ignored maps a Go file the default build excludes (build tags, other
	// OS) to its package.
	ignored map[string]string
	// taggedLanes maps a package to the build-tag lanes (e2e, integration)
	// its tests also run in.
	taggedLanes map[string][]string
	// testDeps maps each module package that has tests to every package its
	// test binary links.
	testDeps map[string][]string
}

// listedPackage is the part of a `go list -json` package affected mode reads;
// encoding/json fills it.
type listedPackage struct {
	ImportPath         string   `json:"ImportPath"`
	Name               string   `json:"Name"`
	Dir                string   `json:"Dir"`
	ForTest            string   `json:"ForTest"`
	Standard           bool     `json:"Standard"`
	Deps               []string `json:"Deps"`
	EmbedFiles         []string `json:"EmbedFiles"`
	TestEmbedFiles     []string `json:"TestEmbedFiles"`
	XTestEmbedFiles    []string `json:"XTestEmbedFiles"`
	EmbedPatterns      []string `json:"EmbedPatterns"`
	TestEmbedPatterns  []string `json:"TestEmbedPatterns"`
	XTestEmbedPatterns []string `json:"XTestEmbedPatterns"`
	IgnoredGoFiles     []string `json:"IgnoredGoFiles"`
}

func readPackageGraph(r io.Reader, root string) (*packageGraph, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	g := &packageGraph{
		root: absRoot, dirPkg: map[string]string{}, embedPkg: map[string]string{},
		testEmbedPkg: map[string]string{}, hasEmbed: map[string]bool{}, ignored: map[string]string{},
		taggedLanes: map[string][]string{}, testDeps: map[string][]string{},
	}
	dec := json.NewDecoder(r)
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if p.Standard || p.Dir == "" {
			continue
		}
		// The test binary's main package: "<pkg>.test", linking everything
		// the package's tests need.
		if p.Name == "main" && strings.HasSuffix(p.ImportPath, ".test") && p.ForTest == "" {
			pkg := strings.TrimSuffix(p.ImportPath, ".test")
			deps := make([]string, 0, len(p.Deps))
			for _, d := range p.Deps {
				deps = append(deps, stripTestVariant(d))
			}
			g.testDeps[pkg] = deps
			continue
		}
		dir := filepath.Clean(p.Dir)
		if !within(absRoot, dir) {
			continue // the module cache, a sibling checkout: not this diff
		}
		path := stripTestVariant(p.ImportPath)
		abs := func(f string) string { return filepath.Join(dir, filepath.FromSlash(f)) }
		if p.ForTest == "" {
			g.dirPkg[dir] = path
			for _, f := range p.EmbedFiles {
				g.embedPkg[abs(f)] = path
			}
			if len(p.EmbedPatterns)+len(p.TestEmbedPatterns)+len(p.XTestEmbedPatterns) > 0 {
				g.hasEmbed[path] = true
			}
			for _, f := range p.IgnoredGoFiles {
				g.ignored[abs(f)] = path
				if strings.HasSuffix(f, "_test.go") {
					for _, l := range fileLanes(abs(f)) {
						g.taggedLanes[path] = addLane(g.taggedLanes[path], l)
					}
				}
			}
		}
		// "p_test [p.test]" is the external test package of p; its embeds,
		// like every test variant's, belong to p's test binary.
		owner := strings.TrimSuffix(path, "_test")
		for _, files := range [][]string{p.TestEmbedFiles, p.XTestEmbedFiles} {
			for _, f := range files {
				g.testEmbedPkg[abs(f)] = owner
			}
		}
		if p.ForTest != "" && strings.HasSuffix(path, "_test") {
			for _, f := range p.EmbedFiles {
				g.testEmbedPkg[abs(f)] = owner
			}
		}
	}
	// Only the module's own packages are candidates to test.
	own := map[string]bool{}
	for _, p := range g.dirPkg {
		own[p] = true
	}
	for pkg := range g.testDeps {
		if !own[pkg] {
			delete(g.testDeps, pkg)
		}
	}
	return g, nil
}

// fileKind says what a changed file is to the package that owns it.
type fileKind int

const (
	// kindCode: the package's code changed, so its test binary and every
	// test binary that links it change.
	kindCode fileKind = iota
	// kindTest: only the package's own tests change.
	kindTest
	// kindTagged: a Go file behind a build tag the unit lane does not set.
	kindTagged
)

// owned is what the import graph says about one changed file.
type owned struct {
	pkg   string
	kind  fileKind
	how   string
	lanes []string // kindTagged: the lanes (e2e, integration) the file belongs to
}

// classify maps a changed project-relative file to the package that owns it.
func (g *packageGraph) classify(rel string) (owned, bool) {
	abs := filepath.Join(g.root, filepath.FromSlash(rel))
	if p, ok := g.embedPkg[abs]; ok {
		return owned{pkg: p, kind: kindCode, how: "embedded by " + p}, true
	}
	if p, ok := g.ignored[abs]; ok {
		lanes := fileLanes(abs)
		if len(lanes) == 0 {
			lanes = nameLanes(rel)
		}
		if len(lanes) > 0 {
			return owned{pkg: p, kind: kindTagged, how: "behind the " + strings.Join(lanes, "/") + " build tag", lanes: lanes}, true
		}
		// Excluded for another OS or architecture: no unit test builds it,
		// but a package's dependents link what it exports on other targets.
		return owned{pkg: p, kind: kindCode, how: "Go source of " + p + " for another OS"}, true
	}
	dir := filepath.Dir(abs)
	if strings.HasSuffix(rel, ".go") && !underTestdata(rel) {
		p, ok := g.dirPkg[dir]
		if !ok {
			return owned{}, false
		}
		if strings.HasSuffix(rel, "_test.go") {
			return owned{pkg: p, kind: kindTest, how: "test file of " + p}, true
		}
		return owned{pkg: p, kind: kindCode, how: "Go source of " + p}, true
	}
	if p, ok := g.testEmbedPkg[abs]; ok {
		return owned{pkg: p, kind: kindTest, how: "embedded by the tests of " + p}, true
	}
	// A package's own testdata/, at any depth below it.
	for d := dir; within(g.root, d); d = filepath.Dir(d) {
		if filepath.Base(d) == "testdata" {
			p, ok := g.dirPkg[filepath.Dir(d)]
			if !ok {
				break
			}
			return owned{pkg: p, kind: kindTest, how: "testdata of " + p}, true
		}
		if d == g.root {
			break
		}
	}
	// A deleted file cannot be in an embed list: judge it by the nearest
	// package above it, when that package embeds files at all.
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		for d := dir; within(g.root, d); d = filepath.Dir(d) {
			if p, ok := g.dirPkg[d]; ok {
				if g.hasEmbed[p] {
					return owned{pkg: p, kind: kindCode, how: "deleted file under " + p + ", which embeds files"}, true
				}
				break
			}
			if d == g.root {
				break
			}
		}
	}
	return owned{}, false
}

// fileLanes returns the build-tag lanes (e2e, integration) a Go file's
// //go:build constraint names.
func fileLanes(abs string) []string {
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "//go:build"); ok {
			var lanes []string
			for _, l := range []string{"e2e", "integration"} {
				if strings.Contains(after, l) {
					lanes = append(lanes, l)
				}
			}
			return lanes
		}
		if strings.HasPrefix(line, "package ") {
			break
		}
	}
	return nil
}

// nameLanes is fileLanes' fallback for a file that cannot be read (deleted):
// this repository names such files after their lane.
func nameLanes(rel string) []string {
	base := filepath.Base(rel)
	var lanes []string
	if strings.Contains(base, "_e2e") {
		lanes = append(lanes, "e2e")
	}
	if strings.Contains(base, "_integration") {
		lanes = append(lanes, "integration")
	}
	return lanes
}

func addLane(lanes []string, l string) []string {
	for _, have := range lanes {
		if have == l {
			return lanes
		}
	}
	return append(lanes, l)
}
