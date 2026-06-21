package generator

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// guardTemplateMode maps a ConfigGuardConfig to the TypedAccessGuard string
// the golangci.yml template branches on: "" when the guardrail is OFF (so
// the template's {{if .TypedAccessGuard}} blocks render nothing), else the
// effective "warn"/"error" mode. Collapsing "off" to "" keeps the template
// conditionals simple truthiness checks.
func guardTemplateMode(g config.ConfigGuardConfig) string {
	if !g.TypedAccessGuardEnabled() {
		return ""
	}
	return g.EffectiveEnforceTypedAccess()
}

// projectTemplateData is the single render payload for every frozen
// project-level template. It used to be modeled twice: an anonymous
// ~18-field struct built inline in ProjectGenerator.Generate() (the
// scaffold lane) and a hand-mirrored upgradeTemplateData (the upgrade /
// regenerate lane). The two drifted field-by-field and reimplemented the
// same per-field derivations (protoName, goVersionMinor,
// dockerBuilderGoVersion, NormalizeAuthProvider). Promoting to one named
// type with two constructors — ForScaffold (from a *ProjectGenerator) and
// ForUpgrade (from a *config.ProjectConfig) — keeps the field set in one
// place; snapshot tests guard that both lanes still emit identical output.
//
// The type is a superset: a handful of fields are populated by exactly one
// lane (the other leaves the zero value). That is safe because the
// managed templates that render in BOTH lanes never branch on a
// lane-specific field:
//
//   - ServicePackage / ForgePkgVersion / ForgePkgDevReplace — scaffold-only.
//     Consumed by scaffold-only templates (user-example.proto.tmpl,
//     config.proto.tmpl, go.mod.tmpl) that the upgrade lane never renders.
//   - Services — upgrade-only. Consumed by alloy-config.alloy.tmpl, which
//     the scaffold lane renders through a separate local struct
//     (generateAlloyConfig), so the scaffold payload never needs it.
type projectTemplateData struct {
	Name                   string
	ProtoName              string
	Module                 string
	ServiceName            string
	ServicePackage         string
	ServicePort            int
	ProjectName            string
	FrontendName           string
	FrontendPort           int
	GoVersion              string
	GoVersionMinor         string
	DockerBuilderGoVersion string
	// Services lists (name, port) pairs for templates like alloy-config.
	// Populated by ForUpgrade only — the scaffold lane renders alloy-config
	// through its own local struct, so ForScaffold leaves this nil.
	Services     []ServiceInfo
	ConfigFields map[string]bool
	// LocalForgePkgVendored indicates whether <projectDir>/.forge-pkg/
	// holds a vendored copy of forge/pkg (sibling-checkout dev mode).
	// At scaffold time this is normally false; the Dockerfile template uses
	// it to gate the COPY .forge-pkg/ ./.forge-pkg/ line. The upgrade lane
	// detects it from the presence of .forge-pkg/go.mod on disk.
	LocalForgePkgVendored bool
	// RESTEnabled mirrors the `api.rest` toggle in forge.yaml. At scaffold
	// time this is always false (REST is opt-in via a post-scaffold edit),
	// but the field is declared here so buf.yaml's dep gate has a known
	// input shape; the upgrade lane reads the live forge.yaml api.rest value.
	RESTEnabled bool
	// ForgePkgVersion / ForgePkgDevReplace drive the forge/pkg dependency
	// block in go.mod.tmpl. Exactly one (or neither) is non-empty — see
	// resolveForgePkgDep in project_pkgdep.go and docs/pkg-versioning.md
	// for the dev-vs-release model. Populated by ForScaffold only (go.mod
	// is not an upgrade-managed file).
	ForgePkgVersion    string
	ForgePkgDevReplace string
	// ForgePkgGenReplace is the forge/pkg replace target the gen/ submodule's
	// go.mod must carry in dev mode. The root replace (ForgePkgDevReplace)
	// lives in the ROOT module and does not cascade into the separate gen/
	// submodule, so gen/go.mod needs its own depth-corrected replace or
	// `go mod tidy` in gen/ can't resolve the unpublished forge/pkg. At
	// scaffold time ForgePkgDevReplace is a host-absolute sibling path, which
	// is depth-independent — so this is the same absolute path (or empty in
	// release / no-sibling mode). The generate pipeline's ensure-gen-module
	// step later reconciles this against the root replace's final form (e.g.
	// after vendoring rewrites the root to ./.forge-pkg). Populated by
	// ForScaffold only — gen/go.mod is not an upgrade-managed file.
	ForgePkgGenReplace string
	// AuthProvider / AuthProviderExternal gate cmd-server.go.tmpl's
	// generated-auth call site. Always zero at scaffold time (forge new
	// never configures an auth provider); the upgrade lane derives them
	// from the live forge.yaml auth.provider via NormalizeAuthProvider.
	AuthProvider         string
	AuthProviderExternal bool
	// VersionVar mirrors forge.yaml build.version_var. The Dockerfile
	// template stamps an extra `-X <VersionVar>=${FORGE_VERSION}` when set;
	// empty (the default) renders nothing, preserving main.version-only
	// stamping for projects that don't set it.
	VersionVar string
	// TypedAccessGuard is the env-access guardrail strictness the
	// golangci.yml.tmpl branches on. It is "" when the guardrail is OFF (the
	// template's {{if .TypedAccessGuard}} then emits nothing), "warn" when
	// advisory, or "error" when gating — i.e. it carries the EFFECTIVE mode
	// EXCEPT that "off" is collapsed to "". The scaffold lane writes "error"
	// (greenfield); the upgrade lane reads the live forge.yaml
	// config.enforce_typed_access (absent → "warn"). See
	// config.ConfigGuardConfig and guardTemplateMode below.
	TypedAccessGuard string
	// LoaderPackage is the allowlisted package path the guardrail excludes
	// from forbidigo (forge's generated config loader). Defaults to
	// config.DefaultLoaderPackage ("pkg/config").
	LoaderPackage string
	// Binaries enumerates every buildable entrypoint the project ships —
	// one per `cmd/<bin>/` directory on disk (devspace idiom: each binary
	// gets its own cmd/<bin>/main.go). The Dockerfile template ranges over
	// this to `go build -o /app/<bin> ./cmd/<bin>` for EACH binary into a
	// single image. Primary is the server entrypoint (cmd/<projectName>/),
	// the one the production stage runs by default; secondary binaries
	// (proxy / ctl / worker-style) are run by overriding the container
	// command to /app/<bin> per-workload at deploy time.
	//
	// Enumerated from disk (not the component list) so it captures every
	// real cmd/<bin>/ — including binaries that predate the component
	// registry or were added by hand. At scaffold time only the primary
	// exists; ForUpgrade re-scans so a project that ran `forge add binary`
	// gets every entrypoint built. Falls back to just the primary when the
	// cmd/ tree can't be read.
	Binaries []BinaryBuild
}

