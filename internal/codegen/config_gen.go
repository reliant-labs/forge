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
	NeedsStrconv bool
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

	data := ConfigTemplateData{
		Fields:       fields,
		NeedsStrconv: needsStrconv,
	}

	configDir := filepath.Join(targetDir, "pkg", "config")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create pkg/config: %w", err)
	}

	content, err := templates.RenderProjectTemplate("config.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render config.go.tmpl: %w", err)
	}

	outPath := filepath.Join(configDir, "config.go")
	if err := os.WriteFile(outPath, content, 0644); err != nil {
		return err
	}

	// Generate .env.example at the project root
	envContent, err := templates.RenderProjectTemplate("env.example.tmpl", data)
	if err != nil {
		return fmt.Errorf("render env.example.tmpl: %w", err)
	}
	envPath := filepath.Join(targetDir, ".env.example")
	return os.WriteFile(envPath, envContent, 0644)
}