package deploytarget

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
//  1. Build — run `<dev_runner> install` then `npm run build` in the
//     frontend dir, with the frontend's env_vars injected as build-time
//     env (NEXT_PUBLIC_* / VITE_*). The build emits PublicDir (e.g.
//     "out" for a Next.js static export, "dist" for Vite).
//  2. Assemble — copy PublicDir into a staging tree under BasePath
//     (e.g. <staging>/admin for base_path "/admin"), then copy each
//     Bundle.Src into <staging>/<Bundle.Dest>. The result is one public
//     root: a root SPA with the forge frontend mounted under its prefix.
//  3. Configure — write a firebase.json (hosting.public = staging,
//     hosting.site = Site, plus any Rewrites) and a .firebaserc mapping
//     the hosting Target to Site for the Project.
//  4. Deploy — run `firebase deploy --project <project> --only
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
	Bundle    []FirebaseBundleSpec
	Rewrites  []map[string]any
}

// FirebaseBundleSpec is one extra pre-built static dir assembled into
// the hosting site. Dest empty means the site root.
type FirebaseBundleSpec struct {
	Src  string
	Dest string
}

// Name returns the provider identifier.
func (FirebaseProvider) Name() string { return "firebase" }

func (p FirebaseProvider) runner() commandRunner {
	if p.Runner != nil {
		return p.Runner
	}
	return defaultRunner
}

