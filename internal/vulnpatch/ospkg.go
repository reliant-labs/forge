package vulnpatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/vulnscan"
)

// OS packages (deb, apk, rpm): the image's base ships them, so the patch is
// a newer base in the Dockerfile whose final stage built the image, and the
// rebuild picks up the distro's fixed packages.
//
// The refresh never leaves the tag's line:
//
//   - a tag that is a full version (3.19.1, 1.22.3-alpine3.19, v2.4.0) moves
//     to the newest tag of the same minor, shape and suffix (3.19.4), when
//     the registry has one;
//   - otherwise the reference is pinned to the newest digest of its tag
//     (alpine:3.19 -> alpine:3.19@sha256:…, or a pinned digest moved to the
//     tag's current one).
//
// For an image a new minor is already a new release of the base (alpine 3.19
// -> 3.20, golang 1.22 -> 1.23), so crossing a minor counts as a major here:
// only --allow-major does it, and then within the tag's major. Crossing the
// image's own major (debian 12 -> 13, node 20 -> 22) is never deterministic.

// EcosystemDocker is the base image of a Dockerfile: an OS package's
// patcher, and the Go builder image's.
const EcosystemDocker Ecosystem = "docker"

// resolveOS ties an OS package to the Dockerfile that built the image and
// plans the refresh of that Dockerfile's final base.
func (p *Plan) resolveOS(ctx context.Context, r *Result, f vulnscan.Finding, idx *index, opts Options, at int) {
	r.Ecosystem = EcosystemDocker
	df, base, reason := pickDockerfile(f.Package, idx.dockerfiles)
	if reason != "" {
		r.Reason = reason
		return
	}
	r.Manifests = []string{df.rel}
	ref, err := parseImageRef(base.image)
	if err != nil {
		r.Reason = fmt.Sprintf("%s:%d: %v", df.rel, base.line, err)
		return
	}
	to, reason, err := p.refreshBase(ctx, ref, opts.AllowMajor)
	switch {
	case err != nil:
		r.Reason = fmt.Sprintf("could not read %s's registry: %v", ref, err)
		return
	case reason != "":
		r.Reason = reason
		return
	}
	r.Target = to.String()
	p.planImage(&plannedBump{eco: EcosystemDocker, manifest: df.rel, dir: filepath.Dir(df.abs), pkg: ref.short(), from: ref.String(), to: to.String(), direct: true, line: base.line}, at)
	r.Status, r.Reason = StatusBumped, ""
}

// refreshBase is the newer reference of ref's base by the rules above, or
// the reason there is none.
func (p *Plan) refreshBase(ctx context.Context, ref imageRef, allowMajor bool) (imageRef, string, error) {
	if ref.tag == "" && ref.digest != "" {
		return ref, fmt.Sprintf("%s is pinned by digest alone: no tag says which newer image is the same base", ref), nil
	}
	tag := ref.tag
	if tag == "" {
		tag = "latest"
	}
	if cur, ok := parseImageTag(tag); ok && (cur.parts == 3 || cur.parts == 2 && allowMajor) {
		tags, err := p.tags(ctx, ref.repository())
		if err != nil {
			return ref, "", err
		}
		if next, ok := newestTag(cur, tags, allowMajor); ok {
			out := ref
			out.tag, out.digest = next.raw, ""
			if ref.digest != "" {
				d, err := p.digest(ctx, ref.repository(), next.raw)
				if err != nil {
					return ref, "", err
				}
				out.digest = d
			}
			return out, "", nil
		}
	}
	d, err := p.digest(ctx, ref.repository(), tag)
	if errors.Is(err, ErrImageNotFound) {
		return ref, fmt.Sprintf("%s is not in its registry any more: no newer image of its tag to refresh to", ref), nil
	}
	if err != nil {
		return ref, "", err
	}
	if ref.digest == d {
		why := "no newer " + tagLine(tag) + " tag exists"
		if !allowMajor {
			why += " (--allow-major crosses a minor)"
		}
		return ref, fmt.Sprintf("%s is already the newest image of its tag and %s: the base has not shipped the fix, or the Dockerfile installs the package itself (a rebuild without cache picks up the distro's fix)", ref, why), nil
	}
	out := ref
	out.tag, out.digest = tag, d
	return out, "", nil
}

