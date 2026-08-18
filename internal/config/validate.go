package config

import (
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/naming"
)

// configWarningSink is where non-fatal config warnings (deprecated
// top-level keys) are written. It defaults to os.Stderr so the notice
// reaches the user on every load path (forge generate / forge project upgrade /
// any caller of LoadProject) without those callers having to
// thread a warnings slice through. The config package is otherwise
// log-free; this is a single, swappable io.Writer rather than a logger so
// tests can capture it (SetConfigWarningSink).
var configWarningSink io.Writer = os.Stderr

// emittedConfigWarnings dedupes warnings within a single process. A
// single CLI command (e.g. `forge lint`) loads forge.yaml several times
// across its sub-steps; without dedup the same deprecated-key notice
// prints once per load. Keyed on label+line+message so a genuinely
// distinct warning (different file, different key) still surfaces.
var emittedConfigWarnings = map[string]bool{}

// SetConfigWarningSink overrides the destination for non-fatal config
// warnings and returns the previous sink so callers can restore it. Used
// by tests to capture warning output; production code leaves the default
// (os.Stderr). Swapping the sink also resets the per-process dedup set so
// each test starts from a clean slate.
func SetConfigWarningSink(w io.Writer) io.Writer {
	prev := configWarningSink
	if w == nil {
		w = io.Discard
	}
	configWarningSink = w
	emittedConfigWarnings = map[string]bool{}
	return prev
}

// partitionIssues splits a flat issue list into the fatal errors (which
// gate the load via ValidationError) and the non-fatal warnings (which
// are flushed to the warning sink but never gate). Order within each
// bucket is preserved.
func partitionIssues(issues []validationIssue) (errs, warns []validationIssue) {
	for _, iss := range issues {
		if iss.warning {
			warns = append(warns, iss)
		} else {
			errs = append(errs, iss)
		}
	}
	return errs, warns
}

// flushConfigWarnings writes each warning to the warning sink in the
// standard `label:line:col: message Fix: ...` shape (same format the
// fatal ValidationError uses) so a user sees warnings and errors in a
// consistent layout. No-op when there are no warnings.
func flushConfigWarnings(label string, warns []validationIssue) {
	for _, w := range warns {
		// Dedup on the file's BASE NAME, not the label as given: a single
		// command loads the same forge.yaml through several callers, some
		// passing an absolute path and some the bare "forge.yaml", and keying
		// on the raw label made those spellings look like different files —
		// so every retired key printed once per caller.
		dedupKey := fmt.Sprintf("%s:%d:%s", filepath.Base(label), w.line, w.msg)
		if emittedConfigWarnings[dedupKey] {
			continue
		}
		emittedConfigWarnings[dedupKey] = true
		var b strings.Builder
		b.WriteString("⚠️  forge.yaml: ")
		b.WriteString(formatIssueLocation(label, w))
		b.WriteString(": ")
		b.WriteString(w.msg)
		if w.fix != "" {
			fmt.Fprintf(&b, " Fix: %s", w.fix)
		}
		_, _ = fmt.Fprintln(configWarningSink, b.String())
	}
}

// LoadProject is THE forge.yaml loader: it parses a forge.yaml byte stream
// into a ProjectConfig with strict validation, derives the project kind from
// the project's real sources, and applies the shape-derived section defaults.
// Both the CLI loader and the generator's ReadProjectConfig route through it,
// so the load rules live in exactly one place.
//
// Unknown keys (typos, dropped fields) and missing required fields are
// reported in a single error rather than silently succeeding or failing on
// the first issue.
//
// path locates the project on disk: its directory is read to derive the
// project kind, and it prefixes error messages. It is never opened for the
// config bytes themselves — the caller supplies those. A path with no
// on-disk project behind it (byte-only loads) loads as a library.
//
// Behaviour:
//
//  1. The YAML is decoded into a yaml.Node tree, then walked against
//     the ProjectConfig struct shape. Unknown keys are collected with
//     their YAML line number and parent path; a Levenshtein-based
//     suggestion is attached when a known sibling key is within edit
//     distance 2 (or 3 for keys >= 8 chars).
//  2. The same bytes are then decoded into a ProjectConfig via the
//     standard yaml decoder so that scalar-type mismatches (e.g.
//     port: "8080") surface as their own error class.
//  3. Required-field validation runs on the populated struct.
//
// All issues across the three phases are batched into a single
// ValidationError; the caller sees the full list rather than just the
// first failure.
//
// What a project CONTAINS is not part of this: components are read from the
// code that declares them (codegen.DiscoverProjectComponents), never from
// forge.yaml and never from a field on the returned config.
func LoadProject(data []byte, path string) (*ProjectConfig, error) {
	label := path
	if label == "" {
		label = "forge.yaml"
	}

	// Phase 1: walk yaml.Node to find unknown keys with position info.
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: parse error: %w", label, err)
	}
	var root *yaml.Node
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	}
	var issues []validationIssue
	if root != nil && root.Kind == yaml.MappingNode {
		issues = append(issues, walkUnknownKeys(root, "", reflect.TypeFor[ProjectConfig]())...)
	} else if root != nil && root.Kind != 0 {
		issues = append(issues, validationIssue{
			line:   root.Line,
			column: root.Column,
			msg:    "expected a YAML mapping at the top level",
			fix:    "the file must be a YAML mapping (key: value pairs), not a list or scalar.",
		})
	}

	// Phase 2: decode into the typed struct. This catches scalar-type
	// mismatches and any other yaml decoding failures. We do NOT pass
	// KnownFields(true) here because phase 1 already covered that with
	// better suggestions.
	var cfg ProjectConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		// yaml type errors look like:
		//   "yaml: line 7: cannot unmarshal !!str `8080` into int"
		// We surface them verbatim alongside any unknown-key issues.
		for _, line := range splitYAMLErrorLines(err) {
			issues = append(issues, validationIssue{msg: line})
		}
	}

	// Derive the project kind BEFORE shape-derived defaults run (feature
	// derivation reads kind). Kind comes from the project's REAL sources —
	// the KCL deploy tree, the pkg/app composition root, internal/handlers/,
	// the service protos, and cmd/<name>/main.go — read relative to the
	// forge.yaml directory. When there is no on-disk project to read
	// (byte-only loads against a synthetic path) there is nothing to read a
	// shape off, so the config is a bare module: library.
	if dir := sourceProjectDir(path); dir != "" {
		cfg.Kind = deriveProjectKindFromSources(dir)
	} else {
		cfg.Kind = ProjectKindLibrary
	}

	// Phase 3: required-field validation. The yaml root is threaded
	// through so issues can carry the line:col of the *parent* mapping
	// (or the existing-field's own line, when it's present but invalid).
	// Without this, "module_path is required" reports no location and
	// the model has to grep — model-friendly file:line:col on every
	// issue is the goal of the loader surface.
	issues = append(issues, validateRequired(&cfg, root)...)

	// Phase 4: name-shape validation over the frontends block. This
	// catches Go-package collisions and reserved-word/identifier shapes
	// that would otherwise blow up the generator with a confusing
	// downstream error.
	issues = append(issues, validateFrontendNames(&cfg, root)...)

	// Partition non-fatal warnings (deprecated top-level keys) out of the
	// gating error set. Warnings are flushed to the user unconditionally —
	// whether or not the load also has hard errors — so a deprecated key
	// is never lost to a silent rewrite even when other issues abort the
	// load.
	errIssues, warnIssues := partitionIssues(issues)
	flushConfigWarnings(label, warnIssues)
	if len(errIssues) > 0 {
		return nil, &ValidationError{Path: label, Issues: errIssues}
	}

	// Resolve shape-derived defaults: fill absent section blocks with the
	// canonical scaffold defaults for the project kind, and attach the
	// feature-derivation context so absent feature flags resolve from
	// shape (see derive.go). Explicit values are never overridden.
	ApplyDerivedDefaults(&cfg)

	// Phase 5: feature dependency graph. Now that the feature set is
	// fully resolved (derived defaults + explicit overrides folded in),
	// reject any enabled feature whose dependency is off — a config that
	// would otherwise load clean and then silently no-op or blow up
	// mid-generate. Batched into the same ValidationError so the caller
	// sees every contradiction at once (see feature_graph.go).
	if graphIssues := validateFeatureGraph(&cfg); len(graphIssues) > 0 {
		return nil, &ValidationError{Path: label, Issues: graphIssues}
	}
	return &cfg, nil
}

