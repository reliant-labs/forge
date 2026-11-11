package cli

import (
	"fmt"
	"io"
	"slices"

	"github.com/reliant-labs/forge/pkg/release"
)

// hostedNativeIdentity resolves a hosted identity's native (mobile)
// applications for the shape: the callbacks KCL cannot know.
//
// KCL derives a kind-native application for every `rn` frontend (the same as
// on a host-run identity) but leaves its callbacks empty, because the callback
// scheme is the frontend's own app.json `expo.scheme`. A host-run identity
// reads it at `forge env up`; a hosted one reads it HERE, at the moment the
// shape is projected for `forge env shape`, `build` and `deploy`, so the
// control plane receives a fully resolved declaration and derives nothing.
// Declared redirect_uris win and are never replaced.
//
// An application whose callbacks cannot be resolved (no usable scheme, or a
// dynamic app.config forge does not evaluate) is NOT sent, with a notice that
// says how to fix it: the control plane refuses a native application with
// empty callbacks, and an app that cannot sign in is not worth failing the
// deploy over (the web sign-in is unaffected).
//
// The identity returned is a copy; the render's own is never modified. An
// identity that is not hosted, or has no native application, comes back
// unchanged.
func hostedNativeIdentity(e *KCLEntities, id *release.ShapeIdentity, projectDir, env string, errOut io.Writer) *release.ShapeIdentity {
	if id == nil || id.Runtime != release.IdentityRuntimeHosted || !slices.ContainsFunc(id.Applications, isNativeApp) {
		return id
	}
	schemes, warnings := nativeAppSchemes(e, id, projectDir)
	for _, w := range warnings {
		fmt.Fprintf(errOut, "[shape] Warning: env %s: %s\n", env, w)
	}
	out := *id
	out.Applications = nil
	for _, a := range id.Applications {
		if !isNativeApp(a) {
			out.Applications = append(out.Applications, a)
			continue
		}
		resolved, ok := resolveHostedNativeApp(a, schemes[a.Frontend])
		if !ok {
			fmt.Fprintf(errOut, "[shape] Warning: env %s: native application %s was NOT sent to the control plane: no callback could be resolved for frontend %q\n", env, a.Name, a.Frontend)
			continue
		}
		out.Applications = append(out.Applications, resolved)
	}
	return &out
}

func isNativeApp(a release.ShapeIdentityApplication) bool {
	return a.Kind == release.IdentityAppNative
}

// resolveHostedNativeApp fills the callbacks of one native application: the
// declared redirect_uris kept as they are, else the ones derived from scheme.
// logout_uris left empty become the `signedout` URI beside each redirect, in
// order. Not ok when there is nothing to register.
func resolveHostedNativeApp(a release.ShapeIdentityApplication, scheme string) (release.ShapeIdentityApplication, bool) {
	a.RedirectURIs = slices.Clone(a.RedirectURIs)
	a.LogoutURIs = slices.Clone(a.LogoutURIs)
	if len(a.RedirectURIs) == 0 {
		a.RedirectURIs = hostedNativeRedirects(scheme)
	}
	if len(a.RedirectURIs) == 0 {
		return a, false
	}
	if len(a.LogoutURIs) == 0 {
		for _, r := range a.RedirectURIs {
			a.LogoutURIs = append(a.LogoutURIs, release.NativeSignedOutBeside(r))
		}
	}
	return a, true
}

// hostedNativeRedirects is the ordered redirect list derived for a hosted
// native application: the private-use scheme callback the app's app.json
// registers with the OS. Empty when the app has no scheme.
//
// The first entry is the one the app signs in with. An https (universal or app
// link) callback is prepended HERE, ahead of the private-use one, once forge
// derives those: resolveHostedNativeApp pairs every entry with its signedout
// URI, and the shape validation accepts https unchanged.
func hostedNativeRedirects(scheme string) []string {
	if scheme == "" {
		return nil
	}
	return []string{release.NativePrivateRedirectURI(scheme)}
}
