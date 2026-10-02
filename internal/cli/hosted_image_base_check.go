package cli

// Two checks over the images a hosted env declares, both pure.
//
// They answer opposite halves of one question — "can the platform pull this?"
// — and they are deliberately separate because the remedies differ:
//
//   - checkHostedImagesResolve: a BARE image with no push base to resolve it
//     under. Nothing can be pushed, so the build is REFUSED.
//   - hostedOffBaseImageFindings: a HOST-BEARING image outside the push base.
//     The bytes can be pushed anywhere, but the control plane admits only its
//     own registry — so this is reported at render and lint time rather than
//     discovered at publish time (which is where it was discovered before).
//
// Both run with no network. The second compares against the base only WHEN
// KNOWN (hosted_push_base.go); when it is unknown it still reports the fact
// that a host was declared at all, because that alone is enough to say
// something useful and nothing about it needs the server.

import (
	"errors"
	"fmt"
	"strings"
)

// checkHostedImagesResolve refuses every hosted item whose image names no
// registry host when there is no push base to resolve it under.
//
// Every offending workload is reported together: a build refused one at a
// time is a build that takes N attempts to learn N things.
func checkHostedImagesResolve(env string, e *KCLEntities, pushBase string) error {
	if e == nil || normalizePushBase(pushBase) != "" {
		return nil
	}
	var errs []error
	for _, w := range e.Workloads {
		if w.Runtime.Type != RuntimeHosted || w.Image == "" || w.Build.Type == "" {
			continue
		}
		if registryHost(imageRepository(w.Image)) == "" {
			errs = append(errs, errHostedImageNeedsPushBase(env, w.Name, w.Image))
		}
	}
	for _, f := range e.Frontends {
		if f.Runtime.Type != RuntimeHosted || f.Image == "" {
			continue
		}
		if registryHost(imageRepository(f.Image)) == "" {
			errs = append(errs, errHostedImageNeedsPushBase(env, f.Name, f.Image))
		}
	}
	return errors.Join(errs...)
}

// hostedOffBaseImageFinding is one host-bearing image on a hosted item. Known
// says whether the push base was available, which decides which of the two
// messages the reader gets.
type hostedOffBaseImageFinding struct {
	// Owner is the workload or frontend that declared it.
	Owner string
	Image string
	// PushBase is the subtree the platform admits, or "" when unknown.
	PushBase string
}

// Known reports whether this finding was judged against a real push base.
func (f hostedOffBaseImageFinding) Known() bool { return f.PushBase != "" }

// Message is the one sentence a reader acts on.
//
// The unknown-base wording claims strictly less, and that restraint is the
// point: render has no network, so an image forge cannot compare must not be
// reported as "outside the registry" — that would be an assertion forge did
// not verify, and on a project whose registry IS the platform's it would be
// false. What is always true, and always worth saying, is that the host does
// not need to be there.
func (f hostedOffBaseImageFinding) Message() string {
	if f.Known() {
		return fmt.Sprintf("%s: image %s is outside this org's image push base %s, and the control plane admits only its own registry. "+
			"Drop the registry host (image = %q) and forge resolves it to %s/%s",
			f.Owner, f.Image, f.PushBase, repositoryName(imageRepository(f.Image)), f.PushBase, repositoryName(imageRepository(f.Image)))
	}
	return fmt.Sprintf("%s: host-bearing image %s on an OnHosted workload: the control plane admits only its own registry; "+
		"drop the host and forge resolves it", f.Owner, f.Image)
}

// hostedOffBaseImageFindings is every host-bearing image on a hosted item of
// this env, judged against pushBase when it is known.
//
// An image already UNDER the base is not a finding: that is the author having
// written the platform's own registry out in full, which is correct, merely
// redundant. Only a host that cannot be admitted — or one forge could not
// check — is reported.
func hostedOffBaseImageFindings(e *KCLEntities, pushBase string) []hostedOffBaseImageFinding {
	if e == nil {
		return nil
	}
	base := normalizePushBase(pushBase)
	var out []hostedOffBaseImageFinding
	consider := func(owner, image string) {
		if image == "" {
			return
		}
		repo := imageRepository(image)
		if registryHost(repo) == "" {
			// Bare: this is the resolved, correct shape, not a finding.
			return
		}
		if base != "" && (repo == base || strings.HasPrefix(repo, base+"/")) {
			return
		}
		out = append(out, hostedOffBaseImageFinding{Owner: owner, Image: image, PushBase: base})
	}
	for _, w := range e.Workloads {
		if w.Runtime.Type == RuntimeHosted {
			consider(w.Name, w.Image)
		}
	}
	for _, f := range e.Frontends {
		if f.Runtime.Type == RuntimeHosted {
			consider(f.Name, f.Image)
		}
	}
	return out
}

// errHostedImagesOffBase is the render-time REFUSAL for a hosted env, built
// from the same findings lint reports.
//
// It is an error on render and a finding on lint for one reason: `forge env
// render <env>` on a hosted env is what a deploy renders, so printing
// manifests the platform will refuse makes the render a document nobody can
// act on. `forge lint` judges a whole project, including envs nobody is
// deploying right now, so the same fact there is something to fix rather than
// something to stop for.
func errHostedImagesOffBase(env string, findings []hostedOffBaseImageFinding) error {
	if len(findings) == 0 {
		return nil
	}
	lines := make([]string, 0, len(findings))
	for _, f := range findings {
		lines = append(lines, "  "+f.Message())
	}
	return fmt.Errorf("env %q: %d hosted image(s) the control plane would refuse to publish:\n%s",
		env, len(findings), strings.Join(lines, "\n"))
}
