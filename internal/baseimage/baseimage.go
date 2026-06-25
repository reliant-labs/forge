// Package baseimage owns forge's base-image vendoring + digest-pinning.
//
// The problem it solves: a project's Dockerfiles `FROM` public base images
// (alpine, golang, node, …) on Docker Hub. Building against those directly
// has two failure modes that bite a real team hard:
//
//   - RATE LIMITS. Docker Hub's per-IP unauthenticated pull limit is low;
//     a busy CI / dev IP exhausts it and every build's base pull (and the
//     BuildKit `# syntax=` frontend pull) starts 429ing. `docker login`
//     does NOT reliably fix this — the daemon's pulls stay anonymous.
//   - DRIFT. A `FROM alpine:3.21` is a moving target — the same tag resolves
//     to different bytes over time, so two builds of the "same" source are
//     not reproducible.
//
// The fix, made declarative: declare each base image ONCE in forge.yaml under
// `docker.base_images`, point at a registry MIRROR (a pull-through cache such
// as a GCP Artifact Registry remote repo that proxies Docker Hub on the
// cloud provider's network — dodging the caller's per-IP limit), and let forge
//
//  1. resolve each tag's multi-arch INDEX digest THROUGH the mirror
//     (never via the rate-limited upstream), writing a committed LOCK
//     (.forge/base-images.lock.json: tag → mirror-ref@sha256:<digest>), and
//  2. wire those pinned refs into `forge build` as `--build-arg BASE_<slug>=…`
//     so every image build pulls the pinned, mirrored base.
//
// Dockerfiles consume a base via `ARG BASE_<slug>=<pinned-default>` +
// `FROM ${BASE_<slug>}`: the default keeps a standalone `docker build`
// pinned + transparent, and `forge build` overrides it from the lock (single
// source of truth). Re-pinning is `forge build --repin-bases`.
//
// This package is pure resolution + lock IO + slug/ref derivation. It does
// NOT shell out to docker for the build itself (that's internal/cli) and it
// does NOT know about forge.yaml's struct shape beyond the small Declared
// input it's handed.
package baseimage

import (
	"fmt"
	"sort"
	"strings"
)

// Declared is the resolved forge.yaml `docker.base_images` block: the mirror
// prefix every base is pulled through, plus the list of upstream tags to pin.
// It's the input to a re-pin; the caller (internal/config) projects the YAML
// onto it so this package stays free of the config struct shape.
type Declared struct {
	// MirrorPrefix is the registry/repository the upstream images are
	// proxied through, with NO trailing slash, e.g.
	// "us-docker.pkg.dev/reliant-nonprod-490701/dockerhub". A declared tag's
	// mirrored ref is composed as <MirrorPrefix>/<MirrorPath(tag)>. Empty is
	// a configuration error (Validate rejects it) — without a mirror there is
	// nothing to pin THROUGH and the whole rate-limit dodge is moot.
	MirrorPrefix string
	// Tags is the set of upstream Docker Hub references to pin, in the
	// human-written `name:tag` form (e.g. "alpine:3.21", "golang:1.26-alpine",
	// "docker/dockerfile:1"). Order is irrelevant; the lock is always written
	// sorted for a stable diff.
	Tags []string
}

// Validate checks a Declared block is usable before any resolution runs, so a
// misconfiguration fails with a clear message rather than a confusing docker
// error mid-build. Returns nil when the block is empty (the feature is opt-in:
// a project with no base_images declared simply gets no lock and no build-arg
// injection).
func (d Declared) Validate() error {
	if len(d.Tags) == 0 {
		return nil
	}
	if strings.TrimSpace(d.MirrorPrefix) == "" {
		return fmt.Errorf("docker.base_images: mirror_prefix is required when base_images are declared " +
			"(it is the pull-through registry the bases are resolved + pinned THROUGH, e.g. " +
			"us-docker.pkg.dev/<project>/<remote-repo>)")
	}
	if strings.Contains(d.MirrorPrefix, "://") {
		return fmt.Errorf("docker.base_images.mirror_prefix %q must be a bare registry/repo path, not a URL "+
			"(drop the scheme; e.g. us-docker.pkg.dev/<project>/<remote-repo>)", d.MirrorPrefix)
	}
	seen := map[string]bool{}
	for _, t := range d.Tags {
		if err := validateTag(t); err != nil {
			return err
		}
		if seen[t] {
			return fmt.Errorf("docker.base_images: duplicate tag %q", t)
		}
		seen[t] = true
	}
	// Slugs must be unique too — two distinct tags that collapse to the same
	// build-arg slug (e.g. "golang:1.26-alpine" and "golang:1.26.0-alpine"
	// would NOT collide, but a hand-crafted pair could) would make the
	// build-arg ambiguous. Check it explicitly so the failure names both tags.
	bySlug := map[string]string{}
	for _, t := range d.Tags {
		s := Slug(t)
		if prev, ok := bySlug[s]; ok {
			return fmt.Errorf("docker.base_images: tags %q and %q both map to build-arg slug %q; "+
				"rename one or pin a more specific tag so the slugs differ", prev, t, s)
		}
		bySlug[s] = t
	}
	return nil
}

