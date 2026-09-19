package deploytarget

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// FrontendConfigJSName is the filename of the runtime config document
// inside a deployed site. It MUST agree with
// codegen.FrontendConfigJSFile — the generator emits the <script src>
// that loads it, and this package writes the file that src resolves to.
// It is duplicated rather than imported because this package deliberately
// has no dependency on the codegen layer; the agreement is pinned by a
// test rather than by the type system.
const FrontendConfigJSName = "config.js"

// FirebaseProvider deploys a frontend's static build output to Firebase
// Hosting. It is the frontend analogue of ExternalProvider — but unlike
// External it is NOT a generic shell escape hatch: the contract is
// "build a static export, assemble it (plus any sibling static dirs)
// into one tree, and ship that tree to a Firebase Hosting site." Firebase
// Hosting is a common-enough target (per-env preview/staging/prod sites,
// SPA + sub-app co-hosting under a base_path) that owning the assembly +
// firebase.json generation in forge — rather than asking every project
// to hand-roll it in CI — is the coherent product move.
//
// The pipeline, per frontend:
//
//  1. Build + assemble — the SHARED static staging step (staticstage.go):
//     install, `npm run build` with the frontend's env injected, copy
//     public_dir under base_path plus every bundle dir, then write the
//     environment's runtime config document last. StaticSiteProvider runs
//     the identical step, which is why it lives there and not here.
//  2. Configure — write a firebase.json (hosting.public = staging,
//     hosting.site = Site, plus any Rewrites) and a .firebaserc mapping
//     the hosting Target to Site for the Project.
//  3. Deploy — run `firebase deploy --project <project> --only
//     hosting:<target> --non-interactive` from the staging parent.
//
// --dry-run prints the resolved plan (build command, assembled layout,
// and the exact firebase deploy command) and performs NO build, NO file
// assembly side effects beyond an in-memory plan, and NO firebase call.
type FirebaseProvider struct {
	// ProjectDir is the project root. Frontend paths and Bundle.Src
	// resolve against it. Empty means the current working directory.
	ProjectDir string

	// Runner is the os/exec indirection (npm / firebase). Nil falls
	// back to the package default. Tests inject a fake runner.
	Runner commandRunner

	// StagingRoot overrides where the assembled hosting tree is written.
	// Empty means a temp dir under os.TempDir(). Tests set it so they
	// can inspect the assembled layout.
	StagingRoot string
}

// FirebaseFrontend is one frontend the Firebase provider should deploy.
// It carries the resolved build inputs plus the FirebaseHosting spec.
// The CLI builds this from the rendered KCL FrontendEntity; tests
// construct it directly.
type FirebaseFrontend struct {
	// Name is the forge frontend name (logging + target fallbacks).
	Name string

	// Path is the frontend source dir relative to the project root —
	// where `npm install` / `npm run build` run.
	Path string

	// DevRunner is "npm" (default) | "pnpm" | "yarn"; selects the
	// install command. The build command is always `<runner> run build`.
	DevRunner string

	// BuildEnv is the build-time env injected into the build process
	// (NEXT_PUBLIC_* / VITE_*). Layered on top of os.Environ().
	BuildEnv map[string]string

	// RuntimeConfigJS is this ENVIRONMENT's rendered runtime config
	// document — the `window.__FORGE_CONFIG__ = {...}` script the browser
	// loads before any bundle code runs, produced from the environment's
	// KCL (frontend_config_gen.k's runtime projection).
	//
	// It is written into the assembled tree AFTER the built bundle is
	// copied, which is what makes promotion real: the bundle is
	// environment-agnostic and this one file is the only part that
	// differs, so the same bytes ship to dev and prod carrying different
	// configuration. Empty means the frontend declares no typed config —
	// nothing is written and the assembled layout is unchanged.
	RuntimeConfigJS string

	// Spec is the FirebaseHosting deploy config.
	Spec FirebaseHostingSpec
}

