package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// hostedCapacityPreflight asks the env's control plane whether its hosted part
// fits the org's compute plan, BEFORE anything is built, pushed or recorded.
// It writes nothing. builds is how many images this command would build.
//
//   - refused or undecidable → *deploytarget.CapacityRefusedError;
//   - a control plane that does not serve the check → a one-line warning and
//     nil (the server still enforces at RecordBundle / Promote);
//   - any other failure (network, auth) → that error, as the other hosted RPCs.
//
// An env with nothing hosted is not checked.
func hostedCapacityPreflight(ctx context.Context, envName string, entities *KCLEntities, builds int, out io.Writer) error {
	if entities == nil || !entities.HasHosted() || entities.ControlPlane == nil {
		return nil
	}
	group, err := buildHostedGroup(envName, entities)
	if err != nil || group == nil {
		// The deploy's own plan reports this refusal with its fix.
		return nil
	}
	items, err := deploytarget.PreflightHosted(*group)
	if err != nil {
		return nil
	}
	demand := deploytarget.DemandOf(items, builds)

	ep, err := cloud.ResolveEndpoint(envName, declarationFromEntities(entities))
	if err != nil {
		return err
	}
	cred, err := cloud.ResolveCredential("", ep)
	if err != nil {
		return fmt.Errorf("env %q deploys to the control plane at %s: %w", envName, ep.URL, err)
	}
	client := hostedDeployClient(ep, cred)
	ref := hostedEnvRefFor(envName, entities)
	envID := ""
	if id, lerr := deploytarget.LookupHostedEnvironment(ctx, client, ref.Project, envName); lerr == nil {
		envID = id
	} else if !errors.Is(lerr, deploytarget.ErrHostedEnvironmentNotFound) {
		return lerr
	}
	verdict, err := deploytarget.CheckHostedCapacity(ctx, client, envID, demand)
	if err != nil {
		return err
	}
	switch {
	case verdict.Unchecked:
		fmt.Fprintf(out, "WARNING: capacity could not be pre-checked: %s\n", verdict.Summary())
	case !verdict.Allowed:
		return &deploytarget.CapacityRefusedError{Env: envName, Verdict: verdict, Demand: demand}
	default:
		fmt.Fprintf(out, "Capacity: %s\n", verdict.Summary())
	}
	return nil
}

// hostedBuildCount is how many images a deploy of this env would build: the
// hosted workloads that declare an image this project builds.
func hostedBuildCount(e *KCLEntities) int {
	n := 0
	for _, w := range e.Workloads {
		if w.OnRuntime(RuntimeHosted) && w.Image != "" {
			n++
		}
	}
	return n
}

// hostedCapacityPreflightForEnv renders envName and runs the pre-flight over
// its hosted part. A render failure is not this check's to report: the
// command's own render fails with the real diagnosis.
func hostedCapacityPreflightForEnv(ctx context.Context, projectDir, envName string, withBuilds bool, out io.Writer) error {
	entities, err := RenderKCL(ctx, projectDir, envName)
	if err != nil || entities == nil {
		return nil
	}
	builds := 0
	if withBuilds {
		builds = hostedBuildCount(entities)
	}
	return hostedCapacityPreflight(ctx, envName, entities, builds, out)
}

func entitiesOrEmpty(e *KCLEntities) *KCLEntities {
	if e == nil {
		return &KCLEntities{}
	}
	return e
}
