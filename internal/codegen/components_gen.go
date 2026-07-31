package codegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// ComponentsJSONRelPath is the project-relative path of the generated
// denormalized component data. The per-env `deploy/kcl/<env>/main.k`
// loads it (via forge.components.load_components) and lets the forge
// KCL Component schema hierarchy expand each entry into k8s resources.
// It is a lockfile-class projection of the discovered component
// [Inventory] — regenerated every run, untracked, owned 100% by forge
// (see GenerateComponentsJSON).
const ComponentsJSONRelPath = "deploy/kcl/components_gen.json"

// componentJSON is the denormalized BASE shape of one component. It
// carries ZERO Kubernetes knowledge — no Deployment/Service/CronJob
// concepts. The forge KCL `Component` schema (and its kind-selected
// subtypes Server/Worker/Cron/Operator/Binary) own ALL normalization
// into k8s resources. forge JSON and the KCL schemas are deliberately
// NOT 1:1: KCL inheritance/defaults do the expansion.
//
// `command` is the denormalized run command. It is populated only for
// binary components today — `["/app/<proj>", "<name>"]`, the cobra
// subcommand the shared image runs. Server/worker/cron run the image's
// default entrypoint, so their command is empty and KCL fills it.
//
// There is no `ports` key, and that is deliberate: a port is a DEPLOY fact.
// Nothing forge discovers (the proto descriptor, the owned worker/operator
// files, cmd/) states one, so any port forge emitted here would be invented.
// Ports are declared where the rest of the per-env deploy shape is declared —
// `deploy/kcl/<env>/main.k`, on the component overlay
// (`ports = [fc.ComponentPort {name = "http", port = 9090, expose = True}]`).
// A server that declares none gets the standard :8080 mux from
// forge.components' Server expansion.
type componentJSON struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Env      map[string]string `json:"env"`
	Command  []string          `json:"command"`
	Schedule string            `json:"schedule"`
	Group    string            `json:"group"`
	Version  string            `json:"version"`
	CRDs     []string          `json:"crds"`
	// Build is the polymorphic build declaration for this component — the
	// per-component answer to "how is this artifact produced". forge emits
	// a GoBuild here by default; the forge.components KCL Component schema
	// carries it through to the per-env main.k bridge, which passes it to
	// forge.Service.build so build.go dispatches on build.type. A project
	// (or an env overlay) may replace it with a DockerBuild / ShellBuild.
	Build componentBuildJSON `json:"build"`
}

// componentBuildJSON is the denormalized GoBuild forge emits per
// component. It carries the `type` discriminator + the go-build target
// (cmd) + the produced artifact's basename (output_name). The discriminator
// is what the KCL Build union and build.go dispatch on.
//
// The cmd contract:
//
//   - binary components are their OWN entrypoint (cmd/<binpkg>/main.go,
//     devspace idiom) → cmd = "./cmd/<binpkg>", output_name = "<binpkg>".
//   - server/worker/cron/operator components run as cobra subcommands of
//     the SHARED project binary (cmd/<project>/main.go) → cmd =
//     "./cmd/<project>", output_name = "<project>". They share one go
//     build; build.go dedups identical (cmd, output_name) targets so the
//     shared binary compiles once.
type componentBuildJSON struct {
	Type       string `json:"type"`
	Cmd        string `json:"cmd"`
	OutputName string `json:"output_name"`
}

// migrateJSON is the project-level DEPLOY-TIME migration step: the argv
// that applies this release's schema migrations, or an empty command when
// the project ships none.
//
// It is project-level, not per-component, because it is one fact about the
// release: this image knows how to migrate itself (`<binary> db migrate
// up`, over the migrations EMBEDDED in it). The env render turns a non-empty
// command into a migration initContainer on every Deployment-shaped
// workload, so the schema is current BEFORE any new pod serves a request —
// a guarantee Kubernetes enforces itself, not one that depends on which
// tool ran the apply.
//
// The command is EMPTY for a project with no db/migrations/*.sql. An empty
// command renders no init container: a migration step that runs a command
// guaranteed to fail is worse than no step at all.
type migrateJSON struct {
	Command []string `json:"command"`
}

// componentsDoc is the top-level shape of components_gen.json.
type componentsDoc struct {
	// Project is the project name. Binary components run
	// `["/app/<project>", "<name>"]`, so KCL needs the project name to
	// build that command without a second data channel.
	Project string `json:"project"`
	// Migrate is the deploy-time migration step (see migrateJSON). Always
	// emitted — an empty command is the "no migrations" answer — so the KCL
	// reader sees a stable key.
	Migrate    migrateJSON     `json:"migrate"`
	Components []componentJSON `json:"components"`
}

// MigrateCommand returns the argv that applies a project's embedded
// migrations from inside its runtime image, or nil when the project ships
// no .sql migrations.
//
// `/app/<project>` is where the generated Dockerfile's production stage
// puts the primary binary (WORKDIR /app, `COPY --from=builder
// /app/bin/<project> ./<project>`), and `db migrate up` is the subcommand
// that applies the EMBEDDED set (cmd-tree-db.go.tmpl). Both halves are
// generated from the same project name, so the argv cannot drift from the
// image it runs in.
func MigrateCommand(projectDir, projectName string) []string {
	if !ProjectHasSQLMigrations(projectDir) {
		return nil
	}
	return []string{"/app/" + projectName, "db", "migrate", "up"}
}