// ValidationError aggregates all forge.yaml validation issues into a
// single error so callers see the full picture instead of fail-fast on
// the first problem. Implements error.
type ValidationError struct {
	Path   string
	Issues []validationIssue
}

func (e *ValidationError) Error() string {
	if len(e.Issues) == 1 {
		var b strings.Builder
		b.WriteString(formatIssueLocation(e.Path, e.Issues[0]))
		b.WriteString(": ")
		b.WriteString(e.Issues[0].msg)
		if e.Issues[0].fix != "" {
			fmt.Fprintf(&b, " Fix: %s", e.Issues[0].fix)
		}
		return b.String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d validation issue", e.Path, len(e.Issues))
	if len(e.Issues) != 1 {
		b.WriteString("s")
	}
	b.WriteString(":\n")
	for _, iss := range e.Issues {
		b.WriteString("  ")
		b.WriteString(formatIssueLocation(e.Path, iss))
		b.WriteString(": ")
		b.WriteString(iss.msg)
		if iss.fix != "" {
			fmt.Fprintf(&b, " Fix: %s", iss.fix)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// formatIssueLocation renders the per-issue position in standard
// compiler/editor format: `path:line:col` when both line and column are
// known, `path:line` for line-only, `path` when neither. Matches what
// every editor, LSP client, and `cc`/`go vet`-style tool already
// understands — a model reading the error can immediately open the
// right line, no grep round-trip required.
func formatIssueLocation(path string, iss validationIssue) string {
	switch {
	case iss.line > 0 && iss.column > 0:
		return fmt.Sprintf("%s:%d:%d", path, iss.line, iss.column)
	case iss.line > 0:
		return fmt.Sprintf("%s:%d", path, iss.line)
	default:
		return path
	}
}

type validationIssue struct {
	line   int    // YAML line number (1-based); 0 if unknown.
	column int    // YAML column (1-based); 0 if unknown.
	msg    string // primary message ("unknown key 'auht' — did you mean 'auth'?")
	fix    string // "Fix: rename to 'auth' or remove if unused."
	// warning marks a non-fatal notice. The zero value (false) is an
	// error: it gates the load via ValidationError. Warnings are
	// partitioned out in loadStrict — they never gate, but they are
	// surfaced to the user (see flushConfigWarnings) so silently-dropped
	// config doesn't vanish without a trace.
	warning bool
}

// removedSchemaKeys maps a normalized key path of a forge.yaml key that
// was deliberately removed from the schema (as opposed to a typo) to the
// one-line "what to do instead" guidance emitted as the issue's Fix:
// hint. When validation hits one of these, that hint replaces the generic
// "unknown key — did you mean ...?", which would otherwise mislead an
// agent into renaming the key rather than migrating it.
//
// A hint is written for someone who has to edit this file right now, so
// it is imperative, self-contained, and names the skill documenting the
// migration where one exists. It states the CURRENT model — never why an
// old key stopped working. Which internal rework retired a key, and
// whether it was load-bearing before it went, are facts about forge's
// own history: they belong in the per-entry comments below (and in the
// audit trail), not in a message a user reads.
//
// Path normalization: slice indices are collapsed to "[]" (e.g.
// "services[3].dev_target" matches the "services[].dev_target" entry),
// so one entry covers every element of a list. Top-level keys use the
// bare key name. Keys nested under a user-defined map segment (e.g. a
// component's `ports.<name>`) cannot be matched here — a removed key
// must be a fixed schema path, not one below a user-chosen map key.
//
// Audit trail (git history of config.go):
//   - k8s.provider: removed in 01bd491 ("remove dead BinaryConfig.Kind
//     and K8sConfig.Provider fields"). Never load-bearing; per-env
//     cluster choice lives in KCL `forge.K8sCluster` blocks.
//   - binaries[].kind: removed in the same commit. The cron/oneshot
//     kinds were reserved-but-unimplemented; every binary is
//     long-running today.
//   - services[].dev_target: added in cd25640, reverted in 16921aa.
//     Host/cluster placement moved to the per-env `deploy:` field on
//     the KCL `forge.Service` schema.
//   - environments (top level): removed in the KCL-canonical cleanup
//     (8d3e185) — handled separately by deprecatedTopLevelKeys below
//     because mid-migration projects must still LOAD; it is reported as
//     a non-fatal WARNING (not silently skipped) so the user migrates it
//     before the next forge.yaml rewrite drops it.
var removedSchemaKeys = map[string]string{
	// The auth VALIDATOR is no longer config — it is owned code. Picking a
	// validator (JWT/Clerk/Auth0/custom) is a code-wiring choice; the
	// per-DEPLOYMENT values it reads are typed config fields like every
	// other per-environment value. Each service scaffolds an editable
	// internal/app/auth.go whose SetupAuth() the generated cmd serve calls.
	"auth.provider": "delete the key — the validator is picked in code now. Edit " +
		"internal/app/auth.go's SetupAuth() (default: JWT); pin jwt_issuer / " +
		"jwt_audience / jwt_jwks_url in deploy/kcl/<env>/config.k and jwt_secret " +
		"in the env's secret provider.",
	"auth.jwt": "delete the block — jwt_issuer / jwt_audience / jwt_jwks_url are " +
		"typed config fields (proto/config/v1/config.proto), pinned per env in " +
		"deploy/kcl/<env>/config.k and read by internal/app/auth.go's SetupAuth().",
	"auth.api_key": "delete the block — auth is owned code now. Build an API-key " +
		"validator in internal/app/auth.go's SetupAuth() if you need one.",
	// The whole `auth:` block went with its last field. It survived the
	// auth-mechanism-to-code move as an empty struct, so a bare `auth:`
	// parsed clean and configured nothing.
	"auth": "delete the block — authentication is owned code. Edit " +
		"internal/app/auth.go's SetupAuth() to pick the validator; pin " +
		"jwt_issuer / jwt_audience / jwt_jwks_url in deploy/kcl/<env>/config.k " +
		"and jwt_secret in the env's secret provider.",
	// version: never read by anything. The binary's version is stamped at
	// link time from a KCL GoBuild.ldflags `-X` entry, which is per-env and
	// therefore cannot live in a project-global file.
	"version": "delete the key — stamp the binary version from a KCL " +
		"GoBuild.ldflags `-X` entry in deploy/kcl/<env>/main.k, which is where " +
		"per-environment build facts live.",
	// hot_reload lived at the top level AND under features:. Only the
	// features one was ever resolved by a caller.
	"hot_reload": "move the value to `features.hot_reload`, which is the live switch.",
	// ci.go_version was written into the workflow template data and read by
	// no template: every setup-go step pins with `go-version-file: go.mod`,
	// so the key changed nothing and a project that set it got a CI Go
	// version it had not asked for, with no warning.
	"ci.go_version": "delete the key — CI reads the toolchain from go.mod " +
		"(`go-version-file: go.mod` in every setup-go step), so set the version " +
		"in go.mod and CI follows.",
	// ci.extra_jobs was a declared "user extension point" that the workflow
	// generator never read: the template ranged over a struct field nothing
	// populated, so every declared job silently vanished.
	"ci.extra_jobs": "delete the block and add the job directly to .github/workflows/ci.yml — " +
		"that file is scaffold-once and yours to edit; forge never re-renders it.",
	// lint.contract had no reader. Whether the contract lint runs is
	// features.contracts.
	"lint.contract": "delete the key — set `features.contracts: false` to turn the contract " +
		"lint off, or list the package under `contracts.exclude` to exempt just that package.",
	// The contract severity dials had no reader either: the contract rules
	// are unconditional and the only real escape hatch is contracts.exclude.
	"contracts.strict": "delete the key — the contract rules are unconditional. Turn the whole " +
		"lint off with `features.contracts: false`, or exempt one package via `contracts.exclude`.",
	"contracts.allow_exported_vars": "delete the key — exempt the package via `contracts.exclude`, or opt it out " +
		"in code with a `// forge:exclude-contract` package-doc directive.",
	"contracts.allow_exported_funcs": "delete the key — exempt the package via `contracts.exclude`, or opt it out " +
		"in code with a `// forge:exclude-contract` package-doc directive.",
	// The pack subsystem was RETIRED wholesale. What packs used to install is
	// now split across owned scaffold + libraries: frontend components
	// (the auth UI) are owned scaffold you edit directly, and auth /
	// audit are code plus the forge/pkg/auth, forge/pkg/apikey, forge/pkg/audit
	// libraries. There is nothing to re-install and nothing to migrate — the
	// key simply drops. `features.packs` (the feature that gated the subsystem)
	// went with it.
	"packs": "delete the key — packs are no longer supported. Frontend components are owned " +
		"scaffold (`forge skill load frontend`); auth and audit are code + libraries " +
		"(`forge skill load auth`).",
	"pack_overrides": "delete the block — there are no packs to override. " +
		"Frontend components are owned scaffold and auth/audit are code + libraries (`forge skill load auth`).",
	"features.packs": "delete the key — the `packs` feature no longer exists. " +
		"Frontend components are owned scaffold; auth/audit are code + libraries (`forge skill load auth`).",
	"k8s.provider": "remove the key — per-environment cluster choice now lives in KCL " +
		"`forge.K8sCluster` blocks under deploy/kcl/.",
	// deploy.provider was never read: the CI provider lives in `ci.provider`
	// (generate_ci.go reads cfg.CI.Provider). Removed in the forge.yaml
	// schema cleanup (FORGE_SHAPE_REDESIGN §4 — deploy is pipeline-control
	// only; provider belongs to ci).
	"deploy.provider": "delete the key — the CI provider is set via `ci.provider` (github is the default).",
	// kind: never lived in forge.yaml. Project kind DERIVES from the
	// project's real sources — the KCL deploy tree, the service registry, the
	// service handlers, and cmd/ binaries. There is no manifest.
	"kind": "delete the key — project kind derives from the real sources: the KCL deploy " +
		"tree (deploy/kcl/), the service registry (pkg/app/services.go), the service handlers " +
		"(internal/handlers/), or a cmd/ binary.",
	// components/services/binaries: forge.yaml is GLOBAL-only. Per-service
	// components are DISCOVERED from real sources (proto descriptor for
	// services, owned code for workers/operators/binaries), never a manifest.
	"components": "delete the key — components are discovered from the real sources (proto " +
		"services, internal/handlers/, cmd/ binaries), not authored in forge.yaml or a manifest.",
	"services": "delete the key — services are discovered from the proto descriptor + " +
		"internal/handlers/; add one with `forge scaffold service <name>`.",
	// packages: was never a codegen input — the bootstrap/injector pass has
	// always walked internal/*/contract.go, so a project whose block was
	// deleted still got every package wired. It steered only the reporters
	// (`forge project map`, `forge project audit`, the architecture doc),
	// which made a stale entry name a package that does not exist while
	// hiding one that does.
	"packages": "delete the key — an internal package is declared by internal/<name>/contract.go, " +
		"and its outbound-boundary claim by the `//forge:outbound-io` marker in its own source; " +
		"add one with `forge scaffold package <name> [--type adapter]`.",
	"binaries": "delete the key — binaries are discovered from their cmd/<name>/main.go; add one with `forge scaffold binary <name>`.",
	// test: backed the orphaned `forge test --env=<env>` port-forward flow,
	// superseded by the `forge env up <env> && go test` two-command loop.
	// The per-env recipe (forwards + env + command) was removed; drive e2e
	// suites with a plain `go test` after `forge env up`.
	"test": "delete the key — bring the env up with `forge env up <env>` and run the " +
		"suite with a plain `go test` (port-forward and env-vars are no longer declared in forge.yaml).",
	"components[].type": "delete `type:` and set `kind:` instead (go_service → server).",
	"binaries[].kind": "remove the key — every `forge scaffold binary` entry is long-running; " +
		"there are no binary kinds.",
	"services[].dev_target": "move host/cluster placement to the per-env `deploy:` field on the KCL " +
		"`forge.Service` schema (`forge.HostDeploy | forge.K8sCluster | forge.External | forge.Compose | forge.BuildOnly`).",
	// serve/served_by shipped only on an unreleased branch (never adopted
	// downstream) before being replaced by registration-in-code: what a
	// binary serves is the row list in pkg/app/services.go, not a yaml
	// knob.
	"components[].serve": "delete the key — what a binary serves is code: to stop serving a " +
		"service from this binary, delete its serviceRow line in pkg/app/services.go and leave a " +
		"comment naming the binary that serves it; see the `services` skill (Types-Only Services).",
	"components[].served_by": "delete the key — document the serving binary as a comment next to the " +
		"deleted serviceRow line in pkg/app/services.go; see the `services` skill " +
		"(Types-Only Services).",
	// stack.{backend,database,proto,deploy,ci} were "forward-looking
	// declarations" that no codegen path ever read — they DUPLICATED the
	// canonical sources. Removed in the forge.yaml schema cleanup
	// (FORGE_SHAPE_REDESIGN §4). Only `stack.frontend` survives. Each old
	// sub-block points the user at the real source of truth.
	"stack.backend": "delete the key — backend language/framework is not a codegen input; " +
		"forge projects are Go + Connect RPC.",
	"stack.database": "delete the key and set the driver under `database.driver` (postgres | none).",
	"stack.proto":    "delete the key — the proto toolchain is buf; there is no per-project toggle.",
	"stack.deploy": "delete the key — the image registry lives in `docker.registry`, and the " +
		"deploy target/cluster is declared per-env in `deploy/kcl/<env>/main.k` (forge.K8sCluster).",
	"stack.ci": "delete the key and set the CI provider under `ci.provider` (github is the default).",
	// deploy graduated from experimental to a stable kind-derived flag in
	// the front-door rework; projects scaffolded in the experimental
	// window still carry the old nesting.
	"features.experimental.deploy": "move the value to `features.deploy` — or delete it entirely if it matches " +
		"the derived default (true for kind: service).",
}

// sliceIndexRe matches "[<digits>]" path segments so removed-key lookup
// can collapse "services[3]" to "services[]".
var sliceIndexRe = regexp.MustCompile(`\[\d+\]`)

// normalizeKeyPath collapses slice indices in a dotted key path so it
// can be looked up in removedSchemaKeys.
func normalizeKeyPath(p string) string {
	return sliceIndexRe.ReplaceAllString(p, "[]")
}

// deprecatedTopLevelKeys maps a top-level forge.yaml key that was once
// part of the schema (but has since been removed) to the migration
// guidance shown when it is encountered. These keys are NOT errors: a
// project mid-migration must still LOAD. But they are also NOT silently
// dropped — NormalizeForWrite re-serializes forge.yaml without them, so
// the next rewrite would lose the user's real config (e.g. per-env log
// levels under `environments:`) with zero trace. We emit a warning so
// the loss is visible and the user is pointed at the migration skill.
//
// Currently:
//   - `environments`: removed in the deploy-target-architecture
//     migration. Per-env deploy info (cluster/namespace/registry/
//     domain) now lives in KCL `forge.K8sCluster` blocks; per-env
//     app config lives in sibling `config.<env>.yaml` files.
var deprecatedTopLevelKeys = map[string]string{
	"environments": "this key is no longer part of the forge.yaml schema and will be DROPPED on the next " +
		"forge.yaml rewrite (forge generate / forge project upgrade re-serialize the file). Migrate per-env config " +
		"before you lose it: per-env deploy info moves to KCL `forge.K8sCluster` blocks and per-env app config " +
		"moves to sibling `config.<env>.yaml` files next to forge.yaml.",
}

// walkUnknownKeys recursively descends a yaml.Node mapping against the
// reflected Go type. Unknown keys produce issues with line numbers and
// suggestions; known keys recurse if they map to nested struct or slice
// types.
func walkUnknownKeys(node *yaml.Node, path string, t reflect.Type) []validationIssue {
	var out []validationIssue
	if t == nil {
		return nil
	}
	// Unwrap pointer.
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// We only descend into struct mappings here. Map[string]X with a
	// declared key type just accepts anything (user-chosen keys), so
	// no unknown-key warning at that layer.
	if t.Kind() != reflect.Struct {
		return nil
	}
	known := yamlKeysOf(t)
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		valNode := node.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		key := keyNode.Value
		// Deprecated keys at the top level do NOT fail validation
		// (projects mid-migration must still load), but they are NOT
		// silently dropped either: the next forge.yaml rewrite would
		// lose the config without a trace. Emit a non-gating warning
		// that names the key and points at the migration skill.
		if path == "" {
			if hint, ok := deprecatedTopLevelKeys[key]; ok {
				out = append(out, validationIssue{
					line:    keyNode.Line,
					column:  keyNode.Column,
					msg:     fmt.Sprintf("%q is a deprecated top-level key", key),
					fix:     hint,
					warning: true,
				})
				continue
			}
		}
		field, ok := known[key]
		if !ok {
			full := qualifiedKey(path, key)
			// Removed keys come FIRST: a key that used to be in the
			// schema gets its specific migration message, never a
			// Levenshtein "did you mean" (which would suggest renaming
			// instead of migrating — the exact trap an agent reading
			// the error would fall into).
			//
			// A removed key is one forge ITSELF wrote a version ago, so it
			// is a WARNING, not a hard fail (fr-57edf33aca): a forge.yaml
			// forge authored must keep loading across a schema removal —
			// every config-loading command hard-failing on forge's own
			// retired key strands the project until a human edits the file.
			// The warning still carries the migration hint, and the next
			// forge.yaml rewrite (NormalizeForWrite) drops the dead key, so
			// the deprecation is visible and self-healing. Genuine typos
			// (NOT in removedSchemaKeys) stay fatal below — that distinction
			// is the whole point of the map.
			if fix, removed := removedSchemaKeys[normalizeKeyPath(full)]; removed {
				out = append(out, validationIssue{
					line:    keyNode.Line,
					column:  keyNode.Column,
					msg:     fmt.Sprintf("%q is no longer a forge.yaml key", full),
					fix:     fix,
					warning: true,
				})
				continue
			}
			msg := fmt.Sprintf("unknown key %q", full)
			fix := "rename or remove this key."
			if suggestion := closestMatch(key, knownNames(known)); suggestion != "" {
				msg += fmt.Sprintf(" — did you mean %q?", suggestion)
				fix = fmt.Sprintf("rename to %q or remove if unused.", suggestion)
			}
			out = append(out, validationIssue{line: keyNode.Line, column: keyNode.Column, msg: msg, fix: fix})
			continue
		}
		// Recurse into nested structs and slices of structs.
		ft := field.Type
		switch ft.Kind() {
		case reflect.Struct:
			if valNode.Kind == yaml.MappingNode {
				childPath := joinPath(path, key)
				out = append(out, walkUnknownKeys(valNode, childPath, ft)...)
			}
		case reflect.Slice:
			elem := ft.Elem()
			if elem.Kind() == reflect.Struct && valNode.Kind == yaml.SequenceNode {
				for idx, item := range valNode.Content {
					if item.Kind == yaml.MappingNode {
						childPath := fmt.Sprintf("%s[%d]", joinPath(path, key), idx)
						out = append(out, walkUnknownKeys(item, childPath, elem)...)
					}
				}
			}
		case reflect.Pointer:
			if ft.Elem().Kind() == reflect.Struct && valNode.Kind == yaml.MappingNode {
				childPath := joinPath(path, key)
				out = append(out, walkUnknownKeys(valNode, childPath, ft.Elem())...)
			}
		case reflect.Map:
			// Map[string]Struct: descend into each entry's value, where
			// the key is user-defined (e.g. a component's port names) so
			// we can't validate the key itself.
			if ft.Elem().Kind() == reflect.Struct && valNode.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(valNode.Content); j += 2 {
					entryKey := valNode.Content[j]
					entryVal := valNode.Content[j+1]
					if entryVal.Kind == yaml.MappingNode {
						childPath := fmt.Sprintf("%s.%s", joinPath(path, key), entryKey.Value)
						out = append(out, walkUnknownKeys(entryVal, childPath, ft.Elem())...)
					}
				}
			}
		}
	}
	return out
}

// yamlKeysOf returns a map from yaml-tag-name -> reflect.StructField for
// every field declared on t. Embedded structs are flattened so their
// keys appear at the parent level (yaml.v3 default behaviour).
func yamlKeysOf(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField)
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("yaml")
		if tag == "" || tag == "-" {
			if f.Anonymous {
				maps.Copy(out, yamlKeysOf(f.Type))
			}
			continue
		}
		name := strings.SplitN(tag, ",", 2)[0]
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		out[name] = f
	}
	return out
}

