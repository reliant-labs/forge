package codegen

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/templates"
)

// ConfigTemplateField holds template data for a single config field.
type ConfigTemplateField struct {
	GoName         string
	GoType         string
	EnvVar         string
	Flag           string
	DefaultValue   string
	Description    string
	Required       bool
	HasDefault     bool
	DefaultInt32   int32
	DefaultInt64   int64
	DefaultBool    bool
	DefaultFloat32 float32
	DefaultFloat64 float64

	// GoPath is the assignment path on the generated Config struct.
	// Root fields: identical to GoName ("Port"). Component config-block
	// leaves: qualified through the block field ("Trader.MaxPerTick").
	// The Load/flag plumbing in config.go.tmpl assigns via GoPath so one
	// flat loop covers both shapes.
	GoPath string

	// IsDuration marks duration-shaped string fields (see
	// isDurationField). They are emitted as time.Duration on the Config
	// struct and parsed ONCE in Load — consumers never re-parse strings,
	// and a typo'd duration fails startup instead of silently zeroing.
	IsDuration bool

	// StructGoType is the Go type emitted on the Config struct:
	// "time.Duration" for duration fields, GoType for everything else.
	StructGoType string

	// ParseFn names the parse helper Load feeds to loadField for this
	// field ("parseString", "parseInt32", "parsePort", "parseGoDuration",
	// …). Selecting it at generate time keeps the emitted Load a flat
	// list of identical one-liners.
	ParseFn string

	// AllowEmptyEnv preserves the historical string semantics: a string
	// env var explicitly set to "" counts as set. Numeric/bool/duration
	// fields treat an empty env var as unset (parsing "" would always
	// error).
	AllowEmptyEnv bool

	// Role is the (forge.v1.config).role annotation (bare enum spelling, ""
	// for none). Codegen selects semantic fields (e.g. the MODE field) by
	// THIS, never by the field's name.
	Role string

	// Sensitive mirrors (forge.v1.config).sensitive. Sensitive fields get NO
	// CLI flag (defense against shell-history / `ps` leaks) and are resolved
	// from env / Secret mount only — never a flag or an inline default.
	Sensitive bool
}

// ConfigTemplateBlockType is one component config-block struct type the
// template declares alongside Config (e.g. `type TraderConfig struct`).
// Deduped by TypeName when the same block message is referenced by more
// than one root field.
type ConfigTemplateBlockType struct {
	TypeName string
	Fields   []ConfigTemplateField
}

// ConfigTemplateBlockField is one block-typed field on the root Config
// struct (e.g. `Trader TraderConfig`).
type ConfigTemplateBlockField struct {
	GoName   string // field on Config, e.g. "Trader"
	TypeName string // block struct type, e.g. "TraderConfig"
}

// ConfigTemplateData is the top-level data passed to the config.go template.
type ConfigTemplateData struct {
	// Fields is every leaf field — root fields plus component config-block
	// leaves — in declaration order, with GoPath set. Drives RegisterFlags
	// and Load so block leaves get the exact same env/flag/default
	// treatment as root fields.
	Fields []ConfigTemplateField
	// RootFields are the leaves declared directly on Config (struct decl).
	RootFields []ConfigTemplateField
	// BlockTypes / BlockFields carry the component config-block shapes:
	// the struct type declarations and the Config fields holding them.
	BlockTypes  []ConfigTemplateBlockType
	BlockFields []ConfigTemplateBlockField
	// RoleModeField is the Go field name of the field tagged
	// role=CONFIG_FIELD_ROLE_MODE, or "" when no field carries it. The
	// generated Mode() reads THIS field — selected by
	// annotation, never by the name "Environment". Renaming the role field
	// is a behavior no-op; naming an unannotated field "environment" never
	// enables dev mode.
	RoleModeField string
	NeedsStrconv  bool

	// Module is the project's Go module path, used by config.go.tmpl to
	// import the generated proto config package (gen/config/v1). Set by
	// GenerateConfigLoader; left empty by callers that only need the
	// field partition (e.g. ConfigBlocksFromMessages).
	Module string
}

