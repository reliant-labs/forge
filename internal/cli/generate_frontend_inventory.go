package cli

// generate_frontend_inventory.go — recovering the codegen inventory for a
// project that declares its frontends in KCL instead of forge.yaml.
//
// forge.yaml's `frontends:` key is what every frontend emitter iterates:
// the Connect hooks, config_gen.ts, the mock transport, the CRUD pages,
// nav. It is a CODEGEN inventory, and when it is absent those emitters
// have nothing to walk.
//
// That used to be a safe assumption, because a frontend had nowhere else
// to be declared. It no longer is: a project can declare its frontend
// topology in deploy/kcl/<env>/main.k, and control-plane did exactly
// that — deleting the block on the (correct) grounds that the topology
// had moved. What went with it was not topology but the codegen
// inventory, and the symptoms were entirely silent:
//
//   - `forge generate` emitted nothing for frontends/internal-console and
//     reported its ~30 existing generated files as STALE, offering to
//     delete them;
//   - config_gen.ts was never refreshed, so the app kept declaring an
//     env var whose proto field had been removed — a compile-clean
//     frontend reading a value that no longer exists.
//
// The recovery has to be narrower than "use the KCL frontends", because
// the deploy graph legitimately contains frontends this project must NOT
// generate into. See config.KCLFrontend.OwnsFrontendCode: the admission
// test is that the code lives inside THIS repository.
//
// Deriving happens ONLY when forge.yaml declares no frontends at all. A
// project with a block keeps exactly the inventory it wrote, and its KCL
// is never rendered for this — so this cannot add a frontend an author
// deliberately left out, which is the other half of how the sibling-repo
// case has always been expressed.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/reliant-labs/forge/internal/config"
)

// stepDeriveFrontendInventory recovers the codegen inventory from the
// DEPLOY GRAPH for a project whose frontends the cheap half could not
// find — one at a custom path, declared in KCL and nowhere else.
//
// The cheap half already ran. config.ResolveInventoryAtLoad resolves the
// marker-gated frontends/<name> scan inside LoadProject, so every command
// — lint, doctor, build, this pipeline — already agrees on the inventory
// before this step is reached. What is left here is only the half that
// costs a KCL render per environment (1.6-2.2s each, four environments on
// control-plane), which is why it stays on the commands that already
// render rather than moving to the load seam and taxing shell completion.
//
// So the residual set this closes is narrow and precise: a frontend whose
// code is in this repository at a path the frontends/<name> convention
// cannot express. DiscoverInRepoFrontends deliberately cannot find it —
// a non-conventional layout is exactly the case where a declaration
// should be required rather than guessed.
//
// Best-effort. A project whose KCL does not render (no dev env yet, a
// half-written main.k, kcl not installed) keeps whatever the cheap half
// resolved rather than failing generate: failing here would turn a
// degraded generate into no generate at all.
func stepDeriveFrontendInventory(ctx *pipelineContext) error {
	if ctx.Cfg == nil || len(ctx.Cfg.Frontends) > 0 {
		return nil
	}

	derived := config.DeriveFrontendsFromKCL(ctx.ProjectDir, kclDeclaredFrontends(ctx.ProjectDir))
	if len(derived) == 0 {
		return nil
	}

	ctx.Cfg.Frontends = derived
	// features.frontend is derived from the inventory being non-empty
	// (DeriveFeatureDefaults), and it was resolved at load time against
	// the empty one. Re-resolve so the frontend steps are not gated off
	// by a feature flag computed before the inventory existed.
	//
	// Still required HERE, and only here: the load seam resolves the
	// cheap half BEFORE feature derivation runs, so it needs no repair.
	// This late KCL-only half is the one case that still changes the
	// inventory after features were computed.
	config.ApplyDerivedDefaults(ctx.Cfg)

	names := make([]string, 0, len(derived))
	for _, fe := range derived {
		names = append(names, fmt.Sprintf("%s (%s)", fe.Name, fe.DeclaredDir()))
	}
	fmt.Printf("  ℹ️  forge.yaml declares no frontends; generating for %d found in deploy/kcl or frontends/: %v\n",
		len(names), names)
	return nil
}

// kclDeclaredFrontends collects the frontends every renderable
// environment declares, as the narrow view the inventory rules consume.
//
// Every environment is read, not just dev: a frontend may be declared in
// prod alone (control-plane's internal-console is), and generating for it
// only when a dev env happens to mention it would make the codegen
// inventory depend on which environments exist — the same class of
// silent, order-dependent emptiness this file exists to remove.
//
// Renders go through renderKCLPure for the reason its own header gives:
// evaluating a module fires any file.write in it, and a write that lands
// on a tracked file makes `forge generate` non-reproducible. An
// environment that fails to render contributes nothing and is skipped
// silently — the deploy checks in `forge doctor` are what report an
// unrenderable environment, and duplicating that here would put a KCL
// stack trace in front of every generate on a project mid-edit.
func kclDeclaredFrontends(projectDir string) []config.KCLFrontend {
	var out []config.KCLFrontend
	for _, env := range deployEnvNames(projectDir) {
		entities, restored, err := renderKCLPure(context.Background(), projectDir, env)
		if err != nil || entities == nil {
			continue
		}
		reportImpureRender(env, restored)
		for _, fe := range entities.Frontends {
			out = append(out, config.KCLFrontend{
				Name:      fe.Name,
				Type:      fe.Type,
				Path:      fe.Path,
				HasSource: fe.Source != nil && fe.Source.Repo != "",
			})
		}
	}
	return out
}

// deployEnvNames lists the environments with a deploy/kcl/<env>/main.k,
// sorted so the derived inventory does not depend on directory order.
func deployEnvNames(projectDir string) []string {
	kclDir := filepath.Join(projectDir, "deploy", "kcl")
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(kclDir, e.Name(), "main.k")); statErr != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}