func knownNames(m map[string]reflect.StructField) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func joinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

func qualifiedKey(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}

// closestMatch returns the closest entry in candidates to needle by
// Levenshtein distance, or "" if no candidate is close enough. Threshold
// scales with needle length: short keys (< 8 chars) require <= 2,
// longer keys allow <= 3.
func closestMatch(needle string, candidates []string) string {
	if needle == "" || len(candidates) == 0 {
		return ""
	}
	threshold := 2
	if len(needle) >= 8 {
		threshold = 3
	}
	best := ""
	bestDist := threshold + 1
	for _, c := range candidates {
		d := levenshtein(strings.ToLower(needle), strings.ToLower(c))
		if d < bestDist {
			bestDist = d
			best = c
		}
	}
	if bestDist <= threshold {
		return best
	}
	return ""
}

// levenshtein returns the edit distance (insert/delete/substitute, all
// cost 1) between a and b. Implementation uses a single-row DP buffer
// for O(min(len)) memory.
func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, min(prev[j]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

// splitYAMLErrorLines turns a yaml decoding error into one issue per
// underlying problem. yaml.v3's TypeError aggregates issues with newlines
// in its message, while plain errors have a single line.
func splitYAMLErrorLines(err error) []string {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// yaml.v3 prefixes TypeError messages with "yaml: unmarshal errors:\n  ".
	msg = strings.TrimPrefix(msg, "yaml: unmarshal errors:\n")
	parts := strings.Split(msg, "\n")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		// Skip the "field X not found" lines — phase 1 covered those
		// with better suggestions.
		if p == "" || strings.Contains(p, " not found in type ") {
			continue
		}
		// Trim the leading "yaml: " prefix when present so the message
		// reads cleanly under our path-prefixed format.
		p = strings.TrimPrefix(p, "yaml: ")
		out = append(out, p)
	}
	return out
}

