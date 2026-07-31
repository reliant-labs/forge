// Package codegen — config_native_emit.go wires the KCL config emitters
// (config_schema_gen.go, config_projection_gen.go, and the per-env config.k
// scaffolder below) into the generate pipeline.
//
// Two ownership tiers, mirroring the design:
//
//   - config_schema.k + config_projection.k are PROJECT-LEVEL and Tier-1
//     forge-owned (writeForgeOwned): one pair per project, regenerated from
//     proto on every `forge generate`. They carry the config TYPE and the
//     projection BEHAVIOR — turning a typed AppConfig into the agnostic-core
//     env map every workload's env is built from.
//   - deploy/kcl/<env>/config.k is PER-ENV and USER-OWNED (write-if-absent):
//     a typed AppConfig instance carrying the per-env values. forge scaffolds
//     it once from the proto's own defaults and never clobbers later edits.
package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
)

// Dev-environment conventions the config.k scaffolder pins so a freshly-forged
// dev environment is turnkey.
const (
	// configDevEnvName is the conventional development environment. Its
	// config.k is seeded with the MODE marker + a concrete local database DSN.
	configDevEnvName = "dev"
	// configDevModeValue is the runtime MODE value marking an environment as
	// development (config.Mode() keys off the CONFIG_FIELD_ROLE_MODE field).
	configDevModeValue = "development"
	// configModeRole is the annotation role that marks the runtime-MODE field.
	configModeRole = "CONFIG_FIELD_ROLE_MODE"
	// configDBURLEnvVar is the env var of the database connection field the dev
	// scaffold seeds with a concrete local DSN.
	configDBURLEnvVar = "DATABASE_URL"
	// configAutoMigrateEnvVar is the env var of the boolean field the dev
	// scaffold seeds True so a freshly-created dev DB is migrated on the first
	// `forge run` boot — the app applies its migrations, THEN auto-seed
	// populates the schema. Non-dev envs inherit the proto default (false): a
	// prod deploy owns its migration story (a Job/initContainer), never an
	// implicit on-boot migrate.
	configAutoMigrateEnvVar = "AUTO_MIGRATE"
	// configDevDBPort is the host port the scaffold's dev postgres binds — the
	// shadow/dev database coordinate `forge` pins for local development.
	configDevDBPort = "5434"
)

// GenerateConfigNativeShared emits the two project-level, forge-owned KCL
// files that back the config path — <kclDirAbs>/config_schema.k and
// <kclDirAbs>/config_projection.k — from the proto-derived config fields.
//
// projectDir is the project root (for the checksum-relative path); kclDirAbs is
// the absolute deploy/kcl directory; cs is the checksum ledger. When cs is nil
// or the path can't be made relative, the files are still written (untracked).
func GenerateConfigNativeShared(fields []ConfigField, projectName, projectDir, kclDirAbs string, cs *checksums.FileChecksums) error {
	schema, err := GenerateConfigSchemaKCL(fields, projectName)
	if err != nil {
		return fmt.Errorf("generate config schema: %w", err)
	}
	proj, err := GenerateConfigProjectionKCL(fields)
	if err != nil {
		return fmt.Errorf("generate config projection: %w", err)
	}

	files := []struct{ name, body string }{
		{ConfigSchemaModule + ".k", schema},
		{"config_projection.k", proj},
	}
	for _, f := range files {
		outPath := filepath.Join(kclDirAbs, f.name)
		if cs != nil && projectDir != "" {
			if rel, rerr := filepath.Rel(projectDir, outPath); rerr == nil {
				if werr := writeForgeOwned(projectDir, rel, []byte(f.body), cs); werr != nil {
					return fmt.Errorf("write %s: %w", outPath, werr)
				}
				continue
			}
		}
		if werr := writeUserScaffold(outPath, []byte(f.body)); werr != nil {
			return fmt.Errorf("write %s: %w", outPath, werr)
		}
	}
	return nil
}

// GenerateConfigKScaffold emits deploy/kcl/<envName>/config.k — the per-env,
// user-owned typed AppConfig VALUES instance — ONLY when it does not already
// exist. Returns true when a fresh file was written, false when an existing
// user-owned file was left untouched.
func GenerateConfigKScaffold(fields []ConfigField, projectName, kclDirAbs, envName string) (bool, error) {
	body := generateConfigKBody(fields, projectName, envName)
	outDir := filepath.Join(kclDirAbs, envName)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return false, fmt.Errorf("create %s: %w", outDir, err)
	}
	return writeUserScaffoldIfAbsent(filepath.Join(outDir, "config.k"), []byte(body))
}

