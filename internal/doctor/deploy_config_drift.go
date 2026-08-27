package doctor

// deploy_config_drift.go — the KCL → forge.yaml direction of the
// config/reality cross-check.
//
// internal/cli/generate_config_check.go already walks the other way:
// for each frontend forge.yaml DECLARES, does its path exist on disk.
// Its header names the principle — a silent skip was "the #1 source of
// 'I added a service but generate did nothing' friction" — but it can
// only cross-check what forge.yaml still declares, so a frontend that
// exists in the deploy graph and NOT in the project config falls in the
// blind spot that file was written to close.
//
// That gap has a concrete failure. `forge build --target <name>`
// resolves its target set from cfg.Frontends (resolveBuildTargetSet in
// internal/cli/build.go), so a frontend declared only in KCL answers:
//
//	Error: target "reliant-web" not found in project config
//
// which names the symptom and not the cause. Meanwhile the env's KCL
// says `deploy = forge.FirebaseHosting { site = "…" }` — the strongest
// statement of intent the project can make about a component. Claiming
// to ship something that cannot be built is the contradiction worth
// reporting.
//
// Severity splits on that claim:
//
//   - a `deploy` block is an ERROR. It says "ship this to production",
//     and the build that would produce the artifact cannot resolve it.
//   - `deploy = None` is a WARNING. A build-only frontend is
//     legitimately compile-checked and makes no shipping claim, so it is
//     a likely oversight rather than a contradiction.
//
// The opposite direction is deliberately NOT reported: a frontend in
// forge.yaml that no environment's KCL declares is the ordinary shape of
// a dev-only frontend (and of one whose deploy is still being written).
// Nothing about it is broken — `forge build --target` resolves it, and
// no environment claims to ship it.
//
// Frontends are the whole check because they are the only component kind
// forge.yaml still declares. Services carry no `services:` key at all —
// they are discovered from the proto descriptor and internal/ tree, so
// there is no second list for a KCL declaration to disagree with, and a
// service in KCL that names no code is already reported by lint's
// component-drift rule against deploy/kcl/workloads.k.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// frontendDrift is one frontend the deploy graph declares and forge.yaml
// does not, accumulated across every environment that declares it.
type frontendDrift struct {
	name string
	// envs are the environments declaring it, sorted, so the finding can
	// name deploy/kcl/{preprod,prod}/main.k rather than repeat itself
	// once per environment.
	envs []string
	// deployEnvs are the subset whose declaration carries a deploy block.
	// Non-empty promotes the finding to an error.
	deployEnvs []string
	// deployTypes are the distinct deploy discriminators seen
	// ("firebase", "cluster"), sorted — named in the evidence so the
	// reader knows what the project believes it is shipping.
	deployTypes []string
}

// shipsSomewhere reports whether any environment's declaration carries a
// deploy target, which is what separates the error case from the warning.
func (d frontendDrift) shipsSomewhere() bool { return len(d.deployEnvs) > 0 }

// CheckFrontendConfigDrift reports frontends the rendered deploy graph
// declares that forge.yaml does not, so `forge build --target <name>`
// cannot resolve them.
//
// SKIPs when the project declares no environments (--kind cli /
// library), and when it has no readable forge.yaml — with no project
// config there is nothing for the KCL to disagree with, and a forge.yaml
// broken enough not to parse has already failed the command that loaded
// it first.
//
// Both of those Skips are DOWNGRADED to UNDETERMINED when any
// environment failed to render (see [renderScope.fold]). That is the
// right answer even for the forge.yaml arm: with a hole in the deploy
// graph AND no config to compare it against, "the question does not
// apply" is a stronger claim than the facts support.
func CheckFrontendConfigDrift(_ context.Context, env *Environment) CheckResult {
	return examineRendered(env, "frontend declarations", func(renders []envRender) CheckResult {
		cfg, err := config.LoadProjectDir(env.ProjectDir)
		if err != nil {
			return CheckResult{
				Status:   StatusSkip,
				Message:  "no readable forge.yaml — nothing to cross-check the deploy graph against",
				Evidence: err.Error(),
			}
		}

		declared := make(map[string]bool, len(cfg.Frontends))
		for _, fe := range cfg.Frontends {
			declared[fe.Name] = true
		}

		drift := collectFrontendDrift(renders, declared)
		if len(drift) == 0 {
			return CheckResult{
				Status: StatusPass,
				Message: fmt.Sprintf("%d env(s): every KCL-declared frontend is in forge.yaml",
					len(renders)),
			}
		}

		var problems []string
		worst := StatusWarn
		for _, d := range drift {
			problems = append(problems, frontendDriftMessage(d))
			if d.shipsSomewhere() {
				worst = StatusFail
			}
		}

		shipping := 0
		for _, d := range drift {
			if d.shipsSomewhere() {
				shipping++
			}
		}
		summary := fmt.Sprintf("%d frontend(s) declared in KCL but absent from forge.yaml", len(drift))
		if shipping > 0 {
			summary = fmt.Sprintf("%s — %d with a deploy target, so they claim to ship and cannot be built",
				summary, shipping)
		}
		return CheckResult{Status: worst, Message: summary, Evidence: strings.Join(problems, "\n")}
	})
}

