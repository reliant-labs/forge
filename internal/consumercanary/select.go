package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/forgereach"
)

// SelectTests lists the packages of the consumer in dir whose build or tests
// depend on a forge package the diff changed (or, when go.mod/go.sum changed,
// on any forge package), so the consumer's tests run what the change can break
// and nothing else. env is the environment that points the consumer at the
// PR's forge, so the dependency graph it reads is the PR's.
func SelectTests(ctx context.Context, dir string, env []string, reach forgereach.Reach) ([]string, error) {
	changed := map[string]bool{}
	for _, p := range reach.Packages {
		changed[p] = true
	}
	hit := func(dep string) bool {
		return changed[dep] || (reach.All && strings.HasPrefix(dep, reach.Module+"/"))
	}

	own, err := goListEnv(ctx, dir, env, "-f", "{{.ImportPath}}", "./...")
	if err != nil {
		return nil, err
	}
	ownSet := map[string]bool{}
	for _, ip := range strings.Fields(own) {
		ownSet[ip] = true
	}

	// -test adds each package's test variants, whose Deps include imports only
	// its tests make; a package is selected when any variant of it is.
	out, err := goListEnv(ctx, dir, env, "-test", "-json=ImportPath,Deps", "./...")
	if err != nil {
		return nil, err
	}
	picked := map[string]bool{}
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var p struct {
			ImportPath string
			Deps       []string
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list -test: %w", err)
		}
		name := p.ImportPath
		if i := strings.Index(name, " ["); i >= 0 {
			name = name[:i]
		}
		name = strings.TrimSuffix(strings.TrimSuffix(name, ".test"), "_test")
		if !ownSet[name] || picked[name] {
			continue
		}
		if hit(name) {
			picked[name] = true
			continue
		}
		for _, d := range p.Deps {
			if hit(d) {
				picked[name] = true
				break
			}
		}
	}
	res := make([]string, 0, len(picked))
	for ip := range picked {
		res = append(res, ip)
	}
	sort.Strings(res)
	return res, nil
}

func goListEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", append([]string{"list"}, args...)...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go list %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

var (
	durationRE = regexp.MustCompile(`\(?\b\d+(\.\d+)?s\b\)?`)
	cachedRE   = regexp.MustCompile(`\(cached\)`)
)

func normalizeLine(l string) string {
	l = cachedRE.ReplaceAllString(l, "")
	l = durationRE.ReplaceAllString(l, "")
	return strings.Join(strings.Fields(l), " ")
}

// NewFailures returns the lines of out that base does not have: what going
// from the consumer's own forge to the PR's forge added. Timings and cache
// markers are ignored, so a re-run of the same failure matches itself. Empty
// means every failure in out also happens without the PR.
func NewFailures(out, base string) []string {
	have := map[string]bool{}
	for _, l := range strings.Split(base, "\n") {
		if n := normalizeLine(l); n != "" {
			have[n] = true
		}
	}
	var fresh []string
	for _, l := range strings.Split(out, "\n") {
		n := normalizeLine(l)
		if n == "" || have[n] {
			continue
		}
		fresh = append(fresh, strings.TrimRight(l, " \t"))
	}
	return fresh
}
