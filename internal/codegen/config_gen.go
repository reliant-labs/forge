package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

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
}

// ConfigTemplateData is the top-level data passed to the config.go template.
type ConfigTemplateData struct {
	Fields       []ConfigTemplateField
	FieldNames   map[string]bool // GoName → true for quick existence checks in templates
	NeedsStrconv bool
}

// CmdServerTemplateData holds the data passed to cmd-server.go.tmpl.
// It combines project-level data (Module) with config field awareness
// so the template can conditionally include code that references
// specific config fields.
type CmdServerTemplateData struct {
	Module       string
	ConfigFields map[string]bool
}

// GenerateCmdServer re-renders cmd/server.go with config field awareness.
// Called during `forge generate` so that cmd/server.go stays in sync with
// the actual config proto fields.
func GenerateCmdServer(messages []ConfigMessage, targetDir string) error {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}

	data := CmdServerTemplateData{
		Module:       modulePath,
		ConfigFields: ConfigFieldNamesFromMessages(messages),
	}

	content, err := templates.ProjectTemplates.Render("cmd-server.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render cmd-server.go.tmpl: %w", err)
	}

	cmdDir := filepath.Join(targetDir, "cmd")
	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return fmt.Errorf("create cmd/: %w", err)
	}

	outPath := filepath.Join(cmdDir, "server.go")
	return os.WriteFile(outPath, content, 0644)
}

// GenerateCmdServerWithFields renders cmd/server.go using a pre-built
// config field map. This variant is used when the caller needs to modify
// the field set (e.g. stripping migration fields when the migrations
// feature is disabled).
func GenerateCmdServerWithFields(configFields map[string]bool, targetDir string) error {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}

	data := CmdServerTemplateData{
		Module:       modulePath,
		ConfigFields: configFields,
	}

	content, err := templates.ProjectTemplates.Render("cmd-server.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render cmd-server.go.tmpl: %w", err)
	}

	cmdDir := filepath.Join(targetDir, "cmd")
	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return fmt.Errorf("create cmd/: %w", err)
	}

	outPath := filepath.Join(cmdDir, "server.go")
	return os.WriteFile(outPath, content, 0644)
}

// GenerateConfigLoader generates pkg/config/config.go from parsed config messages.
func GenerateConfigLoader(messages []ConfigMessage, targetDir string) error {
	// Flatten all fields from all messages into a single Config struct.
	// Most projects will have a single config message, but we support multiple.
	var fields []ConfigTemplateField
	needsStrconv := false

	for _, msg := range messages {
		for _, f := range msg.Fields {
			tf := ConfigTemplateField{
				GoName:       f.GoName,
				GoType:       f.GoType,
				EnvVar:       f.EnvVar,
				Flag:         f.Flag,
				DefaultValue: f.DefaultValue,
				Description:  f.Description,
				Required:     f.Required,
				HasDefault:   f.DefaultValue != "",
			}

			// Pre-parse typed defaults for the template
			if f.DefaultValue != "" {
				switch f.GoType {
				case "int32":
					v, err := strconv.ParseInt(f.DefaultValue, 10, 32)
					if err == nil {
						tf.DefaultInt32 = int32(v)
					}
				case "int64":
					v, err := strconv.ParseInt(f.DefaultValue, 10, 64)
					if err == nil {
						tf.DefaultInt64 = v
					}
				case "bool":
					v, err := strconv.ParseBool(f.DefaultValue)
					if err == nil {
						tf.DefaultBool = v
					}
				case "float32":
					v, err := strconv.ParseFloat(f.DefaultValue, 32)
					if err == nil {
						tf.DefaultFloat32 = float32(v)
					}
				case "float64":
					v, err := strconv.ParseFloat(f.DefaultValue, 64)
					if err == nil {
						tf.DefaultFloat64 = v
					}
				}
			} else {
				// Set safe zero defaults for non-string types
				// (template uses these for flag registration)
				switch f.GoType {
				case "int32":
					tf.DefaultInt32 = 0
				case "int64":
					tf.DefaultInt64 = 0
				case "bool":
					tf.DefaultBool = false
				case "float32":
					tf.DefaultFloat32 = 0
				case "float64":
					tf.DefaultFloat64 = 0
				}
			}

			// Track whether we need strconv import
			if f.GoType != "string" {
				needsStrconv = true
			}

			fields = append(fields, tf)
		}
	}

	if len(fields) == 0 {
		return nil
	}

	fieldNames := make(map[string]bool, len(fields))
	for _, f := range fields {
		fieldNames[f.GoName] = true
	}

	data := ConfigTemplateData{
		Fields:       fields,
		FieldNames:   fieldNames,
		NeedsStrconv: needsStrconv,
	}

	configDir := filepath.Join(targetDir, "pkg", "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create pkg/config: %w", err)
	}

	content, err := templates.ProjectTemplates.Render("config.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render config.go.tmpl: %w", err)
	}

	outPath := filepath.Join(configDir, "config.go")
	if err := os.WriteFile(outPath, content, 0644); err != nil {
		return err
	}

	// Generate .env.example at the project root
	envContent, err := templates.ProjectTemplates.Render("env.example.tmpl", data)
	if err != nil {
		return fmt.Errorf("render env.example.tmpl: %w", err)
	}
	envPath := filepath.Join(targetDir, ".env.example")
	return os.WriteFile(envPath, envContent, 0644)
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
					Name:        "database_url",
					GoName:      "DatabaseUrl",
					GoType:      "string",
					ProtoType:   "string",
					EnvVar:      "DATABASE_URL",
					Flag:        "database-url",
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
					Description:  "Runtime environment (production, development). In development, some defaults are permissive (e.g. authz allow-all) for local ergonomics — never use development in production.",
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