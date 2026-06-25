package baseimage

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// DigestResolver resolves the multi-arch INDEX digest of a registry reference.
// The default (DockerImagetoolsResolver) shells out to
// `docker buildx imagetools inspect`, which queries the REGISTRY directly and
// reports the top-level manifest digest (the index digest for a multi-arch
// repo, which covers every platform). It is an interface purely so the re-pin
// flow is unit-testable without a docker daemon or network.
type DigestResolver interface {
	// Resolve returns the canonical `sha256:…` index digest of ref. ref is
	// ALWAYS a mirror reference (MirrorTagRef) — resolution goes through the
	// pull-through cache, never the rate-limited upstream.
	Resolve(ctx context.Context, ref string) (string, error)
}

// DockerImagetoolsResolver resolves digests via `docker buildx imagetools
// inspect`. This hits the registry (the mirror), so a multi-arch index digest
// comes back covering both arches — the same digest a `FROM <ref>` would
// resolve on any host. Critically it pulls THROUGH the mirror prefix baked
// into the ref, so the upstream (Docker Hub) is contacted only by the mirror's
// own cloud-side fetch, never by the caller's IP.
type DockerImagetoolsResolver struct{}

func (DockerImagetoolsResolver) Resolve(ctx context.Context, ref string) (string, error) {
	// Use the `{{json .Manifest.Digest}}` template, not the bare
	// `{{.Manifest.Digest}}`: some buildx versions (observed on
	// v0.22-desktop) ignore a bare scalar template and fall back to printing
	// the full human inspect table, which would defeat the parse. The `json`
	// function reliably emits ONLY the quoted digest string. `.Manifest` is
	// the top-level descriptor — for a multi-arch repo that's the INDEX
	// digest, which covers every platform (the value a `FROM <ref>` resolves).
	cmd := exec.CommandContext(ctx, "docker", "buildx", "imagetools", "inspect", ref,
		"--format", "{{json .Manifest.Digest}}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker buildx imagetools inspect %s: %w\n%s", ref, err, strings.TrimSpace(string(out)))
	}
	d := strings.Trim(strings.TrimSpace(string(out)), `"`)
	if !strings.HasPrefix(d, "sha256:") {
		return "", fmt.Errorf("docker buildx imagetools inspect %s: unexpected digest output %q", ref, d)
	}
	return d, nil
}

// Repin resolves every declared tag's index digest THROUGH the mirror and
// returns a fresh Lock. It is pure given a resolver: the caller supplies
// DockerImagetoolsResolver in production and a fake in tests. The declared
// block must already have passed Validate (the caller does this).
//
// Resolution is sequential and fail-fast: a single tag that won't resolve
// (typo, mirror missing the upstream, registry auth) aborts the whole re-pin
// with that tag named, rather than writing a partial lock. The cost is small
// (a handful of bases) and a partial lock is worse than none.
func Repin(ctx context.Context, d Declared, r DigestResolver) (*Lock, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	lk := &Lock{MirrorPrefix: d.MirrorPrefix}
	for _, tag := range sortedTags(d.Tags) {
		mirrorRef := MirrorTagRef(d.MirrorPrefix, tag)
		digest, err := r.Resolve(ctx, mirrorRef)
		if err != nil {
			return nil, fmt.Errorf("repin %q (through mirror %s): %w", tag, mirrorRef, err)
		}
		lk.Entries = append(lk.Entries, LockEntry{
			Tag:    tag,
			Arg:    BuildArgName(tag),
			Ref:    MirrorRef(d.MirrorPrefix, tag, digest),
			Digest: digest,
		})
	}
	return lk, nil
}
