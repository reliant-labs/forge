package cli

// A SCOPED DEPLOY RECORDS WHAT IT MOVED (console v3, D5).
//
// `forge env deploy prod v2 --target api` used to append the same promotion an
// unscoped deploy of v2 does, so the ledger said prod runs v2 while every
// workload but api still ran v1 — and release detail, workload history and the
// activity stream all read it that way. The promotion now carries the scope as
// Targets: the --target names as given, or for --frontends-only the env's
// shippable frontends by name. Readers ask release.Promotion.Moved.
//
// A SCOPE IS ONLY RECORDED WHERE IT IS TRUE. Where the env's control plane
// converges promotions — a hub that applies the recorded bundle, or hosted
// tiers whose digests the converger moves — recording a promotion to a NEW
// release moves everything the control plane converges, whatever this machine
// applies. Recording that as "only api moved" would be the same lie in the
// other direction, so it is refused before anything is written, naming the
// two deploys that are honest. Re-applying the release the env already runs is
// still allowed: it moves no binding, so there is nothing to converge.

import (
	"context"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/pkg/release"
)

// renderEntitiesForScope renders the env's declaration to name its frontends.
// A var so a test states the entities.
var renderEntitiesForScope = RenderKCL

// deployScopeTargets is the scope a deploy records on its promotion: nil for an
// unscoped deploy, the --target names (normalized) for a targeted one, and the
// env's shippable frontends for --frontends-only.
func deployScopeTargets(ctx context.Context, projectDir, env string, targets []string, frontendsOnly bool) ([]string, error) {
	if !frontendsOnly {
		return release.NormalizeTargets(targets), nil
	}
	entities, err := renderEntitiesForScope(ctx, projectDir, env)
	if err != nil {
		return nil, fmt.Errorf("--frontends-only: read env %q's declaration to name its frontends: %w", env, err)
	}
	var names []string
	if entities != nil {
		for _, f := range entities.Frontends {
			if f.Runtime.Ships() {
				names = append(names, f.Name)
			}
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("--frontends-only: environment %q declares no shippable frontend to deploy (a forge.FirebaseHosting or forge.StaticSite deploy block)", env)
	}
	return release.NormalizeTargets(names), nil
}

// applyPlanScope puts the deploy's scope on the plan, and corrects the
// direction line for the one case scoping changes it: re-promoting the bound
// release is "no move" only when the current entry already moved everything
// this one would.
func applyPlanScope(plan *promotePlan, scope []string) {
	plan.Scope = scope
	if plan.Direction != promoteDirectionSame || len(plan.Current.Targets) == 0 {
		return
	}
	current := release.Promotion{Release: plan.Current.Release, Targets: plan.Current.Targets}
	requested := release.Promotion{Release: plan.Target.Release, Targets: scope}
	if existing, _ := release.Decide([]release.Promotion{current}, requested); existing != nil {
		return
	}
	moving := "the rest of it"
	if len(scope) > 0 {
		moving = strings.Join(scope, ", ")
	}
	plan.DirectionDetail = fmt.Sprintf("COMPLETES A SCOPED DEPLOY — %s runs %s only for %s; this moves %s",
		plan.Env, plan.Current.Release, strings.Join(plan.Current.Targets, ", "), moving)
}

// refuseConvergedScope refuses a scoped deploy that moves the binding of an env
// whose control plane converges promotions. See the file comment.
func refuseConvergedScope(ledger envLedger, plan promotePlan) error {
	if len(plan.Scope) == 0 || !ledger.Hosted || ledger.ledgerOnly() {
		return nil
	}
	if plan.Current.Bound && plan.Current.Release == plan.Target.Release {
		return nil
	}
	what := "its hosted tiers"
	if ledger.hubUnits() {
		what = "the delivery units its hub converges (" + strings.Join(ledger.Units.Hub, ", ") + ")"
	}
	runs := "nothing yet"
	if plan.Current.Bound {
		runs = plan.Current.Release
	}
	return fmt.Errorf("a scoped deploy cannot move env %q to a new release: its control plane converges every promotion, "+
		"so recording %s would also move %s, and the promotion's scope (%s) would be false.\n\n"+
		"NOTHING WAS RECORDED. Either deploy the whole release:\n"+
		"    forge env deploy %s %s\n"+
		"  or scope a re-apply of the release %s runs (%s)",
		plan.Env, plan.Target.Release, what, strings.Join(plan.Scope, ", "),
		plan.Env, plan.Target.Release, plan.Env, runs)
}