// validateRequired checks that fields the project cannot meaningfully
// be missing are present. The list intentionally stays small — every
// required field here corresponds to a real downstream breakage when
// absent (broken go.mod, empty deploy, ambiguous codegen target).
func validateRequired(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue
	out = append(out, validateProjectFields(cfg, root)...)
	out = append(out, validateFrontends(cfg, root)...)
	out = append(out, validateORMDriver(cfg, root)...)
	out = append(out, validateConfigGuard(cfg, root)...)
	return out
}

// validateConfigGuard checks the `config:` section's enumerated
// enforce_typed_access value. Empty is valid (resolves to "warn"); any
// non-empty value outside {off, warn, error} (case/alias-normalized) is a
// fatal, clearly-explained error rather than a silently-ignored typo.
func validateConfigGuard(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	if raw := strings.TrimSpace(cfg.Config.EnforceTypedAccess); raw != "" {
		switch strings.ToLower(raw) {
		case EnforceTypedAccessOff, EnforceTypedAccessWarn, EnforceTypedAccessError, "warning":
			// valid
		default:
			line, col := findNodePos(root, []string{"config", "enforce_typed_access"})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("config.enforce_typed_access value %q is invalid", cfg.Config.EnforceTypedAccess),
				fix:    "use one of: off, warn, error (absent defaults to warn).",
			})
		}
	}

	if raw := strings.TrimSpace(cfg.Config.EnforceComponentObserve); raw != "" {
		switch strings.ToLower(raw) {
		case EnforceComponentObserveOff, EnforceComponentObserveError:
			// valid
		default:
			line, col := findNodePos(root, []string{"config", "enforce_component_observe"})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("config.enforce_component_observe value %q is invalid", cfg.Config.EnforceComponentObserve),
				fix:    "use one of: error, off (absent defaults to error).",
			})
		}
	}

	return out
}