// ConfigBlockRef names one component config block as composed on the
// root Config: the Config field holding it and the generated Go type.
// wire_gen consumes this (via ConfigBlocksFromMessages) to resolve Deps
// fields of a block type to `cfg.<FieldName>` by TYPE.
type ConfigBlockRef struct {
	FieldName string // root Config field, e.g. "Trader"
	TypeName  string // generated struct type, e.g. "TraderConfig"
}

// ConfigBlocksFromMessages derives the component config-block references
// from parsed config messages: every message-typed field on a root
// config message whose MessageType names another config message in the
// set. Order follows root-message declaration order, so consumers get a
// deterministic candidate list.
func ConfigBlocksFromMessages(messages []ConfigMessage) []ConfigBlockRef {
	data := BuildConfigTemplateData(messages)
	refs := make([]ConfigBlockRef, 0, len(data.BlockFields))
	for _, bf := range data.BlockFields {
		refs = append(refs, ConfigBlockRef{FieldName: bf.GoName, TypeName: bf.TypeName})
	}
	return refs
}

// BuildConfigTemplateData partitions parsed config messages into the
// template shape:
//
//   - Block messages — those referenced by a MessageType field of another
//     config message — become nested struct types (`type TraderConfig
//     struct`) plus a typed field on Config (`Trader TraderConfig`).
//   - Every other message's scalar fields flatten onto the root Config
//     struct exactly as before (most projects have a single AppConfig).
//
// Block leaves keep their own env_var/flag/default annotations and join
// the flat Fields list with a qualified GoPath, so env binding, flag
// registration, and per-env deploy projection all reuse the existing
// flat plumbing unchanged.
//
// One nesting level is supported: message-typed fields ON a block
// message are ignored. References to messages that aren't in the set
// (or carry no config fields) are skipped.
func BuildConfigTemplateData(messages []ConfigMessage) ConfigTemplateData {
	byName := make(map[string]*ConfigMessage, len(messages))
	for i := range messages {
		byName[messages[i].Name] = &messages[i]
	}

	// A message is a block iff some OTHER message references it via a
	// MessageType field. Everything else is a root message.
	isBlock := map[string]bool{}
	for _, m := range messages {
		for _, f := range m.Fields {
			if f.ProtoType != "message" || f.MessageType == "" || f.MessageType == m.Name {
				continue
			}
			if _, known := byName[f.MessageType]; known {
				isBlock[f.MessageType] = true
			}
		}
	}

	data := ConfigTemplateData{}
	seenBlockType := map[string]bool{}
	for _, m := range messages {
		if isBlock[m.Name] {
			continue
		}
		for _, f := range m.Fields {
			if f.ProtoType == "message" {
				if f.MessageType == "" || !isBlock[f.MessageType] {
					// A message-typed config field that is NOT a component
					// config block is a well-known wrapper carrying its own
					// (forge.v1.config) annotation — in practice a
					// google.protobuf.Duration leaf (pre_stop_delay,
					// shutdown_timeout, db_conn_max_*). It binds to a single
					// env var / flag like any scalar, so it MUST flow into
					// the per-env KCL projection. Emit it as
					// a leaf (NOT a struct field — the config object is the
					// proto type now, so there is no generated struct). Skip
					// only un-annotated message fields (genuinely nothing to
					// bind).
					if f.EnvVar != "" || f.Flag != "" {
						tf := configTemplateField(f, f.GoName)
						data.Fields = append(data.Fields, tf)
					}
					continue
				}
				bm := byName[f.MessageType]
				data.BlockFields = append(data.BlockFields, ConfigTemplateBlockField{
					GoName:   f.GoName,
					TypeName: f.MessageType,
				})
				if !seenBlockType[f.MessageType] {
					seenBlockType[f.MessageType] = true
					bt := ConfigTemplateBlockType{TypeName: f.MessageType}
					for _, bf := range bm.Fields {
						if bf.ProtoType == "message" {
							continue // one nesting level only
						}
						bt.Fields = append(bt.Fields, configTemplateField(bf, bf.GoName))
					}
					data.BlockTypes = append(data.BlockTypes, bt)
				}
				for _, bf := range bm.Fields {
					if bf.ProtoType == "message" {
						continue
					}
					data.Fields = append(data.Fields, configTemplateField(bf, f.GoName+"."+bf.GoName))
				}
				continue
			}

			tf := configTemplateField(f, f.GoName)
			data.RootFields = append(data.RootFields, tf)
			data.Fields = append(data.Fields, tf)

			// Select the MODE field by ANNOTATION, never by name. The first
			// root field tagged role=MODE wins; a project should declare at
			// most one. This is the substitute for the deleted
			// `index .FieldNames "Environment"` name-magic.
			if data.RoleModeField == "" && f.Role == "CONFIG_FIELD_ROLE_MODE" {
				data.RoleModeField = tf.GoName
			}
		}
	}

	for _, f := range data.Fields {
		if f.GoType != "string" {
			data.NeedsStrconv = true
			break
		}
	}
	return data
}