// EnvSecretsFileName is the per-env dotenv the `dotenv` secret provider reads:
// `.env.<env>` at the project root, keyed by ENV-VAR NAME (the convention
// internal/secrets resolves on). It is gitignored — it holds secret VALUES,
// the half that never lives in git.
func EnvSecretsFileName(envName string) string { return ".env." + envName }

// GenerateEnvSecretsScaffold emits the gitignored `.env.<envName>` that backs
// the env's `dotenv` secret provider — ONLY for a LOCAL env, and ONLY when the
// file does not already exist. Returns true when a fresh file was written.
//
// One line per SENSITIVE config field, keyed by env-var NAME. The dev database
// URL is seeded with the concrete local DSN (the coordinate the scaffold's
// postgres binds) so `forge run` and `forge env deploy dev` are turnkey with
// zero hand-set values; every other sensitive field is seeded EMPTY, as a
// labelled slot to fill.
//
// Only local envs get one. A cloud env declares `forge.ExternalSecrets {}` —
// forge never sees those values, so scaffolding a dotenv there would create a
// second, unread place to put a production credential.
func GenerateEnvSecretsScaffold(fields []ConfigField, projectName, projectDir, envName string) (bool, error) {
	if envName != configDevEnvName {
		return false, nil
	}
	var sensitive []ConfigField
	for _, f := range fields {
		if f.Sensitive && f.EnvVar != "" {
			sensitive = append(sensitive, f)
		}
	}
	if len(sensitive) == 0 {
		return false, nil
	}
	return writeUserScaffoldIfAbsent(
		filepath.Join(projectDir, EnvSecretsFileName(envName)),
		[]byte(generateEnvSecretsBody(sensitive, projectName, envName)),
	)
}

// generateEnvSecretsBody renders the `.env.<env>` body: a header explaining
// the file's role, then `<ENV_VAR>=<value>` per sensitive field.
func generateEnvSecretsBody(sensitive []ConfigField, projectName, envName string) string {
	var b strings.Builder
	b.WriteString("# SECRET VALUES for the `" + envName + "` environment — GITIGNORED, never commit.\n")
	b.WriteString("#\n")
	b.WriteString("# This is the `dotenv` secret provider deploy/kcl/" + envName + "/main.k declares.\n")
	b.WriteString("# Every config field marked `sensitive: true` in proto/config/v1/config.proto\n")
	b.WriteString("# is projected into the manifest as a valueFrom.secretKeyRef, NOT an inline\n")
	b.WriteString("# value — so its VALUE lives here, keyed by env-var NAME. forge reads this\n")
	b.WriteString("# file to (a) layer the values onto host processes (`forge run`) and (b) render\n")
	b.WriteString("# + apply the backing Secret into LOCAL clusters only.\n")
	b.WriteString("#\n")
	b.WriteString("# Cloud environments do NOT read a dotenv: they declare\n")
	b.WriteString("# `forge.ExternalSecrets {}` and the Secret is provisioned out of band.\n\n")
	for _, f := range sensitive {
		if d := strings.TrimSpace(f.Description); d != "" {
			fmt.Fprintf(&b, "# %s\n", strings.Join(strings.Fields(d), " "))
		}
		fmt.Fprintf(&b, "%s=%s\n", f.EnvVar, devSecretValue(f, projectName))
	}
	return b.String()
}

// devSecretValue is the dev-environment seed for one sensitive field. The
// database-connection field gets the concrete local DSN (the port the
// scaffold's postgres binds, the project's own database) so a fresh clone
// boots turnkey; anything else is an empty slot for the developer to fill.
func devSecretValue(f ConfigField, projectName string) string {
	if f.EnvVar == configDBURLEnvVar {
		return fmt.Sprintf("postgres://postgres:postgres@localhost:%s/%s?sslmode=disable", configDevDBPort, projectName)
	}
	return ""
}