// validateProjectFields checks the top-level project identity fields
// (name, module_path) that the project cannot meaningfully be missing.
func validateProjectFields(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	// rootPos is the fallback location for "this required field is
	// missing entirely from the file" — we point at the top-level
	// mapping (line 1, col 1) so the model knows it's a forge.yaml-wide
	// concern, not a nested-block one.
	var rootLine, rootCol int
	if root != nil {
		rootLine, rootCol = root.Line, root.Column
	}

	if strings.TrimSpace(cfg.Name) == "" {
		out = append(out, validationIssue{
			line:   rootLine,
			column: rootCol,
			msg:    "'name' is required but missing or empty",
			fix:    "add 'name: <project-name>' near the top of forge.yaml.",
		})
	}
	if strings.TrimSpace(cfg.ModulePath) == "" {
		out = append(out, validationIssue{
			line:   rootLine,
			column: rootCol,
			msg:    "'module_path' is required but missing or empty",
			fix:    "add 'module_path: github.com/<org>/<project>' near the top of forge.yaml.",
		})
	} else if !looksLikeGoModulePath(cfg.ModulePath) {
		// Existing-but-invalid: point at the actual `module_path:` line.
		line, col := findNodePos(root, []string{"module_path"})
		out = append(out, validationIssue{
			line:   line,
			column: col,
			msg:    fmt.Sprintf("'module_path' value %q does not look like a Go module path", cfg.ModulePath),
			fix:    "use a path like 'github.com/<org>/<project>' (must contain a slash, no spaces).",
		})
	}
	// kind is not a forge.yaml field — it is DERIVED from the project's real
	// sources (deriveProjectKindFromSources) before validateRequired runs, so
	// it is always one of the valid values and needs no validation here.

	return out
}