// isDurationField reports whether a string config field carries a Go
// duration. Two signals:
//
//  1. The four well-known scaffold duration fields by name — the
//     generated cmd/server.go assigns them to serverkit's time.Duration
//     config directly, so their typing is part of the contract.
//  2. Any other string field whose default parses as a Go duration AND
//     contains a unit letter (so a plain "0" or numeric default stays a
//     string).
func isDurationField(f ConfigField) bool {
	if f.GoType != "string" {
		return false
	}
	switch f.GoName {
	case "PreStopDelay", "ShutdownTimeout", "DbConnMaxIdleTime", "DbConnMaxLifetime":
		return true
	}
	if f.DefaultValue == "" {
		return false
	}
	if _, err := time.ParseDuration(f.DefaultValue); err != nil {
		return false
	}
	return strings.ContainsAny(f.DefaultValue, "nsuµmh")
}

// parseFnFor selects the Load parse helper for a field.
func parseFnFor(f ConfigField, isDuration bool) string {
	if isDuration {
		return "parseGoDuration"
	}
	switch f.GoType {
	case "int32":
		// Ports get strict uint16 validation.
		if f.GoName == "Port" || f.Flag == "port" {
			return "parsePort"
		}
		return "parseInt32"
	case "int64":
		return "parseInt64"
	case "bool":
		return "parseBool"
	case "float32":
		return "parseFloat32"
	case "float64":
		return "parseFloat64"
	default:
		return "parseString"
	}
}

// configTemplateField converts one parsed ConfigField to its template
// shape, pre-parsing typed defaults and recording the assignment path.
func configTemplateField(f ConfigField, goPath string) ConfigTemplateField {
	isDur := isDurationField(f)
	structType := f.GoType
	if isDur {
		structType = "time.Duration"
	}
	tf := ConfigTemplateField{
		GoName:        f.GoName,
		GoType:        f.GoType,
		EnvVar:        f.EnvVar,
		Flag:          f.Flag,
		DefaultValue:  f.DefaultValue,
		Description:   f.Description,
		Required:      f.Required,
		HasDefault:    f.DefaultValue != "",
		GoPath:        goPath,
		IsDuration:    isDur,
		StructGoType:  structType,
		ParseFn:       parseFnFor(f, isDur),
		AllowEmptyEnv: f.GoType == "string" && !isDur,
		Role:          f.Role,
		Sensitive:     f.Sensitive,
	}

	if f.DefaultValue != "" {
		switch f.GoType {
		case "int32":
			if v, err := strconv.ParseInt(f.DefaultValue, 10, 32); err == nil {
				tf.DefaultInt32 = int32(v)
			}
		case "int64":
			if v, err := strconv.ParseInt(f.DefaultValue, 10, 64); err == nil {
				tf.DefaultInt64 = v
			}
		case "bool":
			if v, err := strconv.ParseBool(f.DefaultValue); err == nil {
				tf.DefaultBool = v
			}
		case "float32":
			if v, err := strconv.ParseFloat(f.DefaultValue, 32); err == nil {
				tf.DefaultFloat32 = float32(v)
			}
		case "float64":
			if v, err := strconv.ParseFloat(f.DefaultValue, 64); err == nil {
				tf.DefaultFloat64 = v
			}
		}
	}
	return tf
}