func (p FirebaseProvider) projectDir() string {
	if p.ProjectDir != "" {
		return p.ProjectDir
	}
	return "."
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
// the registry — no bespoke hand-dispatch in forge deploy.
func (p FirebaseProvider) Deploy(ctx context.Context, group ServiceGroup) error {
	return p.deployFrontends(ctx, group.Frontends, group.DryRun)
}

// Rollback is unsupported for Firebase Hosting: a hosting deploy ships a
// fully-assembled static tree with no forge-tracked previous-tag state,
// and Firebase's own `hosting:rollback` (release history) is the right
// recovery surface. We return ErrProviderNotImplemented so the
// dispatcher records "rollback not supported" rather than silently
// claiming success.
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
// frontend's Firebase deploy. It's computed first so --dry-run can print
// it without touching the filesystem or shelling out, and the real
// deploy path executes against the same plan.
type firebasePlan struct {
	Name        string
	FrontendDir string // absolute frontend source dir
	InstallCmd  []string
	BuildCmd    []string
	BuildEnv    map[string]string
	StagingDir  string // absolute assembled public root
	// Copies is the ordered list of (absoluteSrc → relativeDestUnderStaging)
	// the assembler will perform. The first entry is always the frontend's
	// own public_dir (mounted under base_path); the rest are Bundle dirs.
	Copies        []firebaseCopy
	FirebaseJSON  string   // marshaled firebase.json contents
	FirebaseRC    string   // marshaled .firebaserc contents
	DeployCmd     []string // argv for the firebase deploy invocation
	DeployWorkdir string   // dir the firebase command runs from (StagingDir's parent)
}

type firebaseCopy struct {
	// Src is the absolute source directory.
	Src string
	// DestRel is the destination path RELATIVE to the staging root
	// (".", "admin", "docs/v2"). Empty / "." means the staging root.
	DestRel string
	// Label identifies the source in plan output ("public_dir" / a
	// bundle src).
	Label string
}

// buildPlan resolves a frontend into its firebasePlan. Pure aside from
// path resolution (filepath.Abs) — no build, no copy, no firebase call.
func (p FirebaseProvider) buildPlan(fe FirebaseFrontend) (firebasePlan, error) {
	projDir, err := filepath.Abs(p.projectDir())
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: resolve project dir: %w", fe.Name, err)
	}
	frontendDir := fe.Path
	if !filepath.IsAbs(frontendDir) {
		frontendDir = filepath.Join(projDir, fe.Path)
	}

	staging := p.StagingRoot
	if staging == "" {
		staging = filepath.Join(os.TempDir(), "forge-firebase-"+fe.Name)
	}
	staging, err = filepath.Abs(staging)
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: resolve staging dir: %w", fe.Name, err)
	}

	// public_dir resolves against the frontend source dir.
	publicSrc := fe.Spec.PublicDir
	if !filepath.IsAbs(publicSrc) {
		publicSrc = filepath.Join(frontendDir, fe.Spec.PublicDir)
	}

	copies := []firebaseCopy{{
		Src:     publicSrc,
		DestRel: basePathToDestRel(fe.Spec.BasePath),
		Label:   "public_dir",
	}}
	for _, b := range fe.Spec.Bundle {
		src := b.Src
		if !filepath.IsAbs(src) {
			src = filepath.Join(projDir, b.Src)
		}
		copies = append(copies, firebaseCopy{
			Src:     src,
			DestRel: cleanDestRel(b.Dest),
			Label:   "bundle:" + b.Src,
		})
	}

	installCmd := firebaseInstallCmd(fe.DevRunner)
	buildCmd := []string{"npm", "run", "build"}

	fbJSON, err := renderFirebaseJSON(staging, fe.Spec)
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: render firebase.json: %w", fe.Name, err)
	}
	fbRC, err := renderFirebaseRC(fe.Spec)
	if err != nil {
		return firebasePlan{}, fmt.Errorf("firebase %s: render .firebaserc: %w", fe.Name, err)
	}

	deployWorkdir := filepath.Dir(staging)
	deployCmd := []string{
		"firebase", "deploy",
		"--project", fe.Spec.Project,
		"--only", "hosting:" + fe.Spec.resolvedTarget(),
		"--non-interactive",
	}

	return firebasePlan{
		Name:          fe.Name,
		FrontendDir:   frontendDir,
		InstallCmd:    installCmd,
		BuildCmd:      buildCmd,
		BuildEnv:      fe.BuildEnv,
		StagingDir:    staging,
		Copies:        copies,
		FirebaseJSON:  fbJSON,
		FirebaseRC:    fbRC,
		DeployCmd:     deployCmd,
		DeployWorkdir: deployWorkdir,
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
	fmt.Printf("  [firebase] %s: building (%s) in %s...\n", plan.Name, strings.Join(plan.BuildCmd, " "), plan.FrontendDir)

	if err := runFrontendBuild(ctx, runner, plan.Name, plan.FrontendDir, plan.InstallCmd, plan.BuildCmd, plan.BuildEnv); err != nil {
		return err
	}

	// Assemble phase — fresh staging tree, then copy public_dir + bundles.
	if err := assembleFirebaseStaging(plan); err != nil {
		return fmt.Errorf("firebase %s: assemble: %w", plan.Name, err)
	}

	// Configure phase — firebase.json + .firebaserc next to the staging
	// tree (in its parent, which is the firebase deploy workdir).
	if err := os.WriteFile(filepath.Join(plan.DeployWorkdir, "firebase.json"), []byte(plan.FirebaseJSON), 0o644); err != nil {
		return fmt.Errorf("firebase %s: write firebase.json: %w", plan.Name, err)
	}
	if err := os.WriteFile(filepath.Join(plan.DeployWorkdir, ".firebaserc"), []byte(plan.FirebaseRC), 0o644); err != nil {
		return fmt.Errorf("firebase %s: write .firebaserc: %w", plan.Name, err)
	}

	// Deploy phase — firebase deploy from the workdir so it picks up the
	// generated firebase.json + .firebaserc.
	fmt.Printf("  [firebase] %s: deploying to project=%s site=%s target=%s...\n",
		plan.Name, fe.Spec.Project, fe.Spec.Site, fe.Spec.resolvedTarget())
	if err := runInDir(ctx, runner, plan.DeployWorkdir, nil, plan.DeployCmd); err != nil {
		return fmt.Errorf("firebase %s: deploy: %w", plan.Name, err)
	}
	fmt.Printf("  [firebase] %s: deployed.\n", plan.Name)
	return nil
}

// runFrontendBuild runs the install + build phase for a frontend in
// frontendDir. The two phases get DELIBERATELY DIFFERENT env:
//
//   - INSTALL runs under NODE_ENV=development (installEnv) so the package
//     manager pulls the FULL dependency set, devDependencies included.
//     The build toolchain (typescript, bundlers, next's config loader)
//     lives in devDependencies — under NODE_ENV=production, `npm install`
//     SKIPS them and the subsequent build dies with "Cannot find module
//     'typescript'" (Next.js needs typescript to load next.config.ts).
//     The frontend's inline env_vars are NOT injected here: they're
//     build-time values (NEXT_PUBLIC_* / VITE_*), irrelevant to install.
//   - BUILD runs under NODE_ENV=production with the inline env_vars
//     layered on (buildTimeEnv), so the static-export path (Next.js
//     `output: "export"` gated on NODE_ENV) and Vite's production mode
//     both engage and NEXT_PUBLIC_*/VITE_* are baked in.
//
// Shared by the Firebase deploy path (deployOne) and the build-only path
// (BuildOnly) so the two never drift on install command, build command,
// or env semantics.
func runFrontendBuild(ctx context.Context, runner commandRunner, name, frontendDir string, installCmd, buildCmd []string, extraEnv map[string]string) error {
	if err := runInDir(ctx, runner, frontendDir, installEnv(), installCmd); err != nil {
		return fmt.Errorf("frontend %s: install: %w", name, err)
	}
	if err := runInDir(ctx, runner, frontendDir, buildTimeEnv(extraEnv), buildCmd); err != nil {
		return fmt.Errorf("frontend %s: build: %w", name, err)
	}
	return nil
}

