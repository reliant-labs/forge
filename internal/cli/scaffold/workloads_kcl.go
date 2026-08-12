package scaffold

import (
	"fmt"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
)

// declareWorkloadInKCL appends the freshly-scaffolded workload to the
// project's user-owned deploy/kcl/workloads.k, so what the project
// DEPLOYS stays a declaration the user can read, edit and diff.
//
// It re-reads the component inventory rather than trusting the scaffold verb,
// because discovery is what knows the component's real shape: a worker
// package carrying a Schedule const is a cron, and an operator's
// group/version/CRDs are read from its package. The verb only knows the name
// the user typed.
//
// FAILURE IS ADVISORY, on purpose. The component's code is already written
// and every other step has succeeded; a workloads.k that could not be
// appended to is a one-line paste, not a reason to fail a scaffold. Whatever
// happens, the user is told which of the two it was.
func declareWorkloadInKCL(root string, cfg *config.ProjectConfig, spec componentSpec) {
	if cfg == nil {
		return
	}
	inv := codegen.DiscoverProjectComponents(root, binaryName(cfg, root))
	comp, found := inv.Named(spec.name)
	if !found {
		// Discovery cannot see it yet — a service whose proto has not been
		// compiled into the descriptor, say. Print the stanza so the
		// declaration is never silently skipped.
		fmt.Printf("\n📝 %s\n", codegen.WorkloadStanzaHint(cfg.Name, config.ComponentConfig{
			Name: spec.name,
			Kind: kindFromCtxLabel(spec.ctxLabel),
		}))
		return
	}

	applied, err := codegen.AppendWorkloadStanza(root, cfg.Name, comp)
	switch {
	case err != nil:
		fmt.Printf("\n⚠️  could not update %s: %v\n\n%s\n",
			codegen.WorkloadsKCLRelPath, err, codegen.WorkloadStanzaHint(cfg.Name, comp))
	case applied:
		fmt.Printf("   - %s (%s '%s' declared)\n",
			// Report the WORKLOAD kind that was written, not the component
			// kind it was derived from: the user is being told what is now in
			// the file, and `forge scaffold binary` writes kind="tool".
			codegen.WorkloadsKCLRelPath, codegen.WorkloadKindFor(comp.EffectiveKind()), comp.Name)
	default:
		// Already declared, or the file has been restructured past the point
		// where an append is unambiguous. Either way: show, do not guess.
		fmt.Printf("\n📝 %s\n", codegen.WorkloadStanzaHint(cfg.Name, comp))
	}
}

// kindFromCtxLabel recovers the component kind from the "forge scaffold
// <kind> <name>" boundary label. Used only on the path where discovery has
// not caught up yet, to label a printed stanza. Server is the fallback: it is
// the kind whose expansion is a plain Deployment+Service, which is the least
// surprising thing to suggest.
func kindFromCtxLabel(ctxLabel string) string {
	for _, kind := range []string{
		config.ComponentKindWorker,
		config.ComponentKindCron,
		config.ComponentKindOperator,
		config.ComponentKindBinary,
	} {
		if slices.Contains(strings.Fields(ctxLabel), kind) {
			return kind
		}
	}
	return config.ComponentKindServer
}