// CmdServerTemplateData holds the data passed to cmd-server.go.tmpl.
// It combines project-level data (Module) with config field awareness
// so the template can conditionally include code that references
// specific config fields. The generated runServer calls the OWNED
// app.SetupAuth() unconditionally (auth is code now, not config), so no
// auth-provider field is threaded here.
type CmdServerTemplateData struct {
	Module       string
	ConfigFields map[string]bool

	// Name is the primary binary's name — the cmd/<Name>/ leaf. The
	// scaffold-once command-tree templates reference it when they explain
	// where a file sits or what a command is invoked as (`<Name> server`,
	// the `-X <module>/cmd/<Name>/cmd.version=` ldflags path). Derived from
	// the cmd tree on disk by primaryCmdTreeDir, not re-read from
	// forge.yaml, so it agrees with the directory the file is written into
	// even for a project whose binary was renamed.
	Name string

	// RESTEnabled mirrors forge.yaml `api.rest: true`. When set the cmd
	// composition site builds a vanguard transcoder over the mounted
	// services' Connect paths and serves it in place of the bare mux.
	// Filled by generateCmdServerData from projectAPIRESTEnabled.
	RESTEnabled bool
}

// GenerateCmdServer re-renders cmd/server.go with config field awareness.
// Called during `forge generate` so that cmd/server.go stays in sync with
// the actual config proto fields.
//
// cs is the project's checksum tracker — passing it keeps cmd/server.go
// out of `forge project audit`'s orphan/user-edited lists. A nil cs is tolerated.
func GenerateCmdServer(messages []ConfigMessage, targetDir string, cs *checksums.FileChecksums) error {
	return generateCmdServerData(CmdServerTemplateData{
		ConfigFields: ConfigFieldNamesFromMessages(messages),
	}, targetDir, cs)
}

// GenerateCmdServerWithFields renders cmd/server.go using a pre-built
// config field map (e.g. with migration fields stripped when the
// migrations feature is disabled).
func GenerateCmdServerWithFields(configFields map[string]bool, targetDir string, cs *checksums.FileChecksums) error {
	return generateCmdServerData(CmdServerTemplateData{
		ConfigFields: configFields,
	}, targetDir, cs)
}

// generateCmdServerData is the shared render+write tail of the
// GenerateCmdServer* variants. Fills Module from go.mod.
func generateCmdServerData(data CmdServerTemplateData, targetDir string, cs *checksums.FileChecksums) error {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}
	data.Module = modulePath
	data.RESTEnabled = projectAPIRESTEnabled(targetDir)

	// The SCAFFOLD-ONCE half of the command tree. Each of these files is
	// all decisions and no ceremony, so forge writes each exactly once and
	// never rewrites it:
	//
	//   - serve.go   the serve pipeline (auth posture, interceptor order,
	//                payload caps, CORS, readiness, teardown)
	//   - server.go  the all-services ServeSpec (what this process mounts
	//                and supervises, and whether boot fails on a gap)
	//   - version.go the ldflags-stamped variables (renaming one changes
	//                your build's -X targets)
	//   - db.go      migration policy (fail hard on a dirty schema vs
	//                auto-force; is "nothing pending" success)
	//
	// The invariant steps they used to carry live in pkg/serverkit,
	// pkg/cmdkit and pkg/migratekit now, which is what keeps an owned copy
	// on the upgrade path — those improvements arrive by dependency bump,
	// not re-render.
	//
	// These calls therefore only BIRTH a file for a tree that has none yet
	// (an older project, or one whose cmd layer predates it). An existing
	// copy is left untouched, edits and all — which is precisely the
	// migration path for projects whose copies were Tier-1 until now.
	treeDir := primaryCmdTreeDir(targetDir)
	if treeDir == "" {
		return nil
	}
	// treeDir is cmd/<bin>/cmd, so the binary name is its grandparent leaf.
	data.Name = filepath.Base(filepath.Dir(treeDir))

	scaffolds := []struct{ tmpl, file string }{
		{"cmd-tree-serve.go.tmpl", "serve.go"},
		{"cmd-tree-server.go.tmpl", "server.go"},
		{"cmd-tree-version.go.tmpl", "version.go"},
	}
	// db.go is birthed only when the project HAS a migration story:
	// cmd/<bin>/cmd/root.go gates its newDBCmd(deps) call on the same
	// AutoMigrate config field, so emitting one without the other yields
	// either an unused function or an undefined reference.
	if data.ConfigFields["AutoMigrate"] {
		scaffolds = append(scaffolds, struct{ tmpl, file string }{"cmd-tree-db.go.tmpl", "db.go"})
	}

	for _, s := range scaffolds {
		content, rerr := templates.ProjectTemplates().Render(s.tmpl, data)
		if rerr != nil {
			return fmt.Errorf("render %s: %w", s.tmpl, rerr)
		}
		dest := filepath.Join(treeDir, s.file)
		if _, werr := writeForgeScaffoldOnce(targetDir, dest, content); werr != nil {
			return fmt.Errorf("write %s: %w", dest, werr)
		}
	}
	return nil
}