// FirebaseHostingSpec mirrors the kcl/schema.k FirebaseHosting schema
// (and the CLI-side FirebaseHostingDeploy entity). Kept in this package
// so the provider has no import on internal/cli.
type FirebaseHostingSpec struct {
	Project   string
	Site      string
	Target    string
	PublicDir string
	BasePath  string
	Bundle    []BundleDirSpec
	Rewrites  []map[string]any
}

// Name returns the provider identifier.
func (FirebaseProvider) Name() string { return "firebase" }

func (p FirebaseProvider) runner() commandRunner {
	if p.Runner != nil {
		return p.Runner
	}
	return defaultRunner
}

// resolvedTarget returns the hosting selector for `--only hosting:<x>`.
// When an explicit Target alias is declared it's used (resolved via the
// generated .firebaserc target→site mapping); otherwise the bare site id
// is used — `firebase deploy --only hosting:<site>` accepts a site id
// directly, no target alias required.
func (s FirebaseHostingSpec) resolvedTarget() string {
	if s.Target != "" {
		return s.Target
	}
	return s.Site
}

// hasExplicitTarget reports whether the spec declares a real hosting
// target alias (distinct from defaulting to the site id). This is the
// switch that keeps `site` and `target` MUTUALLY EXCLUSIVE in the
// rendered firebase.json: the firebase CLI rejects a hosting config that
// carries BOTH on `deploy --only hosting:<x>`. With an explicit target we
// emit `target` (resolved via .firebaserc); without one we emit `site`
// (and deploy by site id directly).
func (s FirebaseHostingSpec) hasExplicitTarget() bool {
	return s.Target != ""
}

// Deploy ships every frontend in the group to its Firebase Hosting
// site. It reads the frontends off group.Frontends and the dry-run knob
// off group.DryRun so the Firebase provider satisfies the same Provider
// interface as k8s-cluster / external / compose and dispatches through
// the registry — no bespoke hand-dispatch in forge env deploy.
func (p FirebaseProvider) Deploy(ctx context.Context, group ServiceGroup) error {
	return p.deployFrontends(ctx, group.Frontends, group.DryRun)
}

// Rollback is unsupported for Firebase Hosting: a hosting deploy ships a
// fully-assembled static tree with no forge-tracked previous-tag state,
// and Firebase's own `hosting:rollback` (release history) is the right
// recovery surface. We return ErrProviderNotImplemented so the
// dispatcher records "rollback not supported" rather than silently
// claiming success.
//
// (StaticSiteProvider, by contrast, DOES support rollback: it archives
// every deploy's tree under a content digest in the bucket, so a previous
// artifact is still there to re-point at. Firebase owns its own release
// history, so duplicating that here would be forge second-guessing the
// target's native affordance.)
func (FirebaseProvider) Rollback(_ context.Context, _ ServiceGroup, _ string) error {
	return fmt.Errorf("firebase: rollback not supported (use `firebase hosting:rollback`): %w", ErrProviderNotImplemented)
}

// deployFrontends builds, assembles, configures, and ships each frontend
// to its Firebase Hosting site. dryRun prints the plan and skips every
// side effect.
func (p FirebaseProvider) deployFrontends(ctx context.Context, fes []FirebaseFrontend, dryRun bool) error {
	for _, fe := range fes {
		if err := p.deployOne(ctx, fe, dryRun); err != nil {
			return err
		}
	}
	return nil
}

// firebasePlan is the resolved, side-effect-free description of one
// frontend's Firebase deploy: the shared build-and-assemble StagePlan
// plus the Firebase-specific configure and deploy steps.
type firebasePlan struct {
	Stage         StagePlan
	FirebaseJSON  string   // marshaled firebase.json contents
	FirebaseRC    string   // marshaled .firebaserc contents
	DeployCmd     []string // argv for the firebase deploy invocation
	DeployWorkdir string   // dir the firebase command runs from (StagingDir's parent)
}

