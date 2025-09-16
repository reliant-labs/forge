package codegen

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/naming"
)

// ConfigField represents a single field in a config proto message
// with ConfigFieldOptions annotations.
type ConfigField struct {
	Name         string // Proto field name (e.g., "database_url")
	GoName       string // Go field name (e.g., "DatabaseUrl")
	GoType       string // Go type (e.g., "string", "int32", "bool")
	ProtoType    string // Proto type (e.g., "string", "int32", "bool")
	EnvVar       string // From config_field.env_var
	Flag         string // From config_field.flag
	DefaultValue string // From config_field.default_value
	Required     bool   // From config_field.required
	Description  string // From config_field.description
}

// ConfigMessage represents a parsed config proto message.
type ConfigMessage struct {
	Name   string        // Message name (e.g., "AppConfig")
	Fields []ConfigField // Fields with config_field annotations
}

// configFieldOptionRe matches individual key-value pairs inside a config_field block.
var (
	configMessageRe = regexp.MustCompile(`message\s+(\w+)\s*\{`)
	configFieldRe   = regexp.MustCompile(`^\s*(string|int32|int64|bool|float|double)\s+(\w+)\s*=\s*\d+`)
	configOptionRe  = regexp.MustCompile(`\(forge\.options\.v1\.config_field\)\s*=\s*\{`)
	optionKVRe      = regexp.MustCompile(`(\w+)\s*:\s*(?:"([^"]*)"|(true|false))`)
)

// ParseConfigProto parses a config proto file and extracts messages with
// ConfigFieldOptions annotations.
func ParseConfigProto(protoPath string) ([]ConfigMessage, error) {
	file, err := os.Open(protoPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	var messages []ConfigMessage
	var currentMsg *ConfigMessage
	var currentField *ConfigField
	var msgDepth int
	var inConfigOption bool
	var optionLines []string
	var inBlockComment bool

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		// Handle block comments
		if inBlockComment {
			if strings.Contains(trimmed, "*/") {
				idx := strings.Index(trimmed, "*/")
				trimmed = strings.TrimSpace(trimmed[idx+2:])
				inBlockComment = false
				if trimmed == "" {
					continue
				}
			} else {
				continue
			}
		}

		if strings.Contains(trimmed, "/*") {
			if strings.Contains(trimmed, "*/") {
				startIdx := strings.Index(trimmed, "/*")
				endIdx := strings.Index(trimmed, "*/")
				trimmed = strings.TrimSpace(trimmed[:startIdx] + trimmed[endIdx+2:])
				if trimmed == "" {
					continue
				}
			} else {
				startIdx := strings.Index(trimmed, "/*")
				trimmed = strings.TrimSpace(trimmed[:startIdx])
				inBlockComment = true
				if trimmed == "" {
					continue
				}
			}
		}

		// Skip line comments and empty lines
		if strings.HasPrefix(trimmed, "//") || trimmed == "" {
			continue
		}

		// Detect message start
		if matches := configMessageRe.FindStringSubmatch(trimmed); matches != nil && currentMsg == nil {
			currentMsg = &ConfigMessage{Name: matches[1]}
			msgDepth = 1
			continue
		}

		// Track brace depth inside a message
		if currentMsg != nil && !inConfigOption {
			msgDepth += strings.Count(trimmed, "{")
			msgDepth -= strings.Count(trimmed, "}")
			if msgDepth <= 0 {
				if len(currentMsg.Fields) > 0 {
					messages = append(messages, *currentMsg)
				}
				currentMsg = nil
				continue
			}
		}

		if currentMsg == nil {
			continue
		}

		// Detect field declaration (e.g., "int32 port = 1 [...")
		if matches := configFieldRe.FindStringSubmatch(trimmed); matches != nil {
			protoType := matches[1]
			fieldName := matches[2]
			currentField = &ConfigField{
				Name:      fieldName,
				GoName:    protoFieldToGoName(fieldName),
				GoType:    protoTypeToGoType(protoType),
				ProtoType: protoType,
			}

			// Check if config_field option starts on the same line
			if configOptionRe.MatchString(trimmed) {
				inConfigOption = true
				optionLines = []string{trimmed}
				// Check if option also closes on the same line
				if strings.Contains(trimmed, "}]") {
					inConfigOption = false
					parseConfigFieldOptions(currentField, optionLines)
					currentMsg.Fields = append(currentMsg.Fields, *currentField)
					currentField = nil
					optionLines = nil
				}
			}
			continue
		}

		// Detect config_field option start on a separate line
		if currentField != nil && configOptionRe.MatchString(trimmed) {
			inConfigOption = true
			optionLines = []string{trimmed}
			// Check if it closes on the same line
			if strings.Contains(trimmed, "}]") {
				inConfigOption = false
				parseConfigFieldOptions(currentField, optionLines)
				currentMsg.Fields = append(currentMsg.Fields, *currentField)
				currentField = nil
				optionLines = nil
			}
			continue
		}

		// Accumulate option lines
		if inConfigOption {
			optionLines = append(optionLines, trimmed)
			// Check for closing of the option block: }] or }];
			if strings.Contains(trimmed, "}]") || strings.Contains(trimmed, "};") {
				inConfigOption = false
				parseConfigFieldOptions(currentField, optionLines)
				currentMsg.Fields = append(currentMsg.Fields, *currentField)
				currentField = nil
				optionLines = nil
			}
			continue
		}

		// If we had a field without config_field, discard it
		if currentField != nil && !inConfigOption {
			currentField = nil
		}
	}

	// Add last message if still open
	if currentMsg != nil && len(currentMsg.Fields) > 0 {
		messages = append(messages, *currentMsg)
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return messages, nil
}

// ParseConfigProtosFromDir scans a directory for config proto files and parses all of them.
func ParseConfigProtosFromDir(dir string) ([]ConfigMessage, error) {
	var messages []ConfigMessage

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		msgs, err := ParseConfigProto(path)
		if err != nil {
			return fmt.Errorf("failed to parse %s: %w", path, err)
		}

		messages = append(messages, msgs...)
		return nil
	})

	return messages, err
}

