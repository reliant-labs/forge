package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/sbomoci"
	"github.com/reliant-labs/forge/pkg/release"
)

// The SBOM beside the image (design doc §1.3): every OCI artifact a cut
// records — an image, a hosted static site — gets its freshly generated SBOM
// attached in its own registry as an OCI 1.1 referrer (internal/sbomoci),
// and the release records the referrer's address in SBOMRef.Referrer.
//
// It is a post-generate hook, so it runs after the bytes are in the machine
// ledger: a failed attach warns and leaves Referrer empty, the ledger copy
// stays, and the cut goes on. `vuln.attach_sbom: false` in forge.yaml turns
// it off.

func init() {
	sbomPostGenerate = append(sbomPostGenerate, attachSBOMReferrer)
}

// sbomReferrerAttach attaches an SBOM as a referrer. A var so a test points
// it at an in-process registry; nil attaches nothing, the unit tests'
// hermetic default (main_test.go).
var sbomReferrerAttach = sbomoci.Attach

// attachSBOMReferrer is the hook. An artifact it does not attach to (not OCI,
// a variant image, attachment off) keeps its ref unchanged.
func attachSBOMReferrer(ctx context.Context, in sbomGenerated) (release.SBOMRef, error) {
	ref := *in.Artifact.SBOM
	digest, ok := in.Artifact.SharedDigest()
	if !ok || ref.Referrer != "" || sbomReferrerAttach == nil {
		return ref, nil
	}
	if !config.ReadVulnConfig(in.ProjectDir).AttachSBOMEnabled() {
		return ref, nil
	}
	subject := in.Name + "@" + digest
	if !sbomSubjectPushedByProject(in) {
		// A third-party image the project pins (postgres, a vendor image):
		// not ours to write beside, and not worth a warning on every cut.
		ref.Reason = "referrer not attached: " + subject + " is not pushed by this project"
		return ref, nil
	}
	host, _, _ := strings.Cut(in.Name, "/")
	credential, err := sbomRegistryCredential(in.ProjectDir, in.Env, in.Entities, host)
	if err != nil {
		return ref, fmt.Errorf("not attached beside the image (the machine ledger keeps it): %w", err)
	}
	referrer, err := sbomReferrerAttach(ctx, subject, in.Bytes, sbomoci.AttachOptions{
		Registry:  sbomoci.RegistryOptions{Credential: credential},
		Generator: ref.Generator,
		Release:   in.Version,
		Created:   in.CutAt,
	})
	if reason, skipped := sbomoci.SkipReason(err); skipped {
		ref.Reason = reason
		return ref, nil
	}
	if err != nil {
		return ref, fmt.Errorf("not attached beside the image (the machine ledger keeps it; `vuln.attach_sbom: false` in forge.yaml stops trying): %w", err)
	}
	ref.Referrer = referrer
	return ref, nil
}

// sbomSubjectPushedByProject reports whether this project pushed the
// artifact to the repository its release key names, so writing a referrer
// beside it is ours to do:
//
//   - this cut's build state recorded it (built, or carried from an earlier
//     release of the same project, and pushed);
//   - it lives under the env's platform push base, `<registry>/<org>/<project>`
//     — hosted backends (CI-pushed ones and mirrors included) and hosted static
//     sites, the one subtree the platform admits for this project;
//   - it is the repository a workload this env BUILDS resolves to (its declared
//     image, which CI may have pushed): a workload forge does not build names a
//     third-party image, which is never ours.
//
// Everything else — a pinned postgres, a vendor image — is not.
func sbomSubjectPushedByProject(in sbomGenerated) bool {
	if in.Built {
		return true
	}
	e := in.Entities
	if e == nil {
		return false
	}
	if base := platformPushBase(e); base != "" && (in.Name == base || strings.HasPrefix(in.Name, base+"/")) {
		return true
	}
	for _, w := range e.Workloads {
		if w.Image == "" {
			continue
		}
		if imageRepository(w.Spec.Image) == in.Name || (w.Runtime.Type == RuntimeHosted && hostedArtifactKey(e, w) == in.Name) {
			return true
		}
	}
	return false
}