// BinaryBuild is one buildable entrypoint: the cmd/<Dir>/ leaf, which is
// also the output binary name (/app/<Dir>).
type BinaryBuild struct {
	// Dir is the cmd/<Dir>/ leaf — the `go build ./cmd/<Dir>` package path
	// segment AND the output binary basename (/app/<Dir>).
	Dir string
	// Primary marks the server entrypoint (cmd/<projectName>/) that the
	// production image runs by default (ENTRYPOINT/CMD). Exactly one binary
	// is primary; the rest are run via a per-workload command override.
	Primary bool
}

// discoverBinaries enumerates the project's buildable entrypoints from the
// cmd/ tree on disk. Every cmd/<dir>/ that contains a main.go is one
// binary, output to /app/<dir>. primary (the cmd/<projectName>/ server
// leaf) is marked Primary and sorted first; the rest follow in name order
// for a deterministic Dockerfile.
//
// Disk enumeration — not the component registry — is the source of truth:
// it captures hand-added binaries and binaries the registry doesn't model,
// and it matches exactly what `go build` can compile. When the cmd/ tree
// can't be read (fresh scaffold mid-write, missing dir), it falls back to
// the primary alone so the Dockerfile always builds at least the server.
func discoverBinaries(projectDir, primary string) []BinaryBuild {
	fallback := []BinaryBuild{{Dir: primary, Primary: true}}
	if projectDir == "" || primary == "" {
		return fallback
	}
	entries, err := os.ReadDir(filepath.Join(projectDir, "cmd"))
	if err != nil {
		return fallback
	}
	var others []string
	sawPrimary := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := e.Name()
		// Only count a dir that actually holds a main.go — `go build
		// ./cmd/<dir>` needs a `package main` entrypoint there. Skips
		// support dirs (e.g. cmd/<bin>/cmd command-tree subpackages are
		// nested, not siblings, so they never show up here).
		if _, statErr := os.Stat(filepath.Join(projectDir, "cmd", dir, "main.go")); statErr != nil {
			continue
		}
		if dir == primary {
			sawPrimary = true
			continue
		}
		others = append(others, dir)
	}
	if !sawPrimary && len(others) == 0 {
		return fallback
	}
	sort.Strings(others)
	bins := make([]BinaryBuild, 0, len(others)+1)
	bins = append(bins, BinaryBuild{Dir: primary, Primary: true})
	for _, d := range others {
		bins = append(bins, BinaryBuild{Dir: d})
	}
	return bins
}

