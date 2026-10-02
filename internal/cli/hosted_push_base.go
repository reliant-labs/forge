package cli

// The org's image push base, learned from the control plane and remembered.
//
// `<registry_base>/<org>` is the ONE registry subtree a control plane admits
// an org's images from (ADR-0003). It is a fact about the PLATFORM, not about
// the project, so it is not declared anywhere in the checkout — the server
// returns it on every environment read, and forge resolves a bare hosted
// image under it (resolveHostedImageBases).
//
// WHY IT IS CACHED, AND WHY THAT IS NOT A SECOND SOURCE OF TRUTH. Two
// consumers need the base where no call is possible or wanted:
//
//   - `forge env render <env>` and `forge lint` must judge a hosted image
//     against it, and neither may touch the network — a render that needed a
//     credential would stop being the offline, reproducible projection every
//     other surface diffs against.
//   - the deploy resolves the same bare image the build pushed, and must
//     reach the same answer without re-deriving it from a different place.
//
// So the base is written down every time the control plane states it, on the
// EnsureEnvironment forge was already making, and read back by name. The
// server is always authoritative: a cached value is only ever used where
// asking is impossible, and every ensure overwrites it.

import (
	"fmt"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/statefile"
)

// hostedPushBaseRecord is one env's last-known push base.
//
// Keyed by ENV and not by project, because the record lives inside one
// project's .forge/state already and an org could in principle move an env
// between registries. Carries when it was learned so a stale value is
// legible to a human reading the file.
type hostedPushBaseRecord struct {
	Env        string `json:"env"`
	PushBase   string `json:"image_push_base"`
	RecordedAt string `json:"recorded_at"`
}

func hostedPushBasePath(projectDir, env string) string {
	return statefile.Path(projectDir, "push-base-"+statefile.SafeSegment(env)+".json")
}

// rememberHostedPushBase records what the control plane just said. An empty
// base writes nothing: "the server did not tell us" must not be stored as
// "the server said there is none", because the two produce different
// messages and only one of them is true.
func rememberHostedPushBase(projectDir, env, base string) error {
	base = normalizePushBase(base)
	if base == "" {
		return nil
	}
	if cachedHostedPushBase(projectDir, env) == base {
		// Unchanged: skip the write so a deploy does not touch a file
		// (and an mtime) for no reason.
		return nil
	}
	return statefile.Write(hostedPushBasePath(projectDir, env), "image push base", hostedPushBaseRecord{
		Env: env, PushBase: base, RecordedAt: time.Now().UTC().Format(time.RFC3339),
	})
}

// cachedHostedPushBase is the last base recorded for this env, or "" when
// none ever was. An unreadable record is "" as well: a corrupt cache must
// degrade to "unknown" (which has its own, weaker message) rather than fail
// a render.
func cachedHostedPushBase(projectDir, env string) string {
	rec, err := statefile.Read[hostedPushBaseRecord](hostedPushBasePath(projectDir, env), "image push base")
	if err != nil || rec == nil {
		return ""
	}
	return normalizePushBase(rec.PushBase)
}

// normalizePushBase trims the one difference that is not a difference: a
// trailing slash. Everything joins with "/" explicitly.
func normalizePushBase(base string) string {
	return strings.TrimSuffix(strings.TrimSpace(base), "/")
}

// errHostedImageNeedsPushBase is a bare hosted image with no base to resolve
// it under: an older control plane that does not report one (or none
// configured at all).
//
// The remedy is the pre-ADR-0003 behaviour — declare the full reference —
// because that is the only thing the author can do from here. It names the
// workload rather than the rule, since a project may have several and only
// one of them is bare.
func errHostedImageNeedsPushBase(env, owner, image string) error {
	return fmt.Errorf("workload %q declares image %q, which names no registry host, and it is bound to forge.OnHosted.\n"+
		"  The control plane for env %q reports no image push base, so there is nothing to resolve it under.\n"+
		"  fix: declare the full reference on the workload (image = \"ghcr.io/<owner>/%s\"), "+
		"or upgrade the control plane to one that reports its registry",
		owner, image, env, image)
}
