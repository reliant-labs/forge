package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

// checkHostedArtifactPlatforms refuses a release whose pushed manifest cannot
// run on the hosted env's platform's nodes. A hosted image is built on whatever
// machine ran `forge build`; if that machine's arch leaked into the image, the
// pod dies at exec ("exec format error") long after the push succeeded. The
// recorded platforms are the registry's own answer, so this catches an image
// built before the arch was declared as well as one built wrong today.
//
// An artifact with NO recorded platforms is not refused: the capture is
// best-effort and "unknown" is not "wrong".
func checkHostedArtifactPlatforms(e *KCLEntities, artifacts map[string]release.Artifact, env, version string) error {
	if e == nil {
		return nil
	}
	var bad []string
	for _, w := range e.WorkloadsOn(RuntimeHosted) {
		if w.Image == "" {
			continue
		}
		art, ok := artifacts[hostedArtifactKey(e, w)]
		if !ok || len(art.Platforms) == 0 {
			continue
		}
		want := "linux/" + w.Runtime.HostedPlatform()
		found := false
		for _, p := range art.Platforms {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			bad = append(bad, fmt.Sprintf("  %s: expected %s, found %s", w.Name, want, strings.Join(art.Platforms, ", ")))
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("hosted env %q release %s: image platform does not match the control plane's platform's nodes — the pods would exit with \"exec format error\".\n%s\n"+
		"  fix: rebuild with `forge env build %s --release <new-version> --push` (forge builds hosted images for the platform's arch, set by forge.OnHosted.platform; a stale image was built before that), then deploy the new release",
		env, version, strings.Join(bad, "\n"), env)
}