func tagLine(tag string) string {
	if t, ok := parseImageTag(tag); ok && t.parts >= 2 {
		return t.prefix + strconv.Itoa(t.nums[0]) + "." + strconv.Itoa(t.nums[1]) + ".x"
	}
	return "version"
}

// imageTag is a tag that is a version: [v]N.N[.N][-suffix].
type imageTag struct {
	raw    string
	prefix string // "v" or ""
	nums   [3]int
	parts  int // 1, 2 or 3 numbers
	suffix string
}

var imageTagRE = regexp.MustCompile(`^(v?)(\d+)(?:\.(\d+))?(?:\.(\d+))?(-[A-Za-z0-9._-]+)?$`)

func parseImageTag(tag string) (imageTag, bool) {
	m := imageTagRE.FindStringSubmatch(tag)
	if m == nil {
		return imageTag{}, false
	}
	t := imageTag{raw: tag, prefix: m[1], suffix: m[5]}
	for i, s := range m[2:5] {
		if s == "" {
			break
		}
		t.nums[i], _ = strconv.Atoi(s)
		t.parts = i + 1
	}
	return t, true
}

// newestTag is the newest tag of tags with cur's shape (prefix, number of
// parts, suffix) that is newer than cur: in cur's minor for a full version,
// or — allowMajor — anywhere in cur's major. A two-part tag (3.19) floats
// over its patches already, so only allowMajor moves it (to 3.20).
func newestTag(cur imageTag, tags []string, allowMajor bool) (imageTag, bool) {
	var best imageTag
	found := false
	for _, raw := range tags {
		t, ok := parseImageTag(raw)
		if !ok || t.prefix != cur.prefix || t.parts != cur.parts || t.suffix != cur.suffix || t.nums[0] != cur.nums[0] {
			continue
		}
		if !allowMajor && (cur.parts < 3 || t.nums[1] != cur.nums[1]) {
			continue
		}
		if compareTags(t, cur) <= 0 || (found && compareTags(t, best) <= 0) {
			continue
		}
		best, found = t, true
	}
	return best, found
}

