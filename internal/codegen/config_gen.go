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
	GoName       string
	GoType       string
	EnvVar       string
	Flag         string
	DefaultValue string
	Description  string
	Required     bool
	HasDefault   bool
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
	return map[string]bool{
		"Port":                    true,
		"LogLevel":                true,
		"DatabaseUrl":             true,
		"CorsOrigins":             true,
		"CorsAllowCredentials":    true,
		"TlsCertPath":             true,
		"TlsKeyPath":              true,
		"PreStopDelay":            true,
		"ShutdownTimeout":         true,
		"LogFormat":               true,
		"AutoMigrate":             true,
		"Environment":             true,
		"RateLimitRps":            true,
		"RateLimitBurst":          true,
		"DbMaxOpenConns":          true,
		"DbMaxIdleConns":          true,
		"DbConnMaxIdleTime":       true,
		"DbConnMaxLifetime":       true,
		"PprofAddr":               true,
		"SecurityHeadersEnabled":  true,
	}
}