// generateConfigKBody renders the config.k body for one environment from the
// proto-derived config fields — no external input. The file is SPARSE: only
// the fields an env must pin are set, and every other field inherits its
// AppConfig schema default (config_schema.k). A field is pinned when:
//
//   - it is the dev env's MODE field (seeded "development" so the dev env is
//     positively development), or
//   - it is the dev env's AUTO_MIGRATE field (seeded True so the app applies
//     its migrations on the first `forge run` boot — a fresh dev DB comes up
//     with its schema; dev boots alive), or
//   - it is a required field with no schema default (KCL makes such a field
//     mandatory in every AppConfig instance, so config.k must supply it; it
//     gets its type-zero placeholder for the operator to fill — and runtime
//     checkRequired fires loudly if one is left unset).
//
// A SENSITIVE field is never pinned here, in ANY environment. config.k is
// git-tracked, and a sensitive field's AppConfig type is a ConfigSecretRef
// (a Secret name+key REFERENCE), not a value — it already carries the
// default backend as its schema default, so an env that uses that backend
// writes nothing. Its VALUE comes from the env's secret provider; the dev
// value is scaffolded into the gitignored `.env.<env>`
// (GenerateEnvSecretsScaffold). An env that points a sensitive field at a
// DIFFERENT Secret writes the ConfigSecretRef override by hand.
func generateConfigKBody(fields []ConfigField, projectName, envName string) string {
	isDev := envName == configDevEnvName

	var lines []string
	for _, f := range fields {
		// Block-reference / unbound fields carry no env binding of their own.
		if f.EnvVar == "" {
			continue
		}
		// Sensitive: a Secret REFERENCE with a schema default, never a value
		// in a git-tracked file. See the doc comment above.
		if f.Sensitive {
			continue
		}
		switch {
		case isDev && f.Role == configModeRole:
			lines = append(lines, fmt.Sprintf("    %s = %q", f.Name, configDevModeValue))
		case isDev && f.EnvVar == configAutoMigrateEnvVar:
			// Dev boots alive: the app applies its migrations on the first
			// `forge run` so a freshly-created dev DB has its schema before the
			// first-boot auto-seed runs. KCL bool literals are capitalized
			// (True/False); the config projection lowercases this to
			// AUTO_MIGRATE=true for the runtime loader. DEV ONLY — a non-dev env
			// inherits the proto default (false), leaving prod's migration story
			// to the deploy path's migration step (an initContainer).
			lines = append(lines, fmt.Sprintf("    %s = True", f.Name))
		case f.EnvVar == configDBURLEnvVar && isDev:
			// A database URL the project chose NOT to mark sensitive still
			// gets the turnkey local DSN so `forge run` boots against the dev
			// postgres. (The scaffolded proto DOES mark it sensitive, so this
			// branch is only reached by a project that un-marked it.)
			dsn := fmt.Sprintf("postgres://postgres:postgres@localhost:%s/%s?sslmode=disable", configDevDBPort, projectName)
			lines = append(lines, fmt.Sprintf("    %s = %q", f.Name, dsn))
		default:
			if isKCLMandatory(f) {
				lines = append(lines, fmt.Sprintf("    %s = %s", f.Name, kclConfigZeroLiteral(f)))
			}
		}
	}

	var b strings.Builder
	b.WriteString("# Per-environment app config VALUES (user-owned — edit here).\n")
	b.WriteString("# The typed AppConfig schema + projection are generated from\n")
	b.WriteString("# proto/config/v1/config.proto; this instance supplies the per-env values\n")
	b.WriteString("# they project. Only fields this env pins are set — every other field\n")
	b.WriteString("# inherits its AppConfig schema default.\n\n")
	// The AppConfig schema lives in the sibling config_schema.k module; KCL does
	// not share top-level symbols across separately-imported modules, so import
	// + qualify it (see ConfigSchemaModule).
	b.WriteString(fmt.Sprintf("import %s\n\n", ConfigSchemaModule))
	b.WriteString(fmt.Sprintf("app_config: %s.AppConfig = {\n", ConfigSchemaModule))
	for _, l := range lines {
		b.WriteString(l)
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// isKCLMandatory reports whether a config field is one KCL forces an AppConfig
// instance to set — a required field with no schema default. This mirrors
// config_schema_gen.go: such a field is emitted with no `= default`, so KCL
// rejects any instance that omits it.
func isKCLMandatory(f ConfigField) bool {
	return f.Required && f.DefaultValue == ""
}

// kclConfigZeroLiteral returns the type-zero KCL literal for a field in its
// KCL type — the placeholder a mandatory field is scaffolded with when it is
// not otherwise seeded.
func kclConfigZeroLiteral(f ConfigField) string {
	switch kclTypeForProtoConfig(f) {
	case "int":
		return "0"
	case "bool":
		return "False"
	case "float":
		return "0.0"
	default: // str (covers durations, carried as strings)
		return strconv.Quote("")
	}
}
