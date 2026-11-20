package cli

import (
	"context"
	"fmt"
	"strings"

	"oras.land/oras-go/v2/registry/remote/auth"

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

// sbomReferrerCredential is the credential the attach presents to host. A
// var so a test states it.
var sbomReferrerCredential = registryCredentialFor

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
	host, _, _ := strings.Cut(in.Name, "/")
	credential, err := sbomReferrerCredential(in.ProjectDir, in.Env, in.Entities, host)
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

// registryCredentialFor is the credential forge presents to a registry host
// for an in-process push or read: nil — the ambient docker credential store,
// which `docker push` used for the same image — for every host but the env's
// platform registry, and for that one the env's control-plane identity, the
// one token the platform registry takes (bundle_registry_auth.go). The docker
// store holds the platform host only when an image push logged in to it
// earlier in this process; a site-only or CI-pushed env never did.
//
// The identity is memoized with the bundle push's, per (project, env).
func registryCredentialFor(projectDir, env string, e *KCLEntities, host string) (auth.CredentialFunc, error) {
	if registryTakesNoCredentials(host) {
		return nil, nil
	}
	platform := platformRegistryHost(e)
	if platform == "" || !strings.EqualFold(host, platform) {
		return nil, nil
	}
	key := projectDir + "\x00" + env
	if v, ok := bundleRegistryAuths.Load(key); ok {
		return v.(bundleRegistryAuth).credentialFunc(), nil
	}
	id, err := bundleRegistryAuthOf(env, e)
	if err != nil {
		return nil, err
	}
	bundleRegistryAuths.Store(key, id)
	return id.credentialFunc(), nil
}