// primaryCmdTreeDir returns the project-relative directory of the primary
// binary's command tree (cmd/<bin>/cmd), discovered by glob so this path
// doesn't need to re-read forge.yaml for the binary name.
//
// It anchors on root.go, NOT on any of the scaffold-once files that live
// beside it. Those are write-if-absent, so "it isn't there yet" is precisely
// the case that still needs a destination — anchoring on a file we might be
// about to create would make it uncreatable. root.go is the right anchor
// because it is Tier-1 (regenerated every run, hence always present in a
// service tree) and lives in exactly the directory they belong to. Returns
// "" when there is no cmd tree at all — a CLI/library kind, or a project
// whose tree has not been generated yet.
func primaryCmdTreeDir(targetDir string) string {
	matches, _ := filepath.Glob(filepath.Join(targetDir, "cmd", "*", "cmd", "root.go"))
	if len(matches) == 0 {
		return ""
	}
	rel, err := filepath.Rel(targetDir, filepath.Dir(matches[0]))
	if err != nil {
		return ""
	}
	return rel
}

// GenerateConfigLoader generates pkg/config/config.go from parsed config messages.
//
// cs is the project's checksum tracker. Passing it ensures the generated
// pkg/config/config.go is recorded so `forge project audit` doesn't flag it as an
// orphan. A nil cs is tolerated (file is still written).
func GenerateConfigLoader(messages []ConfigMessage, targetDir string, cs *checksums.FileChecksums) error {
	// Partition messages into root fields + component config blocks.
	// Most projects have a single flat AppConfig; projects with block
	// messages additionally get nested struct types on Config.
	data := BuildConfigTemplateData(messages)

	if len(data.Fields) == 0 {
		return nil
	}

	// config.go.tmpl is a thin shim that aliases Config to the generated
	// proto type (gen/config/v1.AppConfig) and wraps the descriptor-driven
	// loader — it needs the module path for that import.
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}
	data.Module = modulePath

	content, err := templates.ProjectTemplates().Render("config.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render config.go.tmpl: %w", err)
	}

	if err := writeForgeOwned(targetDir, filepath.Join("pkg", "config", "config.go"), content, cs); err != nil {
		return fmt.Errorf("write pkg/config/config.go: %w", err)
	}
	return nil
}

// ConfigFieldNamesFromMessages returns a map of Go field names present in the
// given config messages. Used by templates to conditionally include code
// blocks that reference specific config fields.
func ConfigFieldNamesFromMessages(messages []ConfigMessage) map[string]bool {
	names := make(map[string]bool)
	for _, msg := range messages {
		for _, f := range msg.Fields {
			names[f.GoName] = true
		}
	}
	return names
}