// ForScaffold builds the render payload for the `forge new` scaffold lane
// from a *ProjectGenerator. It reproduces, verbatim, the derivations the
// old inline anonymous struct performed (protoName via hyphen→underscore,
// servicePackage via naming.ServicePackage, the goVersion family, the
// forge/pkg dep resolution and its LocalForgePkgVendored gate).
func (g *ProjectGenerator) ForScaffold() projectTemplateData {
	goVersion := g.resolveGoVersion()

	// Sanitize name for proto files (no hyphens allowed). Use underscores
	// rather than stripping so that "my-cool-app" becomes "my_cool_app"
	// (a valid proto package identifier) instead of "mycoolapp" — which
	// silently loses the word boundaries and breaks grep.
	protoName := strings.ReplaceAll(g.Name, "-", "_")

	// ServicePackage is the Go-package-safe form of ServiceName: hyphens
	// become underscores so the value is valid in `package` declarations
	// and proto package segments. Templates that emit Go/proto identifiers
	// must use ServicePackage; ServiceName is retained for display strings.
	servicePackage := ""
	if g.ServiceName != "" {
		servicePackage = naming.ServicePackage(g.ServiceName)
	}

	data := projectTemplateData{
		Name:                   g.Name,
		ProtoName:              protoName,
		Module:                 g.ModulePath,
		ServiceName:            g.ServiceName,
		ServicePackage:         servicePackage,
		ServicePort:            g.ServicePort,
		ProjectName:            g.Name,
		FrontendName:           g.FrontendName,
		FrontendPort:           g.FrontendPort,
		GoVersion:              goVersion,
		GoVersionMinor:         goVersionMinor(goVersion),
		DockerBuilderGoVersion: dockerBuilderGoVersion(goVersion),
		ConfigFields:           codegen.DefaultConfigFieldNames(),
		// false by default — only flipped below after the forge/pkg dep is
		// resolved and dev-mode vendoring is known to run.
		LocalForgePkgVendored: false,
		// REST is off at scaffold time; users opt-in post-scaffold by
		// editing forge.yaml's `api.rest:` and re-running `forge generate`
		// (RegenerateInfraFiles re-renders buf.yaml from the live value).
		RESTEnabled: false,
		VersionVar:  g.BuildVersionVar,
		// Greenfield projects carry no legacy env-reading debt, so the
		// typed-access guardrail scaffolds in its strict, gating form. The
		// matching `config.enforce_typed_access: error` is written into the
		// scaffolded forge.yaml (project_config.go). The schema default for
		// an ABSENT key is "warn" — these two defaults are intentionally
		// different (greenfield strict; existing-project upgrade soft).
		TypedAccessGuard: guardTemplateMode(config.ConfigGuardConfig{EnforceTypedAccess: config.EnforceTypedAccessError}),
		LoaderPackage:    config.DefaultLoaderPackage,
		// At scaffold time the cmd/ tree isn't written yet, so this resolves
		// to the primary alone — exactly right for a fresh `forge new`. Once
		// the user runs `forge add binary`, the upgrade/regenerate lane
		// (ForUpgrade) re-scans cmd/ and the Dockerfile picks up every binary.
		Binaries: discoverBinaries(g.Path, g.binaryName()),
	}
	data.ForgePkgVersion, data.ForgePkgDevReplace = resolveForgePkgDep(g.Path)
	// The scaffold's forge/pkg dev replace is a host-absolute sibling path,
	// which resolves identically from any directory depth — so the gen/
	// submodule's replace is the same absolute path. Empty in release /
	// no-sibling mode, where gen/ resolves forge/pkg from the proxy like the
	// root. The first `forge generate` (run by `forge new`) reconciles this
	// to the root replace's post-vendoring form via reconcileGenForgePkgReplace.
	if data.ForgePkgVersion == "" {
		data.ForgePkgGenReplace = data.ForgePkgDevReplace
	}
	// When the scaffold emits a dev-mode forge/pkg replace AND codegen is
	// on, the `forge generate` run that `forge new` performs immediately
	// after will vendor the target into ./.forge-pkg/ — so the Dockerfile
	// (Tier 2: never auto-regenerated later) must carry the COPY line
	// from the start or docker builds diverge from host builds. Without
	// codegen there is no generate run to create the vendor dir, so the
	// COPY line would reference a missing path; keep it off.
	if data.ForgePkgDevReplace != "" && g.Features.CodegenEnabled() {
		data.LocalForgePkgVendored = true
	}

	// Strip migration-related config fields when migrations are disabled.
	// The server template conditionally includes migration code based on
	// ConfigFields["AutoMigrate"], so removing the field here prevents
	// the template from emitting app.AutoMigrate() calls.
	if !g.Features.MigrationsEnabled() {
		delete(data.ConfigFields, "AutoMigrate")
		delete(data.ConfigFields, "DatabaseUrl")
		delete(data.ConfigFields, "MaxOpenConns")
		delete(data.ConfigFields, "MaxIdleConns")
		delete(data.ConfigFields, "ConnMaxIdleTime")
		delete(data.ConfigFields, "ConnMaxLifetime")
	}

	return data
}

