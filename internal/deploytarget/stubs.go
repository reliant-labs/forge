package deploytarget

import (
	"context"
	"fmt"
)

// VMDockerProvider is the stub for the VMDocker target. The KCL
// schema is declarative-only in this release; the SSH dispatch lands
// in a future commit. The provider exists so:
//
//  1. forge.yaml that declares forge.VMDocker renders cleanly and
//     reaches the dispatcher (rather than producing an opaque
//     "no provider" error at grouping time).
//  2. The migration skill can point at a real Go type that explains
//     the deferred state.
type VMDockerProvider struct{}

// Name returns the provider identifier.
func (VMDockerProvider) Name() string { return "vm-docker" }

// Deploy returns a structured "not yet implemented" error pointing
// at the roadmap so users know whether to wait or implement it
// themselves out-of-band.
func (VMDockerProvider) Deploy(_ context.Context, group ServiceGroup) error {
	names := make([]string, 0, len(group.Services))
	host := ""
	for _, s := range group.Services {
		names = append(names, s.Name)
		if s.VMDocker != nil && host == "" {
			host = s.VMDocker.SSHHost
		}
	}
	return fmt.Errorf("%w: forge.VMDocker target for ssh_host=%q (services: %v)\n  See the deploy-target roadmap and file feedback at github.com/reliant-labs/forge/issues if you need this implemented",
		ErrProviderNotImplemented, host, names)
}

// Rollback for vm-docker is a no-op since Deploy never ran. Returns
// the same not-implemented error so callers see a consistent signal.
func (VMDockerProvider) Rollback(_ context.Context, _ ServiceGroup, _ string) error {
	return fmt.Errorf("%w: vm-docker rollback (Deploy never ran)", ErrProviderNotImplemented)
}

// ComposeProvider is the stub for the Compose target. Same deferred-
// state shape as VMDockerProvider.
type ComposeProvider struct{}

// Name returns the provider identifier.
func (ComposeProvider) Name() string { return "compose" }

// Deploy returns a structured "not yet implemented" error.
func (ComposeProvider) Deploy(_ context.Context, group ServiceGroup) error {
	names := make([]string, 0, len(group.Services))
	composeFile := ""
	for _, s := range group.Services {
		names = append(names, s.Name)
		if s.Compose != nil && composeFile == "" {
			composeFile = s.Compose.ComposeFile
		}
	}
	return fmt.Errorf("%w: forge.Compose target for compose_file=%q (services: %v)\n  See the deploy-target roadmap and file feedback at github.com/reliant-labs/forge/issues if you need this implemented",
		ErrProviderNotImplemented, composeFile, names)
}

// Rollback for compose is a no-op since Deploy never ran.
func (ComposeProvider) Rollback(_ context.Context, _ ServiceGroup, _ string) error {
	return fmt.Errorf("%w: compose rollback (Deploy never ran)", ErrProviderNotImplemented)
}