// stageInput projects a FirebaseFrontend onto the target-neutral
// StageInput the shared build-and-assemble step consumes.
func (p FirebaseProvider) stageInput(fe FirebaseFrontend) StageInput {
	staging := p.StagingRoot
	if staging == "" {
		staging = filepath.Join(os.TempDir(), "forge-firebase-"+fe.Name)
	}
	return StageInput{
		Name:            fe.Name,
		Path:            fe.Path,
		DevRunner:       fe.DevRunner,
		BuildEnv:        fe.BuildEnv,
		PublicDir:       fe.Spec.PublicDir,
		BasePath:        fe.Spec.BasePath,
		Bundle:          fe.Spec.Bundle,
		RuntimeConfigJS: fe.RuntimeConfigJS,
		ProjectDir:      p.ProjectDir,
		StagingRoot:     staging,
	}
}

// buildPlan resolves a frontend into its firebasePlan. Pure aside from
// path resolution (filepath.Abs) — no build, no copy, no firebase call.
func (p FirebaseProvider) buildPlan(fe FirebaseFrontend) (firebasePlan, error) {
	stage, err := buildStagePlan(p.stageInput(fe))
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: %w", fe.Name, err)
	}

	fbJSON, err := renderFirebaseJSON(stage.StagingDir, fe.Spec)
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: render firebase.json: %w", fe.Name, err)
	}
	fbRC, err := renderFirebaseRC(fe.Spec)
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: render .firebaserc: %w", fe.Name, err)
	}

	return firebasePlan{
		Stage:        stage,
		FirebaseJSON: fbJSON,
		FirebaseRC:   fbRC,
		DeployCmd: []string{
			"firebase", "deploy",
			"--project", fe.Spec.Project,
			"--only", "hosting:" + fe.Spec.resolvedTarget(),
			"--non-interactive",
		},
		DeployWorkdir: filepath.Dir(stage.StagingDir),
	}, nil
}

func (p FirebaseProvider) deployOne(ctx context.Context, fe FirebaseFrontend, dryRun bool) error {
	plan, err := p.buildPlan(fe)
	if err != nil {
		return err
	}

	if dryRun {
		printFirebasePlan(os.Stdout, plan)
		return nil
	}

	runner := p.runner()
	fmt.Printf("  [firebase] %s: building (%s) in %s...\n",
		plan.Stage.Name, strings.Join(plan.Stage.BuildCmd, " "), plan.Stage.FrontendDir)

	// Build + assemble — the shared static staging step.
	if err := runStagePlan(ctx, runner, plan.Stage); err != nil {
		return err
	}

	// Configure phase — firebase.json + .firebaserc next to the staging
	// tree (in its parent, which is the firebase deploy workdir).
	if err := os.WriteFile(filepath.Join(plan.DeployWorkdir, "firebase.json"), []byte(plan.FirebaseJSON), 0o644); err != nil {
		return fmt.Errorf("firebase %s: write firebase.json: %w", plan.Stage.Name, err)
	}
	if err := os.WriteFile(filepath.Join(plan.DeployWorkdir, ".firebaserc"), []byte(plan.FirebaseRC), 0o644); err != nil {
		return fmt.Errorf("firebase %s: write .firebaserc: %w", plan.Stage.Name, err)
	}

	// Deploy phase — firebase deploy from the workdir so it picks up the
	// generated firebase.json + .firebaserc.
	fmt.Printf("  [firebase] %s: deploying to project=%s site=%s target=%s...\n",
		plan.Stage.Name, fe.Spec.Project, fe.Spec.Site, fe.Spec.resolvedTarget())
	if err := runInDir(ctx, runner, plan.DeployWorkdir, nil, plan.DeployCmd); err != nil {
		return fmt.Errorf("firebase %s: deploy: %w", plan.Stage.Name, err)
	}
	fmt.Printf("  [firebase] %s: deployed.\n", plan.Stage.Name)
	return nil
}