// ForUpgrade builds the render payload for the upgrade / Tier-1
// regeneration lane from a *config.ProjectConfig. It reproduces, verbatim,
// the derivations the old buildTemplateData performed.
//
// projectDir (when non-empty) is used to read the project's go.mod `go`
// directive so upgrade doesn't silently retarget the project to the host's
// Go version, to parse proto/config for the live ConfigFields set, and to
// detect the dev-mode .forge-pkg/ vendoring state. When projectDir is
// empty or go.mod can't be parsed, we fall back to the host's detected
// version.
func ForUpgrade(cfg *config.ProjectConfig, projectDir string) projectTemplateData {
	goVersion := goVersionFromGoMod(projectDir)
	if goVersion == "" {
		goVersion = detectGoVersion()
	}
	protoName := strings.ReplaceAll(cfg.Name, "-", "_")

	servers := cfg.Servers()
	serviceName := "api"
	servicePort := 8080
	if len(servers) > 0 {
		serviceName = servers[0].Name
		if p := servers[0].PrimaryPort(); p != 0 {
			servicePort = p
		}
	}

	frontendName := ""
	frontendPort := 3000
	if len(cfg.Frontends) > 0 {
		frontendName = cfg.Frontends[0].Name
		if cfg.Frontends[0].Port != 0 {
			frontendPort = cfg.Frontends[0].Port
		}
	}

	// Build the services list for templates like alloy-config.
	// The first server maps to docker-compose name "app".
	var services []ServiceInfo
	for i, svc := range servers {
		name := svc.Name
		if i == 0 {
			name = "app" // docker-compose service name for the primary service
		}
		port := svc.PrimaryPort()
		if port == 0 {
			port = 8080
		}
		services = append(services, ServiceInfo{Name: name, Port: port})
	}
	if len(services) == 0 {
		services = []ServiceInfo{{Name: "app", Port: 8080}}
	}

	// Parse config fields from proto/config/ so templates can conditionally
	// include code blocks that reference specific config fields.
	configFields := codegen.DefaultConfigFieldNames()
	if projectDir != "" {
		if msgs, err := codegen.ParseConfigProtosFromDir(filepath.Join(projectDir, "proto/config")); err == nil && len(msgs) > 0 {
			configFields = codegen.ConfigFieldNamesFromMessages(msgs)
		}
	}

	// Detect whether the project is in the dev-mode local-vendor state for
	// forge/pkg. The Dockerfile template gates its COPY .forge-pkg/ line on
	// this so production-published projects (no .forge-pkg/ on disk) keep
	// their canonical Dockerfile and dev-mode projects get the COPY line
	// without the user editing the file by hand.
	localForgePkgVendored := false
	if projectDir != "" {
		if _, err := os.Stat(filepath.Join(projectDir, ".forge-pkg", "go.mod")); err == nil {
			localForgePkgVendored = true
		}
	}

	authProvider, authExternal := codegen.NormalizeAuthProvider(cfg.Auth.Provider)

	return projectTemplateData{
		Name:                   cfg.Name,
		ProtoName:              protoName,
		Module:                 cfg.ModulePath,
		ServiceName:            serviceName,
		ServicePort:            servicePort,
		ProjectName:            cfg.Name,
		FrontendName:           frontendName,
		FrontendPort:           frontendPort,
		GoVersion:              goVersion,
		GoVersionMinor:         goVersionMinor(goVersion),
		DockerBuilderGoVersion: dockerBuilderGoVersion(goVersion),
		Services:               services,
		ConfigFields:           configFields,
		LocalForgePkgVendored:  localForgePkgVendored,
		RESTEnabled:            cfg.API.REST,
		AuthProvider:           authProvider,
		AuthProviderExternal:   authExternal,
		VersionVar:             cfg.Build.VersionVar,
		// Resolve the typed-access guardrail from the live forge.yaml. An
		// absent config: block resolves to "warn" (advisory) so existing
		// projects gain the guardrail without a flag-day; an explicit
		// off/error is honored.
		TypedAccessGuard: guardTemplateMode(cfg.Config),
		LoaderPackage:    cfg.Config.EffectiveLoaderPackage(),
		// Re-scan the cmd/ tree so the Dockerfile builds EVERY entrypoint
		// the project has grown (server + every `forge add binary`), each
		// into /app/<bin> in the single image.
		Binaries: discoverBinaries(projectDir, cfg.Name),
	}
}