// validateFrontends checks per-frontend required fields and the
// enumerated values (name, type, output, base_path).
func validateFrontends(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	for i, fe := range cfg.Frontends {
		prefix := fmt.Sprintf("frontends[%d]", i)
		if strings.TrimSpace(fe.Name) == "" {
			line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i)})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name is required", prefix),
				fix:    "add a 'name:' for this frontend entry.",
			})
		}
		// frontends[].type and frontends[].path are filled in by the
		// loader when omitted (type → "nextjs", path → "frontends/<name>"),
		// so we only validate non-empty values here. Required-ness would
		// be a regression for existing forge.yaml files.
		if t := strings.ToLower(strings.TrimSpace(fe.Type)); t != "" {
			if t != "nextjs" && t != "react_native" && t != "react-native" && t != "vite-spa" {
				line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "type"})
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.type value %q is invalid", prefix, fe.Type),
					fix:    "use one of: nextjs, react-native, vite-spa.",
				})
			}
		}
		// frontends[].source declares the code as a pinned cross-repo
		// dependency. It is an ALTERNATIVE to `path`, never a companion:
		// with both set there are two answers to "where is this
		// frontend's code", and silently preferring one would make the
		// other a lie that reads as truth in review. Reject instead.
		if fe.Source != nil {
			line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "source"})
			if fe.Path != "" {
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s declares both 'path' and 'source'", prefix),
					fix:    "keep one: 'path' for a directory in this repo, 'source' for a pinned checkout of another repo. To build a local working copy of a `source` frontend, add an override in .forge/source-overrides.yaml instead of re-adding 'path'.",
				})
			}
			if strings.TrimSpace(fe.Source.Repo) == "" {
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.source.repo is required", prefix),
					fix:    "add 'repo:' — e.g. github.com/org/app.",
				})
			}
			if strings.TrimSpace(fe.Source.Ref) == "" {
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.source.ref is required", prefix),
					fix:    "add 'ref:' — a tag, branch, or commit sha. forge does not default to a branch: an unpinned cross-repo source is what makes a build unreproducible.",
				})
			}
		}
		// frontends[].output selects the Next.js build/runtime shape.
		// Only meaningful for type=nextjs; we still validate the value
		// for other types because changing the type later shouldn't
		// silently re-validate against a stale value. Defaults to
		// "standalone" when empty.
		if o := strings.ToLower(strings.TrimSpace(fe.Output)); o != "" {
			if o != "static" && o != "standalone" && o != "server" {
				line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "output"})
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.output value %q is invalid", prefix, fe.Output),
					fix:    "use one of: standalone (default), static, server.",
				})
			}
		}
		// frontends[].base_path mounts the frontend under a URL prefix.
		// The shape is deliberately strict (see FrontendConfig.BasePath):
		// the literal is rendered verbatim into next.config.ts and the
		// generated basepath_gen.ts helper, so a malformed value here
		// becomes a silently-broken deploy there. As with `output`, we
		// validate regardless of frontend type so a later type change
		// can't resurrect a stale invalid value.
		if bp := strings.TrimSpace(fe.BasePath); bp != "" {
			if msg, ok := ValidateBasePath(bp); !ok {
				line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "base_path"})
				out = append(out, validationIssue{
					line:   line,
					column: col,
					msg:    fmt.Sprintf("%s.base_path value %q is invalid: %s", prefix, fe.BasePath, msg),
					fix:    `use a "/"-prefixed path with no trailing slash, e.g. "/admin" (omit the field entirely for root mounting).`,
				})
			}
		}
		// frontends[].auth_mode picks where the user types their password.
		// "redirect" is the only mode forge scaffolds: driving a hosted
		// sign-in from a first-party form is provider-proprietary, so
		// there is nothing generic to generate. A rejected value here is
		// better than a silently-ignored one.
		if am := strings.ToLower(strings.TrimSpace(fe.AuthMode)); am != "" && am != AuthModeRedirect {
			line, col := findNodePos(root, []string{"frontends", fmt.Sprintf("[%d]", i), "auth_mode"})
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.auth_mode value %q is invalid", prefix, fe.AuthMode),
				fix:    "use redirect (the default, and the only mode forge scaffolds). A first-party sign-in form is yours to build against your IdP's own API — see `forge skill load auth/frontend`.",
			})
		}
	}

	return out
}