func compareTags(a, b imageTag) int {
	for i := range a.nums {
		if a.nums[i] != b.nums[i] {
			if a.nums[i] < b.nums[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (p *Plan) tags(ctx context.Context, repository string) ([]string, error) {
	if tags, ok := p.tagCache[repository]; ok {
		return tags, nil
	}
	tags, err := p.registry.Tags(ctx, repository)
	if err != nil {
		return nil, err
	}
	p.tagCache[repository] = tags
	return tags, nil
}

func (p *Plan) digest(ctx context.Context, repository, tag string) (string, error) {
	key := repository + ":" + tag
	if d, ok := p.digestCache[key]; ok {
		return d, nil
	}
	d, err := p.registry.Digest(ctx, repository, tag)
	if err != nil {
		return "", err
	}
	p.digestCache[key] = d
	return d, nil
}

// planImage adds a Dockerfile bump, merging it into one of the same FROM
// line: the newer tag wins.
func (p *Plan) planImage(b *plannedBump, result int) {
	for _, have := range p.bumps {
		if have.eco == EcosystemDocker && have.manifest == b.manifest && have.line == b.line {
			if newerRef(b.to, have.to) {
				have.to = b.to
			}
			have.results = appendInt(have.results, result)
			return
		}
	}
	b.results = []int{result}
	p.bumps = append(p.bumps, b)
}

// newerRef reports whether reference a names a newer version tag than b.
func newerRef(a, b string) bool {
	ra, errA := parseImageRef(a)
	rb, errB := parseImageRef(b)
	if errA != nil || errB != nil {
		return false
	}
	ta, okA := parseImageTag(ra.tag)
	tb, okB := parseImageTag(rb.tag)
	return okA && okB && compareTags(ta, tb) > 0
}

// pickDockerfile is the Dockerfile whose final stage built the image an OS
// package was found in, and that stage's base. It picks only when exactly
// one Dockerfile of the tree could have: a final stage FROM scratch has no
// OS packages, and a base that names another distro than the package's
// (alpine for a deb) did not build it. Anything else — none, several, a
// base named by a build argument — is the reason it does not pick.
func pickDockerfile(pkg vulnscan.Package, dfs []*dockerfile) (*dockerfile, *stage, string) {
	if len(dfs) == 0 {
		return nil, nil, fmt.Sprintf("an OS package (%s), and the tree has no Dockerfile whose base could be refreshed", orUnknown(pkg.Type))
	}
	distro := packageDistro(pkg)
	type cand struct {
		df     *dockerfile
		base   *stage
		opaque string
	}
	var possible []cand
	var others []string
	for _, df := range dfs {
		base, opaque := df.finalBase()
		switch {
		case opaque != "":
			possible = append(possible, cand{df: df, base: base, opaque: opaque})
		case base == nil:
			// FROM scratch, or no FROM: no OS packages.
		default:
			names := imageDistros(base.image)
			if distro != "" && len(names) > 0 && !contains(names, distro) {
				others = append(others, fmt.Sprintf("%s (%s)", df.rel, base.image))
				continue
			}
			possible = append(possible, cand{df: df, base: base})
		}
	}
	what := "an OS package"
	if distro != "" {
		what = "a " + distro + " package"
	}
	switch len(possible) {
	case 0:
		if len(others) > 0 {
			return nil, nil, fmt.Sprintf("%s, and no Dockerfile of the tree builds on a %s base (%s)", what, orUnknown(distro), strings.Join(others, ", "))
		}
		return nil, nil, fmt.Sprintf("%s, and every Dockerfile of the tree builds FROM scratch", what)
	case 1:
		if possible[0].opaque != "" {
			return nil, nil, possible[0].opaque
		}
		return possible[0].df, possible[0].base, ""
	}
	var names []string
	for _, c := range possible {
		names = append(names, c.df.rel)
	}
	sort.Strings(names)
	return nil, nil, fmt.Sprintf("%s, and which of %d Dockerfiles built the image is ambiguous (%s)", what, len(names), strings.Join(names, ", "))
}

// packageDistro is the distro an OS package's purl says it is from: its
// distro qualifier (distro=alpine-3.19.1), else its namespace
// (pkg:deb/debian/...). "" when it says neither.
func packageDistro(pkg vulnscan.Package) string {
	purl := pkg.PURL
	if _, q, ok := strings.Cut(purl, "?"); ok {
		for _, kv := range strings.Split(q, "&") {
			if k, v, _ := strings.Cut(kv, "="); k == "distro" && v != "" {
				id, _, _ := strings.Cut(v, "-")
				if d := normalizeDistro(id); d != "" {
					return d
				}
			}
		}
	}
	if rest, ok := strings.CutPrefix(purl, "pkg:"); ok {
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) == 3 {
			return normalizeDistro(parts[1])
		}
	}
	return ""
}

// distroWords are words in an image's name or tag that say which distro it
// is built on, by prefix: alpine3.19, bookworm-slim, static-debian12,
// ubi9-minimal.
var distroWords = []struct{ word, distro string }{
	{"alpine", "alpine"},
	{"wolfi", "wolfi"}, {"chainguard", "wolfi"},
	{"debian", "debian"}, {"buster", "debian"}, {"bullseye", "debian"}, {"bookworm", "debian"}, {"trixie", "debian"}, {"distroless", "debian"},
	{"ubuntu", "ubuntu"}, {"focal", "ubuntu"}, {"jammy", "ubuntu"}, {"noble", "ubuntu"},
	{"ubi", "rhel"}, {"rhel", "rhel"}, {"redhat", "rhel"},
	{"centos", "centos"}, {"fedora", "fedora"}, {"rockylinux", "rocky"}, {"almalinux", "almalinux"},
	{"amazonlinux", "amzn"}, {"oraclelinux", "ol"}, {"opensuse", "opensuse"}, {"sles", "sles"},
}

func normalizeDistro(id string) string {
	id = strings.ToLower(id)
	switch id {
	case "alpine", "wolfi", "debian", "ubuntu", "rhel", "centos", "fedora", "rocky", "almalinux", "amzn", "ol", "opensuse", "sles":
		return id
	case "redhat":
		return "rhel"
	case "chainguard":
		return "wolfi"
	case "opensuse-leap", "opensuse-tumbleweed":
		return "opensuse"
	}
	return ""
}

// imageDistros are the distros an image reference names; none for one that
// names none (node:20, which is Debian, says so nowhere).
func imageDistros(image string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(image), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		for _, d := range distroWords {
			if strings.HasPrefix(w, d.word) {
				out = appendUnique(out, d.distro)
			}
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