// ComponentsToJSON projects a discovered component [Inventory] to the
// denormalized JSON document. Deterministic: components keep inventory order
// so re-generation is idempotent.
//
// migrateCommand is the project-level deploy-time migration argv (see
// [MigrateCommand]); nil/empty means the project ships no migrations and
// the env render emits no migration step.
func ComponentsToJSON(projectName string, components Inventory, migrateCommand []string) ([]byte, error) {
	if migrateCommand == nil {
		migrateCommand = []string{}
	}
	doc := componentsDoc{
		Project:    projectName,
		Migrate:    migrateJSON{Command: migrateCommand},
		Components: []componentJSON{},
	}
	for _, c := range components {
		cj := componentJSON{
			Name:     c.Name,
			Kind:     c.EffectiveKind(),
			Env:      map[string]string{},
			Command:  []string{},
			Schedule: c.Schedule,
			Group:    c.Group,
			Version:  c.Version,
			CRDs:     []string{},
		}

		// Binary components are their OWN entrypoint in the shared image —
		// each lives at cmd/<binpkg>/main.go (devspace idiom) and the
		// Dockerfile builds it to /app/<binpkg>. So the deploy command is
		// `["/app/<binpkg>"]`, NOT a `<project> <name>` cobra subcommand of
		// the server binary (that subcommand does not exist; the binary is a
		// standalone main). <binpkg> is the Go-package-safe form of the
		// component name (hyphens → underscores), matching the cmd/<binpkg>/
		// dir the binary scaffold writes and the /app/<binpkg> the Dockerfile
		// `go build -o /app/<binpkg> ./cmd/<binpkg>` emits.
		if cj.Kind == config.ComponentKindBinary {
			cj.Command = []string{
				fmt.Sprintf("/app/%s", naming.ServicePackage(c.Name)),
			}
		}

		// Build declaration. Binary components build their own
		// cmd/<binpkg> package; everything else (server/worker/cron/
		// operator) builds the shared cmd/<project> binary and selects
		// its behavior via a cobra subcommand at runtime. forge always
		// emits a GoBuild default here; a project or env overlay can
		// replace it with a DockerBuild / ShellBuild in KCL.
		//
		// The path forms differ by kind on purpose (F5):
		//   - SECONDARY binary → cmd/<ServicePackage(name)> (hyphens →
		//     underscores): a secondary binary is named after a Go
		//     component, and `forge scaffold binary` scaffolds the sanitized dir.
		//   - PRIMARY (server/worker/cron/operator) → cmd/<project> VERBATIM
		//     (hyphens preserved): the primary binary is named after the
		//     project, and `forge project new`'s scaffold + the generated Dockerfile
		//     both write/build the raw `./cmd/<project>` path. Sanitizing the
		//     project name here (the historical default) produced a build
		//     target that pointed at cmd/<project_underscored> while the tree
		//     on disk was cmd/<project-hyphenated> — the exact mismatch that
		//     forced every hyphenated project to hand-override GoBuild.cmd in
		//     KCL. `go build ./cmd/<hyphen>` is valid (a directory path, not a
		//     package identifier), so the raw form is correct.
		if cj.Kind == config.ComponentKindBinary {
			binPkg := naming.ServicePackage(c.Name)
			cj.Build = componentBuildJSON{
				Type:       "go",
				Cmd:        "./cmd/" + binPkg,
				OutputName: binPkg,
			}
		} else {
			cj.Build = componentBuildJSON{
				Type:       "go",
				Cmd:        "./cmd/" + projectName,
				OutputName: projectName,
			}
		}

		for _, crd := range c.CRDs {
			cj.CRDs = append(cj.CRDs, crd.Name)
		}

		doc.Components = append(doc.Components, cj)
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal components_gen.json: %w", err)
	}
	out = append(out, '\n')
	return out, nil
}

// GenerateComponentsJSON writes deploy/kcl/components_gen.json from a
// discovered component [Inventory].
//
// components_gen.json is a LOCKFILE-CLASS artifact: a pure, deterministic
// projection of what the tree declares, with ZERO user-editable
// surface. forge owns 100% of it and rewrites it byte-for-byte every
// run. It is therefore NOT registered in `.forge/hashes.json` and NOT
// subject to the Tier-1 stomp guard — detecting a "hand-edit" to a
// derived projection is meaningless (the next run discards it anyway),
// and TRACKING it would reintroduce the WIP-lane bookkeeping hazard
// TestE2ESelfCertParallelLaneSubsetCommit guards against: a committed
// `.forge/hashes.json` recording a render that was never committed makes
// a clean clone of HEAD refuse to regenerate. An always-regenerated,
// untracked file sidesteps that entirely.
//
// A stale entry under the legacy tracked path is cleared so an upgrade
// from a tracked-components_gen.json build can't leave a poison hash
// behind.
func GenerateComponentsJSON(projectDir, projectName string, components Inventory, cs *checksums.FileChecksums) error {
	content, err := ComponentsToJSON(projectName, components, MigrateCommand(projectDir, projectName))
	if err != nil {
		return err
	}
	if cs != nil && cs.Unstampable != nil {
		delete(cs.Unstampable, ComponentsJSONRelPath)
	}
	dest := filepath.Join(projectDir, ComponentsJSONRelPath)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return writeUserScaffold(dest, content)
}