// DefaultConfigFieldNames returns the field names from the default scaffold
// config proto. Used at initial project scaffold time before the config
// proto has been parsed by the generator.
func DefaultConfigFieldNames() map[string]bool {
	return ConfigFieldNamesFromMessages(DefaultConfigMessages())
}

// DefaultConfigMessages returns the default scaffold config metadata used
// before protoc-gen-forge has produced a descriptor for proto/config/config.proto.
func DefaultConfigMessages() []ConfigMessage {
	return []ConfigMessage{
		{
			Name: "AppConfig",
			Fields: []ConfigField{
				{
					Name:         "port",
					GoName:       "Port",
					GoType:       "int32",
					ProtoType:    "int32",
					EnvVar:       "PORT",
					Flag:         "port",
					DefaultValue: "8080",
					Description:  "HTTP server port",
				},
				{
					Name:         "log_level",
					GoName:       "LogLevel",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "LOG_LEVEL",
					Flag:         "log-level",
					DefaultValue: "info",
					Description:  "Log level (debug, info, warn, error)",
				},
				{
					Name:      "database_url",
					GoName:    "DatabaseUrl",
					GoType:    "string",
					ProtoType: "string",
					EnvVar:    "DATABASE_URL",
					Flag:      "database-url",
					Required:  true,
					// Mirrors the scaffolded proto (config.proto.tmpl): the DSN
					// embeds the database password, so it is projected as a Secret
					// REFERENCE, never an inline manifest value. This fallback set
					// is what a project with no readable config proto renders from,
					// so it must not be the lenient one.
					Sensitive:   true,
					Description: "PostgreSQL connection string",
				},
				{
					Name:        "cors_origins",
					GoName:      "CorsOrigins",
					GoType:      "string",
					ProtoType:   "string",
					EnvVar:      "CORS_ORIGINS",
					Flag:        "cors-origins",
					Description: "Comma-separated list of allowed CORS origins",
				},
				{
					Name:         "cors_allow_credentials",
					GoName:       "CorsAllowCredentials",
					GoType:       "bool",
					ProtoType:    "bool",
					EnvVar:       "CORS_ALLOW_CREDENTIALS",
					Flag:         "cors-allow-credentials",
					DefaultValue: "false",
					Description:  "Set Access-Control-Allow-Credentials: true on CORS responses. MUST NOT be combined with a wildcard origin ('*') — that combination is spec-invalid and will be rejected at startup.",
				},
				{
					Name:        "tls_cert_path",
					GoName:      "TlsCertPath",
					GoType:      "string",
					ProtoType:   "string",
					EnvVar:      "TLS_CERT_PATH",
					Flag:        "tls-cert-path",
					Description: "Path to the TLS certificate (PEM). When both this and TLS_KEY_PATH are set, the server listens over HTTPS; otherwise it serves plaintext. Setting only one of the two is a configuration error.",
				},
				{
					Name:        "tls_key_path",
					GoName:      "TlsKeyPath",
					GoType:      "string",
					ProtoType:   "string",
					EnvVar:      "TLS_KEY_PATH",
					Flag:        "tls-key-path",
					Description: "Path to the TLS private key (PEM). See TLS_CERT_PATH.",
				},
				{
					Name:         "pre_stop_delay",
					GoName:       "PreStopDelay",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "PRE_STOP_DELAY",
					Flag:         "pre-stop-delay",
					DefaultValue: "5s",
					Description:  "Duration to wait after flipping readiness to false before beginning HTTP shutdown. Gives load balancers time to observe the failing probe and stop routing new traffic (Go duration).",
				},
				{
					Name:         "shutdown_timeout",
					GoName:       "ShutdownTimeout",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "SHUTDOWN_TIMEOUT",
					Flag:         "shutdown-timeout",
					DefaultValue: "30s",
					Description:  "Maximum time to wait for in-flight requests and workers to drain during graceful shutdown (Go duration).",
				},
				{
					Name:         "log_format",
					GoName:       "LogFormat",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "LOG_FORMAT",
					Flag:         "log-format",
					DefaultValue: "json",
					Description:  "Log output format. One of 'json' (structured, machine-readable) or 'text' (human-friendly). Any other value is rejected at startup.",
				},
				{
					Name:         "auto_migrate",
					GoName:       "AutoMigrate",
					GoType:       "bool",
					ProtoType:    "bool",
					EnvVar:       "AUTO_MIGRATE",
					Flag:         "auto-migrate",
					DefaultValue: "false",
					Description:  "Run database migrations on startup",
				},
				{
					Name:         "environment",
					GoName:       "Environment",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "ENVIRONMENT",
					Flag:         "environment",
					DefaultValue: "production",
					Role:         "CONFIG_FIELD_ROLE_MODE",
					Description:  "Runtime environment (production, development). In development, some defaults are permissive (e.g. verbose errors, relaxed CORS) for local ergonomics — never use development in production.",
				},
				{
					Name:         "rate_limit_rps",
					GoName:       "RateLimitRps",
					GoType:       "int32",
					ProtoType:    "int32",
					EnvVar:       "RATE_LIMIT_RPS",
					Flag:         "rate-limit-rps",
					DefaultValue: "100",
					Description:  "Per-key request rate limit (requests per second). 0 or negative disables rate limiting.",
				},
				{
					Name:         "rate_limit_burst",
					GoName:       "RateLimitBurst",
					GoType:       "int32",
					ProtoType:    "int32",
					EnvVar:       "RATE_LIMIT_BURST",
					Flag:         "rate-limit-burst",
					DefaultValue: "200",
					Description:  "Per-key rate limit burst size. Must be >= rate_limit_rps.",
				},
				{
					Name:         "db_max_open_conns",
					GoName:       "DbMaxOpenConns",
					GoType:       "int32",
					ProtoType:    "int32",
					EnvVar:       "DB_MAX_OPEN_CONNS",
					Flag:         "db-max-open-conns",
					DefaultValue: "25",
					Description:  "Maximum number of open database connections.",
				},
				{
					Name:         "db_max_idle_conns",
					GoName:       "DbMaxIdleConns",
					GoType:       "int32",
					ProtoType:    "int32",
					EnvVar:       "DB_MAX_IDLE_CONNS",
					Flag:         "db-max-idle-conns",
					DefaultValue: "5",
					Description:  "Maximum number of idle database connections kept in the pool.",
				},
				{
					Name:         "db_conn_max_idle_time",
					GoName:       "DbConnMaxIdleTime",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "DB_CONN_MAX_IDLE_TIME",
					Flag:         "db-conn-max-idle-time",
					DefaultValue: "5m",
					Description:  "Maximum amount of time a connection may be idle before being closed (Go duration, e.g. 5m).",
				},
				{
					Name:         "db_conn_max_lifetime",
					GoName:       "DbConnMaxLifetime",
					GoType:       "string",
					ProtoType:    "string",
					EnvVar:       "DB_CONN_MAX_LIFETIME",
					Flag:         "db-conn-max-lifetime",
					DefaultValue: "30m",
					Description:  "Maximum amount of time a connection may be reused before being closed (Go duration, e.g. 30m).",
				},
				{
					Name:        "pprof_addr",
					GoName:      "PprofAddr",
					GoType:      "string",
					ProtoType:   "string",
					EnvVar:      "PPROF_ADDR",
					Flag:        "pprof-addr",
					Description: "If set, starts a net/http/pprof server on this address (e.g. localhost:6060). Never expose publicly. Empty disables pprof.",
				},
				{
					Name:         "security_headers_enabled",
					GoName:       "SecurityHeadersEnabled",
					GoType:       "bool",
					ProtoType:    "bool",
					EnvVar:       "SECURITY_HEADERS_ENABLED",
					Flag:         "security-headers-enabled",
					DefaultValue: "true",
					Description:  "Set OWASP security response headers (CSP, X-Content-Type-Options, Referrer-Policy, Permissions-Policy, HSTS in production). Disable only if a dedicated edge proxy already sets them.",
				},
			},
		},
	}
}