// collectFrontendDrift folds every environment's rendered frontends into
// one finding per NAME. A frontend is reported once however many
// environments declare it — the fix is a single forge.yaml entry, so a
// per-environment finding would be the same instruction repeated.
//
// Only environments that RENDERED are folded in — which is now a
// property of the input rather than a filter here: examineRendered hands
// the check body nothing else. That matters because "this env does not
// declare the frontend" and "this env did not evaluate" are
// indistinguishable in a failed render, and the difference is the whole
// question. The unread environments are named by the caller's scope
// instead, where they downgrade the verdict.
func collectFrontendDrift(renders []envRender, declared map[string]bool) []frontendDrift {
	byName := map[string]*frontendDrift{}
	var order []string

	for _, r := range renders {
		for _, fe := range r.frontends {
			if fe.Name == "" || declared[fe.Name] {
				continue
			}
			d, seen := byName[fe.Name]
			if !seen {
				d = &frontendDrift{name: fe.Name}
				byName[fe.Name] = d
				order = append(order, fe.Name)
			}
			d.envs = append(d.envs, r.env)
			if fe.Deploy != nil {
				d.deployEnvs = append(d.deployEnvs, r.env)
				if t := fe.Deploy.Type; t != "" && !contains(d.deployTypes, t) {
					d.deployTypes = append(d.deployTypes, t)
				}
			}
		}
	}

	sort.Strings(order)
	out := make([]frontendDrift, 0, len(order))
	for _, name := range order {
		d := byName[name]
		sort.Strings(d.envs)
		sort.Strings(d.deployEnvs)
		sort.Strings(d.deployTypes)
		out = append(out, *d)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// frontendDriftMessage renders one finding in the batched,
// named-and-remediated style the sibling config check uses: name the
// entity, name the files that declare it, name what breaks, and give the
// two ways out.
func frontendDriftMessage(d frontendDrift) string {
	if !d.shipsSomewhere() {
		return fmt.Sprintf(
			"frontends[name=%s] declared in %s with `deploy = None` but absent from forge.yaml — "+
				"`forge build --target %s` cannot resolve it, so it is never built. "+
				"Add it to forge.yaml frontends[], or remove the KCL declaration.",
			d.name, kclPathsPhrase(d.envs), d.name)
	}
	target := "a deploy target"
	if len(d.deployTypes) > 0 {
		target = fmt.Sprintf("a %s deploy target", strings.Join(d.deployTypes, "/"))
	}
	return fmt.Sprintf(
		"frontends[name=%s] declared in %s with %s but absent from forge.yaml — "+
			"`forge build --target %s` cannot resolve it. "+
			"Add it to forge.yaml frontends[], or remove the KCL declaration.",
		d.name, kclPathsPhrase(d.deployEnvs), target, d.name)
}

// kclPathsPhrase renders the environments as a brace-expanded path so a
// frontend declared in four environments reads as one location rather
// than four.
func kclPathsPhrase(envs []string) string {
	if len(envs) == 1 {
		return fmt.Sprintf("deploy/kcl/%s/main.k", envs[0])
	}
	return fmt.Sprintf("deploy/kcl/{%s}/main.k", strings.Join(envs, ","))
}
