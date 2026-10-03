package cli

// Resolving an env's platform push base from its DECLARATION.
//
// There is one rule — `<registry_host>/<organization>/<project>` — and it
// lives in internal/hostedimage.PushBase, which also records why forge
// composes it rather than asking the control plane for it. What lives here is
// the KCLEntities-shaped adapter over it, plus the two errors a caller needs
// when the declaration cannot produce an address.
//
// EVERY CONSUMER READS THIS, AND NOTHING ELSE. The build, the deploy, the
// release-coverage gate, `forge env render` and `forge lint` all resolve a
// hosted address through this one function, so the address a build pushes to
// is the address a deploy pins and the address a render judges. Before this,
// the base arrived from the server and was cached on disk, and "resolve it
// again over here" was a defect waiting to happen — it shipped once already,
// pushing a site to `web/static.v1` with no registry in it at all while the
// release recorded the resolved address.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/reliant-labs/forge/internal/hostedimage"
)

// declaredPushBase is the platform registry subtree this env's hosted
// artifacts go to, composed from its control-plane declaration.
//
// "" when the env declares no control plane, or declares one with no
// organization. Both are legitimate states for this function to be asked
// about — an env with nothing hosted never needs a base — so neither is an
// error here. KCL refuses the one combination that IS a mistake (something
// hosted with no organization) at load, which is earlier and names the field.
func declaredPushBase(e *KCLEntities) string {
	if e == nil || e.ControlPlane == nil {
		return ""
	}
	return hostedimage.PushBase(e.ControlPlane.RegistryHost, e.ControlPlane.Organization, hostedProjectName())
}

// declaredOrganization is the env's declared organization, or "" when it
// declares no control plane or no organization. The placeholder reads as ""
// here for the same reason PushBase composes nothing from it: it is not an
// organization, so a message that named it as one would be misleading at the
// exact moment someone is reading for a cause.
func declaredOrganization(e *KCLEntities) string {
	if e == nil || e.ControlPlane == nil || hostedimage.IsOrgPlaceholder(e.ControlPlane.Organization) {
		return ""
	}
	return e.ControlPlane.Organization
}

// deniedPushHint is the realm-mismatch explanation to append to a failed
// push, or "" when the failure was not a refusal.
//
// It returns a SUFFIX rather than wrapping, so each push path keeps its own
// error shape and the hint is purely additive — a push that failed for any
// other reason reads exactly as it did before. output is the subprocess
// output for a `docker push` and "" for an in-process oras push, which
// carries its status in the error itself.
func deniedPushHint(err error, output, reference, organization string) string {
	if !hostedimage.IsDenied(err, output) {
		return ""
	}
	return "\n\n" + hostedimage.DeniedHint(reference, organization)
}

// dockerPush runs `docker push <reference>`, streaming the daemon's output
// through as it always did, and appends the realm-mismatch hint when the
// registry refused it.
//
// ONE helper for every `docker push` forge makes, which is the only way the
// hint can be reliable: there were four copies of this loop, and a hint added
// to three of them would be a hint that appears or not depending on which
// artifact failed — the least debuggable possible behaviour. organization is
// the env's declared org ("" when it declares none).
//
// The output is TEE'd rather than captured: the author must still see
// docker's own progress and message in real time, and the copy exists only so
// IsDenied can read the distribution error code off it. A subprocess's exit
// status is just "failed", so that text is the only signal available.
func dockerPush(ctx context.Context, reference, organization string) error {
	var captured bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "push", reference)
	cmd.Stdout = io.MultiWriter(os.Stdout, &captured)
	cmd.Stderr = io.MultiWriter(os.Stderr, &captured)
	err := cmd.Run()
	if err == nil {
		return nil
	}
	return fmt.Errorf("docker push %s: %w%s", reference, err,
		deniedPushHint(err, captured.String(), reference, organization))
}

// errHostedImageNeedsPushBase is a bare hosted image in an env that resolves
// no push base: it declares a control plane with no `organization`, so there
// is no subtree to compose the image under.
//
// The remedy is to declare the org, NOT to write a full reference. That
// inverts the pre-ADR-0003 advice on purpose: a hosted author's registry is
// the platform's, so telling them to transcribe a host would be telling them
// to restate a value forge already knows the shape of — which is the defect
// ADR-0003 F1 closed. It names the workload rather than the rule, since a
// project may declare several and only one of them is bare.
func errHostedImageNeedsPushBase(env, owner, image string) error {
	return fmt.Errorf("workload %q declares image %q, which names no registry host, and it is bound to forge.OnHosted.\n"+
		"  Env %q declares no organization, so there is no registry subtree to resolve it under.\n"+
		"  fix: set `organization = \"<your org id>\"` on control_plane in deploy/kcl/%s/main.k; "+
		"forge then resolves the image to %s/<organization>/<project>/%s",
		owner, image, env, env, hostedimage.DefaultRegistryHost, image)
}