// validateORMDriver requires database.driver when the ORM feature has
// been explicitly enabled.
func validateORMDriver(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	// rootPos is the fallback when the `database:` block is absent.
	var rootLine, rootCol int
	if root != nil {
		rootLine, rootCol = root.Line, root.Column
	}

	// Only require database.driver when ORM has been *explicitly* enabled.
	// Features.ORM defaults to nil → ORMEnabled() reports true, but a nil
	// value means "user didn't make a choice"; many legacy projects work
	// without a driver because they aren't actually exercising the ORM
	// codegen at runtime. Demanding a driver in that case would be a
	// breaking change. Explicit `features.orm: true` is the signal that
	// the user is committing to the ORM and so must declare a driver.
	if cfg.Features.ORM != nil && *cfg.Features.ORM && strings.TrimSpace(cfg.Database.Driver) == "" {
		// Point at the `database:` block (or the file root if absent).
		line, col := findNodePos(root, []string{"database"})
		if line == 0 {
			line, col = rootLine, rootCol
		}
		out = append(out, validationIssue{
			line:   line,
			column: col,
			msg:    "'database.driver' is required when features.orm is explicitly enabled",
			fix:    "add 'database:\\n  driver: postgres'.",
		})
	}

	return out
}

// basePathSegmentRE matches one path segment of frontends[].base_path:
// letters, digits, dot, underscore, hyphen. Deliberately narrower than
// what URLs technically allow — the value is spliced verbatim into
// next.config.ts (basePath / assetPrefix) and into generated TypeScript
// string literals, so "no fancy chars" is the safety contract.
var basePathSegmentRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateBasePath checks the shape of a non-empty frontends[].base_path
// value. Returns (reason, false) on failure, ("", true) when valid.
//
// Valid:   "/admin", "/internal/admin", "/v2.1_beta"
// Invalid: "admin" (no leading slash), "/admin/" (trailing slash),
//
//	"/" (root mount — omit the field instead), "/ad min", "/a%2Fb".
func ValidateBasePath(bp string) (string, bool) {
	if !strings.HasPrefix(bp, "/") {
		return `must start with "/"`, false
	}
	if bp == "/" {
		return `bare "/" means root mounting — omit base_path instead`, false
	}
	if strings.HasSuffix(bp, "/") {
		return `must not end with "/"`, false
	}
	for _, seg := range strings.Split(bp[1:], "/") {
		if seg == "" {
			return "must not contain empty segments (\"//\")", false
		}
		if !basePathSegmentRE.MatchString(seg) {
			return fmt.Sprintf("segment %q contains characters outside [A-Za-z0-9._-]", seg), false
		}
	}
	return "", true
}