// BuildOnlyFrontend is a frontend that forge must BUILD (env-injected)
// but NOT deploy — a `deploy = None` frontend. Its build output (e.g. a
// Next.js static export under PublicDir) becomes available on disk so a
// sibling FirebaseHosting / StaticSite frontend can assemble it into its
// bundle. Mirrors the build inputs of FirebaseFrontend minus any deploy
// spec.
type BuildOnlyFrontend struct {
	// Name is the forge frontend name (logging).
	Name string

	// Path is the frontend source dir relative to the project root —
	// where install / `npm run build` run.
	Path string

	// DevRunner is "npm" (default) | "pnpm" | "yarn"; selects the
	// install command.
	DevRunner string

	// BuildEnv is the build-time env injected into the build process
	// (NEXT_PUBLIC_* / VITE_*). Layered over NODE_ENV=production.
	BuildEnv map[string]string

	// PublicDir is the build-output dir the build emits (relative to
	// Path), e.g. "out" for a Next.js static export. Used for dry-run
	// reporting of the emitted directory.
	PublicDir string
}

// buildOnlyPlan is the resolved, side-effect-free description of one
// build-only frontend's build. Computed first so --dry-run can print it
// without shelling out, and the real build executes against the same plan.
type buildOnlyPlan struct {
	Name        string
	FrontendDir string // absolute frontend source dir
	InstallCmd  []string
	BuildCmd    []string
	BuildEnv    map[string]string
	EmittedDir  string // absolute build-output dir the build is expected to emit
}

// buildOnlyPlanFor resolves a BuildOnlyFrontend into its buildOnlyPlan.
// Pure aside from path resolution (filepath.Abs).
func (p FirebaseProvider) buildOnlyPlanFor(fe BuildOnlyFrontend) (buildOnlyPlan, error) {
	projDir := p.ProjectDir
	if projDir == "" {
		projDir = "."
	}
	projDir, err := filepath.Abs(projDir)
	if err != nil {
		return buildOnlyPlan{}, fmt.Errorf("build-only %s: resolve project dir: %w", fe.Name, err)
	}
	frontendDir := fe.Path
	if !filepath.IsAbs(frontendDir) {
		frontendDir = filepath.Join(projDir, fe.Path)
	}
	emitted := fe.PublicDir
	if emitted != "" && !filepath.IsAbs(emitted) {
		emitted = filepath.Join(frontendDir, fe.PublicDir)
	}
	return buildOnlyPlan{
		Name:        fe.Name,
		FrontendDir: frontendDir,
		InstallCmd:  frontendInstallCmd(fe.DevRunner),
		BuildCmd:    []string{"npm", "run", "build"},
		BuildEnv:    buildTimeEnv(fe.BuildEnv),
		EmittedDir:  emitted,
	}, nil
}

// BuildOnly builds each build-only frontend (install + `npm run build`
// with its env_vars injected) so its output exists on disk before any
// deploying frontend assembles a bundle that references it. dryRun
// prints the build plan and performs no side effects, mirroring the
// Firebase deploy dry-run.
func (p FirebaseProvider) BuildOnly(ctx context.Context, fes []BuildOnlyFrontend, dryRun bool) error {
	for _, fe := range fes {
		plan, err := p.buildOnlyPlanFor(fe)
		if err != nil {
			return err
		}
		if dryRun {
			printBuildOnlyPlan(os.Stdout, plan)
			continue
		}
		fmt.Printf("  [build-only] %s: building (%s) in %s...\n", plan.Name, strings.Join(plan.BuildCmd, " "), plan.FrontendDir)
		if err := runFrontendBuild(ctx, p.runner(), plan.Name, plan.FrontendDir, plan.InstallCmd, plan.BuildCmd, fe.BuildEnv); err != nil {
			return err
		}
		fmt.Printf("  [build-only] %s: built", plan.Name)
		if plan.EmittedDir != "" {
			fmt.Printf(" -> %s", plan.EmittedDir)
		}
		fmt.Println(".")
	}
	return nil
}