// parseConfigFieldOptions extracts key-value pairs from config_field option lines.
func parseConfigFieldOptions(field *ConfigField, lines []string) {
	combined := strings.Join(lines, " ")
	matches := optionKVRe.FindAllStringSubmatch(combined, -1)

	for _, match := range matches {
		key := match[1]
		// Quoted string value is in group 2, bare bool is in group 3
		value := match[2]
		if value == "" {
			value = match[3]
		}

		switch key {
		case "env_var":
			field.EnvVar = value
		case "flag":
			field.Flag = value
		case "default_value":
			field.DefaultValue = value
		case "required":
			field.Required = value == "true"
		case "description":
			field.Description = value
		}
	}
}

// protoFieldToGoName converts a snake_case proto field name to PascalCase Go name,
// respecting Go initialisms.
// e.g., "database_url" -> "DatabaseURL", "port" -> "Port", "api_id" -> "APIID"
func protoFieldToGoName(name string) string {
	parts := strings.Split(name, "_")
	var result strings.Builder
	for _, part := range parts {
		if part == "" {
			continue
		}
		if naming.GoInitialismsMap[strings.ToLower(part)] {
			result.WriteString(strings.ToUpper(part))
		} else {
			result.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return result.String()
}

// protoTypeToGoType converts a proto type to its Go equivalent.
func protoTypeToGoType(protoType string) string {
	switch protoType {
	case "string":
		return "string"
	case "int32":
		return "int32"
	case "int64":
		return "int64"
	case "bool":
		return "bool"
	case "float":
		return "float32"
	case "double":
		return "float64"
	default:
		return "string"
	}
}