// installEnv is the env overlay for the dependency-install phase. It
// forces NODE_ENV=development so devDependencies are installed even when
// the ambient/inherited NODE_ENV is "production" (the package manager
// skips devDeps under production). Set explicitly rather than left empty:
// runInDir inherits the parent process env when the overlay is empty, so
// an inherited NODE_ENV=production would otherwise leak through and strip
// the build toolchain (typescript, bundlers) the build phase needs.
func installEnv() map[string]string {
	return map[string]string{"NODE_ENV": "development"}
}

// buildTimeEnv layers a frontend's inline env_vars over a forced
// NODE_ENV=production. Extracted so the deploy path and the build-only
// path produce byte-identical build env.
func buildTimeEnv(extraEnv map[string]string) map[string]string {
	env := map[string]string{"NODE_ENV": "production"}
	for k, v := range extraEnv {
		env[k] = v
	}
	return env
}

// BuildOnlyFrontend is a frontend that forge must BUILD (env-injected)
// but NOT deploy — a `deploy = None` frontend. Its build output (e.g. a
// Next.js static export under PublicDir) becomes available on disk so a
// sibling FirebaseHosting frontend can assemble it into its hosting
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
	projDir, err := filepath.Abs(p.projectDir())
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
		InstallCmd:  firebaseInstallCmd(fe.DevRunner),
		BuildCmd:    []string{"npm", "run", "build"},
		BuildEnv:    buildTimeEnv(fe.BuildEnv),
		EmittedDir:  emitted,
	}, nil
}

// BuildOnly builds each build-only frontend (install + `npm run build`
// with its env_vars injected) so its output exists on disk before any
// FirebaseHosting frontend assembles a bundle that references it. dryRun
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
	fmt.Fprintf(w, "  [DRY-RUN] build-only plan for frontend %q:\n", plan.Name)
	fmt.Fprintf(w, "    build dir:    %s\n", plan.FrontendDir)
	fmt.Fprintf(w, "    [DRY-RUN] would exec: %s\n", strings.Join(plan.InstallCmd, " "))
	if len(plan.BuildEnv) > 0 {
		fmt.Fprintf(w, "    build env:    %s\n", formatBuildEnv(plan.BuildEnv))
	}
	fmt.Fprintf(w, "    [DRY-RUN] would exec: %s (NODE_ENV=production)\n", strings.Join(plan.BuildCmd, " "))
	if plan.EmittedDir != "" {
		fmt.Fprintf(w, "    emits dir:    %s\n", plan.EmittedDir)
	}
}

// runInDir runs a command in dir with an optional env overlay. The
// commandRunner abstraction doesn't carry a working dir, so we shell via
// `sh -c 'cd <dir> && <cmd>'` to keep the seam (and the test double)
// unchanged. dir is quoted to tolerate spaces.
func runInDir(ctx context.Context, runner commandRunner, dir string, env map[string]string, argv []string) error {
	if len(argv) == 0 {
		return nil
	}
	script := fmt.Sprintf("cd %s && %s", shellQuote(dir), strings.Join(quoteArgv(argv), " "))
	if len(env) > 0 {
		return runner.RunWithEnv(ctx, env, "sh", "-c", script)
	}
	return runner.Run(ctx, "sh", "-c", script)
}

// quoteArgv shell-quotes each token so an argv slice round-trips through
// `sh -c`. Cheap single-quote escaping; sufficient for npm / firebase /
// flag tokens.
func quoteArgv(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = shellQuote(a)
	}
	return out
}