// printBuildOnlyPlan renders the dry-run plan for one build-only
// frontend: the install + build commands, the injected build env, and
// the emitted output dir. Mirrors printFirebasePlan's "[DRY-RUN] would
// exec" style.
func printBuildOnlyPlan(w io.Writer, plan buildOnlyPlan) {
	_, _ = fmt.Fprintf(w, "  [DRY-RUN] build-only plan for frontend %q:\n", plan.Name)
	_, _ = fmt.Fprintf(w, "    build dir:    %s\n", plan.FrontendDir)
	_, _ = fmt.Fprintf(w, "    [DRY-RUN] would exec: %s\n", strings.Join(plan.InstallCmd, " "))
	if len(plan.BuildEnv) > 0 {
		_, _ = fmt.Fprintf(w, "    build env:    %s\n", formatBuildEnv(plan.BuildEnv))
	}
	_, _ = fmt.Fprintf(w, "    [DRY-RUN] would exec: %s (NODE_ENV=production)\n", strings.Join(plan.BuildCmd, " "))
	if plan.EmittedDir != "" {
		_, _ = fmt.Fprintf(w, "    emits dir:    %s\n", plan.EmittedDir)
	}
}

// renderFirebaseJSON builds the firebase.json contents. `hosting.public`
// is the staging dir (relative to the deploy workdir, which is its
// parent — so just the basename). `hosting.site` pins the target site;
// rewrites pass through verbatim. ignore mirrors the firebase defaults so
// the generated config files don't get uploaded.
func renderFirebaseJSON(stagingDir string, spec FirebaseHostingSpec) (string, error) {
	hosting := map[string]any{
		"public": filepath.Base(stagingDir),
		"ignore": []string{"firebase.json", "**/.*", "**/node_modules/**"},
	}
	// site and target are MUTUALLY EXCLUSIVE in a firebase.json hosting
	// config — the firebase CLI errors out ("Cannot have both site and
	// target ...") on `deploy --only hosting:<x>` when both are present.
	// Emit `target` only when an explicit alias is declared (resolved via
	// the .firebaserc target→site map); otherwise emit the bare `site`.
	if spec.hasExplicitTarget() {
		hosting["target"] = spec.Target
	} else {
		hosting["site"] = spec.Site
	}
	if len(spec.Rewrites) > 0 {
		hosting["rewrites"] = spec.Rewrites
	}
	doc := map[string]any{"hosting": hosting}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// renderFirebaseRC builds the .firebaserc contents: the default project
// plus a hosting target → site mapping so `--only hosting:<target>`
// resolves. Mirrors what `firebase target:apply hosting <target> <site>`
// would write.
func renderFirebaseRC(spec FirebaseHostingSpec) (string, error) {
	doc := map[string]any{
		"projects": map[string]any{"default": spec.Project},
	}
	// The target→site mapping is only meaningful when firebase.json
	// references a target alias. Without an explicit Target, firebase.json
	// carries `site` directly and the deploy selects `--only
	// hosting:<site>` by site id, so no target alias is configured (an
	// orphan mapping whose alias nothing references is dead config).
	if spec.hasExplicitTarget() {
		doc["targets"] = map[string]any{
			spec.Project: map[string]any{
				"hosting": map[string]any{
					spec.Target: []string{spec.Site},
				},
			},
		}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(b) + "\n", nil
}

// printFirebasePlan renders the dry-run plan for one frontend. Output is
// stable + greppable: the build command, the assembled layout (one line
// per copy, with the destination mount), and the exact firebase deploy
// command. Mirrors the External provider's "[DRY-RUN] would exec" style.
func printFirebasePlan(w io.Writer, plan firebasePlan) {
	_, _ = fmt.Fprintf(w, "  [DRY-RUN] firebase deploy plan for frontend %q:\n", plan.Stage.Name)
	printStagePlanBuild(w, plan.Stage)
	_, _ = fmt.Fprintf(w, "    firebase.json (hosting.public=%s):\n", filepath.Base(plan.Stage.StagingDir))
	for _, line := range strings.Split(strings.TrimRight(plan.FirebaseJSON, "\n"), "\n") {
		_, _ = fmt.Fprintf(w, "      %s\n", line)
	}
	_, _ = fmt.Fprintf(w, "    [DRY-RUN] would exec (cwd %s): %s\n",
		plan.DeployWorkdir, strings.Join(plan.DeployCmd, " "))
}
