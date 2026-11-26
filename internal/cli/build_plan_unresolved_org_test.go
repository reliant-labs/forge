package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// unresolvableOrg makes every env's org lookup fail, as it does on a machine
// with no control-plane credential.
func unresolvableOrg(t *testing.T) {
	t.Helper()
	isolatedCredentials(t)
	prev := resolveControlPlaneOrg
	resolveControlPlaneOrg = func(context.Context, string, *ControlPlaneEntity) (string, error) {
		return "", errors.New("no control-plane credential")
	}
	t.Cleanup(func() { resolveControlPlaneOrg = prev })
}

// TestBuildPlan_ReleasePlanOfBareHostedImagesNeedsNoCredential: a release
// plan of an env whose every hosted image is bare (agent report 28e570f6)
// refused itself with "no workload … declares an image forge builds AND
// pushes": with no credential there was no org, so each bare image stayed bare
// and dropped out of the destinations. The plan now composes them under a
// placeholder org, preflights the rest, and labels the addresses it could not
// validate.
func TestBuildPlan_ReleasePlanOfBareHostedImagesNeedsNoCredential(t *testing.T) {
	planProject(t, hostedPlatformFixture("", "pt"))
	unresolvableOrg(t)

	out, err := runBuildCommand(t, "prod", "--release", "v0.0.0-offline", "--plan", "--no-generate")
	if err != nil {
		t.Fatalf("a credential-free release plan of bare hosted images: want nil, got %v\n%s", err, out)
	}
	for _, want := range []string{
		"registry.reliantapi.com/<org>/pt/pt",
		"UNRESOLVED",
		"not validated",
		"every artifact the env declares would be in the ledger",
		"Nothing was built or pushed",
		"Not validated: 1 hosted address(es)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q:\n%s", want, out)
		}
	}
}

// TestBuildPlan_PushingBuildOfBareHostedImagesStillNeedsTheOrg: the
// placeholder is the plan's alone. A build that pushes still resolves the org
// strictly and refuses before compiling anything.
func TestBuildPlan_PushingBuildOfBareHostedImagesStillNeedsTheOrg(t *testing.T) {
	planProject(t, hostedPlatformFixture("", "pt"))
	unresolvableOrg(t)
	stubDockerLoginArgs(t)

	opts := buildOptions{env: "prod", push: true, release: "v0.0.0", buildDocker: true, outputDir: "bin", buildTarget: "all"}
	_, plan, err := renderBuildInputs(t.Context(), mustLoadProjectConfig(t), &opts)
	if err == nil {
		t.Fatalf("a pushing build with no org: want the credential refusal, got plan %+v", plan)
	}
	if strings.Contains(err.Error(), unresolvedOrgSegment) {
		t.Errorf("the refusal must not mention the plan's placeholder: %v", err)
	}
}

// bareHostedStaticFixture is a hosted env whose workload and site both declare
// bare images, the shape of report 28e570f6.
const bareHostedStaticFixture = `{
  "output": {
    "control_plane": {
      "type": "control_plane",
      "endpoint": "http://127.0.0.1:1",
      "token_env": "FORGE_CONTROL_PLANE_TOKEN"
    },
    "workloads": [
      {
        "name": "api",
        "kind": "service",
        "image": "pt",
        "build": {"type": "go", "cmd": "./cmd/pt", "output_name": "pt"},
        "runtime": {"type": "hosted"},
        "spec": {"kind": "service", "image": "pt"}
      }
    ],
    "frontends": [
      {
        "name": "web",
        "type": "nextjs",
        "path": "frontends/web",
        "public_dir": "out",
        "image": "web",
        "runtime": {"type": "hosted"}
      }
    ]
  }
}`

// TestBuildPlan_BareHostedStaticSiteIsPlannedWithoutCredential: the hosted
// site's release is composed under the same placeholder, so the step, the
// recorded artifact and the coverage gate all name one address.
func TestBuildPlan_BareHostedStaticSiteIsPlannedWithoutCredential(t *testing.T) {
	dir := planProject(t, bareHostedStaticFixture)
	writeWebFrontend(t, dir)
	unresolvableOrg(t)

	out, err := runBuildCommand(t, "prod", "--release", "v0.0.0-offline", "--plan", "--no-generate")
	if err != nil {
		t.Fatalf("a credential-free release plan with a bare hosted site: want nil, got %v\n%s", err, out)
	}
	for _, want := range []string{
		"push registry.reliantapi.com/<org>/pt/web/static.v1",
		"registry.reliantapi.com/<org>/pt/pt",
		"every artifact the env declares would be in the ledger",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output lacks %q:\n%s", want, out)
		}
	}
}
