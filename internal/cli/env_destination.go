package cli

import (
	"context"
	"errors"
	"sort"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

// The console contract for WHERE an environment runs.
//
// `forge env topology --json` and `forge env status --json` both carry these
// fields on their env-level object. They are ADDITIVE: no existing field
// changes meaning, and a consumer that ignores them sees the old document.
//
//   - destination     hosted | cluster | compose | host | external | static | mixed
//     Always set for an env declared in this checkout. Derived from the
//     env's own render, never from machine state.
//   - endpoint        hosted only: the control plane's normalized base URL.
//   - environment_id  hosted only: the control plane's id for the env. EMPTY
//     when the env has never been ensured (never deployed) or the control
//     plane could not be read — never fabricated.
//   - verdict         hosted only: the environment-level deploystate verdict
//     (unknown | converging | converged | diverged | degraded).
//   - workloads[]     hosted only: one deploytarget.HostedWorkloadStatus per
//     deployment — name, tier, url, hostname, verdict, verdict_reason,
//     observed_state, observed_digest, desired_digest, drifted, last_error.

// The destination vocabulary.
const (
	destinationHosted   = "hosted"
	destinationCluster  = "cluster"
	destinationCompose  = "compose"
	destinationHost     = "host"
	destinationExternal = "external"
	destinationStatic   = "static"
	destinationMixed    = "mixed"
)

// envDestination is where one env runs, as the contract above reports it.
type envDestination struct {
	Destination   string
	Endpoint      string
	EnvironmentID string
	Verdict       string
	Workloads     []deploytarget.HostedWorkloadStatus
	// Note explains a hosted read that failed; the other fields stay
	// honest (empty) rather than guessed.
	Note string
}

// destinationOf classifies a rendered env. Pure.
//
// A Bundle declaring control_plane is hosted, whatever else it holds — that
// is the declaration that routes the deploy. Otherwise every deployable thing
// votes for its target kind; one kind is that kind, several are "mixed". An
// env that declares nothing deployable runs nothing anywhere but the local
// machine, which is "host".
func destinationOf(e *KCLEntities) string {
	if e == nil {
		return ""
	}
	if e.ControlPlane != nil {
		return destinationHosted
	}
	kinds := map[string]bool{}
	for _, s := range e.Services {
		switch s.Deploy.Type {
		case "cluster", "simple-backend":
			kinds[destinationCluster] = true
		case "compose":
			kinds[destinationCompose] = true
		case "host", "host-infra":
			kinds[destinationHost] = true
		case "external":
			kinds[destinationExternal] = true
		}
	}
	if len(e.Operators) > 0 || len(e.CronJobs) > 0 || len(e.Databases) > 0 {
		kinds[destinationCluster] = true
	}
	for _, f := range e.Frontends {
		if f.Deploy == nil {
			continue
		}
		switch f.Deploy.Type {
		case "firebase", "static-site":
			kinds[destinationStatic] = true
		case "cluster":
			kinds[destinationCluster] = true
		}
	}
	switch len(kinds) {
	case 0:
		return destinationHost
	case 1:
		for k := range kinds {
			return k
		}
	}
	return destinationMixed
}

// hostedStatusReader reads a hosted env's status. A seam so topology/status
// tests state the control plane's answer.
type hostedStatusReader func(ctx context.Context, envName string, e *KCLEntities) (deploytarget.HostedEnvStatus, error)

// readHostedStatusFromDeclaration builds a client from the env's own
// declaration and credential precedence, then reads its status.
func readHostedStatusFromDeclaration(ctx context.Context, envName string, e *KCLEntities) (deploytarget.HostedEnvStatus, error) {
	ep, err := cloud.ResolveEndpoint(envName, declarationFromEntities(e))
	if err != nil {
		return deploytarget.HostedEnvStatus{}, err
	}
	cred, err := cloud.ResolveCredential("", ep)
	if err != nil {
		return deploytarget.HostedEnvStatus{}, err
	}
	return deploytarget.ReadHostedStatus(ctx, hostedDeployClient(ep, cred), envName)
}

// resolveEnvDestination fills the contract for one rendered env. read may be
// nil, in which case a hosted env reports destination and endpoint only.
func resolveEnvDestination(ctx context.Context, envName string, e *KCLEntities, read hostedStatusReader) envDestination {
	out := envDestination{Destination: destinationOf(e)}
	if out.Destination != destinationHosted {
		return out
	}
	if decl := declarationFromEntities(e); decl != nil {
		if ep, err := cloud.ResolveEndpoint(envName, decl); err == nil {
			out.Endpoint = ep.URL
		}
	}
	if read == nil {
		return out
	}
	st, err := read(ctx, envName, e)
	switch {
	case errors.Is(err, deploytarget.ErrHostedEnvironmentNotFound):
		out.Note = "the control plane has no environment of this name yet — never deployed"
		return out
	case err != nil:
		// The id may still have resolved (a status read that failed after
		// the lookup); report what is KNOWN.
		out.EnvironmentID = st.EnvironmentID
		out.Note = "could not read the control plane's status: " + err.Error()
		return out
	}
	out.EnvironmentID = st.EnvironmentID
	out.Verdict = st.Verdict
	out.Workloads = st.Workloads
	sort.Slice(out.Workloads, func(i, j int) bool { return out.Workloads[i].Name < out.Workloads[j].Name })
	return out
}