func validateTag(t string) error {
	if strings.TrimSpace(t) == "" {
		return fmt.Errorf("docker.base_images: empty tag entry")
	}
	if strings.Contains(t, "@") {
		return fmt.Errorf("docker.base_images: tag %q must not be digest-pinned — declare the human tag "+
			"(e.g. alpine:3.21); forge resolves + pins the digest into the lock", t)
	}
	if strings.Contains(t, "://") {
		return fmt.Errorf("docker.base_images: tag %q must be a bare image reference, not a URL", t)
	}
	if !strings.Contains(t, ":") {
		return fmt.Errorf("docker.base_images: tag %q must include an explicit `:tag` (e.g. alpine:3.21) "+
			"so the pin is unambiguous", t)
	}
	return nil
}

// MirrorPath maps an upstream Docker Hub reference to its path UNDER the
// mirror prefix, applying Docker Hub's official-image namespacing convention:
//
//   - A single-segment repo ("alpine:3.21", "golang:1.26-alpine") is an
//     OFFICIAL image, which Docker Hub serves under `library/` — so it maps to
//     `library/<name>:<tag>`. A pull-through mirror reproduces that namespace,
//     so the mirrored path must include `library/`.
//   - A multi-segment repo ("docker/dockerfile:1", "ghcr-style/owner/img:tag")
//     already carries its namespace and is passed through unchanged.
//
// The tag is preserved (NOT the digest); the digest is appended separately by
// MirrorRef once resolved. Example:
//
//	MirrorPath("alpine:3.21")          == "library/alpine:3.21"
//	MirrorPath("docker/dockerfile:1")  == "docker/dockerfile:1"
func MirrorPath(tag string) string {
	repo, ver := splitTag(tag)
	if !strings.Contains(repo, "/") {
		repo = "library/" + repo
	}
	if ver == "" {
		return repo
	}
	return repo + ":" + ver
}

// MirrorTagRef returns the mirrored, tag-form reference forge resolves the
// digest THROUGH: <prefix>/<MirrorPath(tag)>. This is the ref handed to the
// registry index-digest lookup — the resolution always goes through the
// mirror, never the rate-limited upstream.
func MirrorTagRef(prefix, tag string) string {
	return strings.TrimRight(prefix, "/") + "/" + MirrorPath(tag)
}

// MirrorRef returns the fully-pinned, digest-addressed mirror reference for a
// resolved entry: <prefix>/<repo-without-tag>@<digest>. This is the value
// written into the lock and injected as the build-arg — a digest ref drops the
// human tag (a `repo:tag@digest` is legal but redundant; the digest is
// authoritative). Example:
//
//	MirrorRef("us-docker.pkg.dev/p/dockerhub", "alpine:3.21", "sha256:48b0…")
//	  == "us-docker.pkg.dev/p/dockerhub/library/alpine@sha256:48b0…"
func MirrorRef(prefix, tag, digest string) string {
	repo, _ := splitTag(MirrorPath(tag))
	return strings.TrimRight(prefix, "/") + "/" + repo + "@" + digest
}

// Slug derives the build-arg suffix for a tag: BASE_<Slug(tag)>. It is an
// uppercased, identifier-safe rendering of the full `repo:tag` so two tags of
// the same image (golang:1.26-alpine vs golang:1.24-bookworm) get distinct
// build-args, while staying a legal Dockerfile ARG name ([A-Z0-9_]). Every
// non-alphanumeric run collapses to a single underscore; leading digits are
// prefixed with `_` so the result is a valid identifier. Examples:
//
//	Slug("alpine:3.21")           == "ALPINE_3_21"
//	Slug("golang:1.26-alpine")    == "GOLANG_1_26_ALPINE"
//	Slug("docker/dockerfile:1")   == "DOCKER_DOCKERFILE_1"
func Slug(tag string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToUpper(tag) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevUnderscore = false
		default:
			if !prevUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				prevUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "BASE"
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "_" + out
	}
	return out
}

// BuildArgName is the full Dockerfile ARG / `--build-arg` name for a tag:
// BASE_<Slug(tag)>. Dockerfiles declare `ARG BASE_<slug>=<pinned-default>`
// and `FROM ${BASE_<slug>}`; forge build passes `--build-arg <name>=<ref>`.
func BuildArgName(tag string) string {
	return "BASE_" + Slug(tag)
}

// splitTag splits a `repo:tag` (or `repo`) into (repo, tag). A `:` inside a
// registry-port segment is NOT a concern here because base_images tags are
// Docker Hub references (no host:port), and MirrorPath is only ever called on
// such tags — so the LAST colon is the tag separator. A digest (`@…`) is not
// expected (Validate rejects it) but is tolerated by treating the whole
// remainder as the tag.
func splitTag(ref string) (repo, tag string) {
	i := strings.LastIndex(ref, ":")
	// Guard against a `:` that's actually part of a path segment before a `/`
	// (can't happen for Docker Hub refs, but keep it correct): a colon that
	// precedes a slash is not a tag separator.
	if i < 0 || strings.Contains(ref[i:], "/") {
		return ref, ""
	}
	return ref[:i], ref[i+1:]
}

// sortedTags returns d.Tags sorted, for deterministic lock output and stable
// build-arg ordering.
func sortedTags(tags []string) []string {
	out := append([]string(nil), tags...)
	sort.Strings(out)
	return out
}
