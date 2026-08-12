package cli

import (
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/codegen"
)

// projectDeclaresDevIDP reports whether this project converges a DEV
// identity provider — the gate on whether an env's config.k is seeded with
// the identity block at all (see EnsureIDPIdentityStub /
// EnsureFrontendConfigInstances in generate_middleware.go).
//
// The signal is the `idp-provision` WORKLOAD in deploy/kcl/workloads.k, and
// the choice of signal is the point. It used to be "does docker-compose.yml
// have an `idp` service, and what host port does it publish" — forge parsed
// the compose file, reproduced docker compose's own `${VAR:-default}`
// precedence, and derived the address from the result. That answered the
// question but put forge in the business of interpreting a container
// runtime's config format to learn something the project had already stated
// in its own configuration language, and the derived address then had to be
// injected into a job's environment behind the user's back.
//
// The port now lives in the env's KCL, once, and is handed to both the
// container (`Compose.env`) and the provisioning job (`OneShotJob.env_vars`)
// from that one declaration — so nothing here needs to know an address at
// all. What remains is a yes/no: does this project HAVE a dev IdP to
// converge? The workload that converges it is the honest answer, because a
// project that drops it (an API-key service, a frontend pointed at a real
// issuer) is exactly a project whose config.k should not carry a dev
// identity block.
//
// A missing or unreadable workloads.k reads as "no" — the same answer a
// project scaffolded without a frontend gets, since the workload is only
// written for a project that ships a browser.
func projectDeclaresDevIDP(projectDir string) bool {
	raw, err := os.ReadFile(filepath.Join(projectDir, codegen.WorkloadsKCLRelPath))
	if err != nil {
		return false
	}
	return codegen.WorkloadDeclared(string(raw), codegen.IDPProvisionWorkloadName)
}