// shellQuote wraps s in single quotes, escaping any embedded single
// quotes. Empty string becomes ”.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n'\"\\$`&|;<>(){}*?[]#~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// firebaseInstallCmd returns the dependency-install command for a dev
// runner. npm uses `npm ci` when a lockfile is present at runtime — but
// to keep the plan deterministic (and not stat the lockfile during
// planning) we use `npm install`, which works with or without a
// lockfile. pnpm/yarn use their `install` verb.
func firebaseInstallCmd(devRunner string) []string {
	switch devRunner {
	case "pnpm":
		return []string{"pnpm", "install"}
	case "yarn":
		return []string{"yarn", "install"}
	default:
		return []string{"npm", "install"}
	}
}

// basePathToDestRel maps a frontend base_path ("/admin", "") to the
// staging-relative destination ("admin", "."). A root mount (empty
// base_path) lands at the staging root.
func basePathToDestRel(basePath string) string {
	return cleanDestRel(basePath)
}

// cleanDestRel normalises a dest path (base_path or bundle.dest) into a
// staging-relative directory: leading/trailing slashes stripped, empty
// becomes ".". Defends against absolute / "./" / trailing-slash inputs
// so the assembled layout is predictable.
func cleanDestRel(dest string) string {
	d := strings.Trim(strings.TrimSpace(dest), "/")
	d = filepath.Clean(d)
	if d == "" || d == "." || d == "/" {
		return "."
	}
	return d
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

// assembleFirebaseStaging realises plan.Copies into a fresh staging tree.
// It removes any prior staging dir first so a re-deploy doesn't inherit
// stale files, then copies each source into its destination under the
// staging root.
func assembleFirebaseStaging(plan firebasePlan) error {
	if err := os.RemoveAll(plan.StagingDir); err != nil {
		return fmt.Errorf("clean staging: %w", err)
	}
	if err := os.MkdirAll(plan.StagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging: %w", err)
	}
	for _, c := range plan.Copies {
		dst := plan.StagingDir
		if c.DestRel != "." {
			dst = filepath.Join(plan.StagingDir, c.DestRel)
		}
		if _, err := os.Stat(c.Src); err != nil {
			return fmt.Errorf("source %s (%s): %w", c.Src, c.Label, err)
		}
		if err := copyDir(c.Src, dst); err != nil {
			return fmt.Errorf("copy %s -> %s: %w", c.Src, dst, err)
		}
	}
	return nil
}

// copyDir recursively copies src into dst, creating dst (and parents).
// Plain file copy — symlinks are dereferenced (static export output is
// regular files). Sufficient for assembling a static hosting tree.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode())
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// printFirebasePlan renders the dry-run plan for one frontend. Output is
// stable + greppable: the build command, the assembled layout (one line
// per copy, with the destination mount), and the exact firebase deploy
// command. Mirrors the External provider's "[DRY-RUN] would exec" style.
func printFirebasePlan(w io.Writer, plan firebasePlan) {
	fmt.Fprintf(w, "  [DRY-RUN] firebase deploy plan for frontend %q:\n", plan.Name)
	fmt.Fprintf(w, "    build dir:    %s\n", plan.FrontendDir)
	fmt.Fprintf(w, "    [DRY-RUN] would exec: %s\n", strings.Join(plan.InstallCmd, " "))
	if len(plan.BuildEnv) > 0 {
		fmt.Fprintf(w, "    build env:    %s\n", formatBuildEnv(plan.BuildEnv))
	}
	fmt.Fprintf(w, "    [DRY-RUN] would exec: %s (NODE_ENV=production)\n", strings.Join(plan.BuildCmd, " "))
	fmt.Fprintf(w, "    assemble into %s:\n", plan.StagingDir)
	for _, c := range plan.Copies {
		mount := "/"
		if c.DestRel != "." {
			mount = "/" + c.DestRel
		}
		fmt.Fprintf(w, "      %-18s -> %s   (%s)\n", c.Src, mount, c.Label)
	}
	fmt.Fprintf(w, "    firebase.json (hosting.public=%s):\n", filepath.Base(plan.StagingDir))
	for _, line := range strings.Split(strings.TrimRight(plan.FirebaseJSON, "\n"), "\n") {
		fmt.Fprintf(w, "      %s\n", line)
	}
	fmt.Fprintf(w, "    [DRY-RUN] would exec (cwd %s): %s\n",
		plan.DeployWorkdir, strings.Join(plan.DeployCmd, " "))
}

// formatBuildEnv renders the build-time env map as a stable, sorted
// KEY=VALUE list for the dry-run plan.
func formatBuildEnv(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+env[k])
	}
	return strings.Join(parts, " ")
}
