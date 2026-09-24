package cli

import (
	"context"

	"github.com/reliant-labs/forge/internal/deploytarget"
)

// hostedEnvResolver maps a forge environment NAME to the control plane's id
// for the environment of that name, in the caller's organization.
//
// The id is what every per-environment hosted RPC is keyed on (secrets are
// stored per environment id on the control plane), and the NAME is what the
// user types. Keeping this a one-method interface, declared here at its first
// consumer, is deliberate: the hosted deploy target resolves the same thing,
// and whichever lands second shares or replaces this rather than growing a
// second resolver with its own opinion of "which environment".
//
// This file holds the whole default implementation so it can be lifted as
// one unit.
type hostedEnvResolver interface {
	ResolveEnvironmentID(ctx context.Context, envName string) (string, error)
}

// cloudEnvResolver resolves through deploytarget.LookupHostedEnvironment —
// the ONE exact-name lookup, shared with the hosted deploy provider so the two
// cannot disagree about "which environment".
type cloudEnvResolver struct {
	client cloudCaller
}

// cloudEnvironment is the subset of controlplane.v1.DeployEnvironment forge
// reads. Declared locally, like cloudRelease, because forge does not vendor
// the control plane's protos.
type cloudEnvironment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type cloudEnvironmentsResponse struct {
	Environments []cloudEnvironment `json:"environments"`
}

// errHostedEnvNotFound is the resolver's "no environment of that name"
// answer, distinguishable with errors.Is. The release ledger reads it as
// "never promoted" (an env that does not exist yet has run nothing); every
// write path keeps it an error.
var errHostedEnvNotFound = deploytarget.ErrHostedEnvironmentNotFound

func (r cloudEnvResolver) ResolveEnvironmentID(ctx context.Context, envName string) (string, error) {
	return deploytarget.LookupHostedEnvironment(ctx, r.client, envName)
}

// ensureHostedEnv makes the control-plane environment of this NAME exist and
// returns its id. THE rule for hosted writes: every MUTATING hosted command
// (promote, rollback, secret set/unset, deploy) ensures the env first, because
// the env is DECLARED in this project's KCL — whichever of them runs first on
// a fresh env creates it, and none of them can deadlock on another having run.
// Reads (topology, status, secret list, releases) never call this: they
// report an env the control plane has not seen as `environment_id: ""`.
//
// Idempotent server-side (EnsureEnvironment). The hosted deploy provider calls
// the same underlying deploytarget.EnsureHostedEnvironment, so there is one
// request shape for "make it exist".
func ensureHostedEnv(ctx context.Context, client cloudCaller, envName string) (string, error) {
	id, _, err := deploytarget.EnsureHostedEnvironment(ctx, client, envName)
	return id, err
}