// findNodePos walks a YAML mapping/sequence tree along a dot/index path
// and returns the line/col of the resolved node. Path segments are
// either bare keys (e.g. "module_path") or sequence indices in literal
// `[N]` form (e.g. "[0]") — same shape used in qualifiedKey output so
// callers can construct paths once and reuse them across issue messages
// and position lookups. Returns (0, 0) when the path doesn't resolve;
// callers fall back to the root position (or omit position entirely)
// in that case.
func findNodePos(node *yaml.Node, segments []string) (int, int) {
	if node == nil {
		return 0, 0
	}
	cur := node
	for _, seg := range segments {
		if cur == nil {
			return 0, 0
		}
		if strings.HasPrefix(seg, "[") && strings.HasSuffix(seg, "]") {
			// Sequence index.
			if cur.Kind != yaml.SequenceNode {
				return 0, 0
			}
			idx := 0
			if _, err := fmt.Sscanf(seg, "[%d]", &idx); err != nil {
				return 0, 0
			}
			if idx < 0 || idx >= len(cur.Content) {
				return 0, 0
			}
			cur = cur.Content[idx]
			continue
		}
		// Mapping key lookup.
		if cur.Kind != yaml.MappingNode {
			return 0, 0
		}
		var matched *yaml.Node
		for i := 0; i+1 < len(cur.Content); i += 2 {
			if cur.Content[i].Kind == yaml.ScalarNode && cur.Content[i].Value == seg {
				matched = cur.Content[i+1]
				break
			}
		}
		if matched == nil {
			return 0, 0
		}
		cur = matched
	}
	if cur == nil {
		return 0, 0
	}
	return cur.Line, cur.Column
}

// goReservedWords is the set of Go keywords plus predeclared identifiers
// that cannot be used as package names without breaking the build.
// We use this to flag service / binary / frontend names whose canonical
// Go-package form (naming.ServicePackage) lands on one of them — e.g.
// a service named "select" or "type" would compile-fail downstream.
var goReservedWords = map[string]bool{
	// Keywords.
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	// Predeclared identifiers that would shadow basic types and break
	// `package <name>` in the generated tree.
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "any": true, "true": true, "false": true, "nil": true,
	"iota": true, "init": true,
}

// validateFrontendNames rejects frontend name shapes that would silently
// break codegen downstream:
//
//   - empty name (or name that normalises to empty)
//   - non-Go-legal package shape after normalisation (starts with a
//     digit, contains punctuation/space that survives `ServicePackage`)
//   - normalisation collisions (e.g. `admin-server` and `admin_server`
//     both → `admin_server` since hyphens normalise to underscores)
//   - the canonical form lands on a Go reserved word / predeclared
//     identifier (e.g. "select", "type"), which would compile-fail
//
// The lint is name-shape-only — it does not look at config semantics.
// Returning the issues batched lets ValidationError surface every
// problem in one go. Component names go through the same rules at
// `forge scaffold` time, where the name is actually chosen.
func validateFrontendNames(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	var out []validationIssue

	// Track canonical -> first-seen-source so collisions can name both
	// the earlier and the later entry in the error message.
	seen := map[string]string{}

	check := func(rawName, source string, pathSegs []string) {
		trimmed := strings.TrimSpace(rawName)
		if trimmed == "" {
			// Empty-name issues are already reported by validateRequired
			// for the slices that have a required-name rule. Don't double
			// up; just skip the canonical check.
			return
		}
		// Resolve position once for whichever issue fires. Falls back to
		// (0,0) if the path doesn't resolve — formatIssueLocation handles
		// that by omitting the position part of the error.
		line, col := findNodePos(root, pathSegs)
		canonical := naming.ServicePackage(trimmed)
		if canonical == "" {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q normalises to an empty Go package", source, rawName),
				fix:    "use at least one ASCII letter or digit in the name.",
			})
			return
		}
		if !isValidGoPackageIdent(canonical) {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q produces invalid Go package %q", source, rawName, canonical),
				fix:    "use ASCII letters, digits, hyphens, and underscores only; must not start with a digit.",
			})
			return
		}
		if goReservedWords[canonical] {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q normalises to Go reserved word %q", source, rawName, canonical),
				fix:    "rename so the compact lowercase form is not a Go keyword or predeclared identifier.",
			})
			return
		}
		if prev, ok := seen[canonical]; ok {
			out = append(out, validationIssue{
				line:   line,
				column: col,
				msg:    fmt.Sprintf("%s.name %q collides with %s after normalisation (both → %q)", source, rawName, prev, canonical),
				fix:    "rename one of the entries so their compact lowercase forms differ.",
			})
			return
		}
		seen[canonical] = source
	}

	for i, fe := range cfg.Frontends {
		check(fe.Name, fmt.Sprintf("frontends[%d]", i), []string{"frontends", fmt.Sprintf("[%d]", i), "name"})
	}

	return out
}

// isValidGoPackageIdent reports whether s is a syntactically-legal Go
// package identifier: starts with an ASCII letter or underscore, and
// the rest are ASCII letters, digits, or underscores. We restrict to
// ASCII even though Go technically allows broader Unicode-letter
// package names — every forge-generated import path, directory name,
// and KCL/k8s identifier downstream assumes ASCII, so a Unicode-letter
// service name would surface as a downstream error far from the cause.
func isValidGoPackageIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r > unicode.MaxASCII {
			return false
		}
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if i == 0 {
			if !isLetter {
				return false
			}
			continue
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

// looksLikeGoModulePath does a cheap shape check so we catch obvious
// typos (e.g. a stray period only) without trying to be a full Go
// modules validator. The Go module path rule we enforce: contains at
// least one slash and no whitespace.
func looksLikeGoModulePath(s string) bool {
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	if !strings.Contains(s, "/") {
		return false
	}
	return true
}
