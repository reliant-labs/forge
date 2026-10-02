package cli

// Writing the push-base cache internal/hostedimage defines.
//
// The format, the read side, and WHY the base is cached at all live in
// internal/hostedimage/cache.go — `forge lint` reads the same records and
// cannot import this package, so a second definition of the filename would be
// a cache one side silently never finds.
//
// What lives here is the WRITE, which only this package performs: it happens
// on the EnsureEnvironment forge was already making
// (ensureHostedEnvRecordingPushBase), so the base is learned with no extra
// call and refreshed by every command that could care.

import (
	"fmt"
	"time"

	"github.com/reliant-labs/forge/internal/hostedimage"
	"github.com/reliant-labs/forge/internal/statefile"
)

// rememberHostedPushBase records what the control plane just said.
//
// An EMPTY base writes nothing, and that is the load-bearing case: "the
// server did not tell us" must not be stored as "the server says there is
// none". The two produce different messages — one notes that a host was
// declared at all, the other asserts it is outside the admitted subtree — and
// only one of them would be true.
func rememberHostedPushBase(projectDir, env, base string) error {
	base = hostedimage.NormalizeBase(base)
	if base == "" {
		return nil
	}
	if cachedHostedPushBase(projectDir, env) == base {
		// Unchanged: skip the write so a deploy does not touch a file —
		// and an mtime the render's write scan reads — for no reason.
		return nil
	}
	return statefile.Write(hostedimage.CachePath(projectDir, env), "image push base", hostedimage.CacheRecord{
		Env: env, PushBase: base, RecordedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// cachedHostedPushBase is the last base recorded for this env, or "" when
// none ever was — including when the record is unreadable, because a corrupt
// cache must degrade to "unknown" (which has its own, weaker message) rather
// than fail a render over a file that exists only to sharpen one.
func cachedHostedPushBase(projectDir, env string) string {
	return hostedimage.CachedBase(projectDir, env)
}

// errHostedImageNeedsPushBase is a bare hosted image with no base to resolve
// it under: an older control plane that reports none, or none configured.
//
// The remedy is the pre-ADR-0003 behaviour — declare the full reference —
// because that is the only thing the author can do from here. It names the
// workload rather than the rule, since a project may declare several and only
// one of them is bare.
func errHostedImageNeedsPushBase(env, owner, image string) error {
	return fmt.Errorf("workload %q declares image %q, which names no registry host, and it is bound to forge.OnHosted.\n"+
		"  The control plane for env %q reports no image push base, so there is nothing to resolve it under.\n"+
		"  fix: declare the full reference on the workload (image = \"ghcr.io/<owner>/%s\"), "+
		"or upgrade the control plane to one that reports its registry",
		owner, image, env, image)
}
