package tools

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

// getListProtosTool returns the list_protos tool definition
func getListProtosTool() Tool {
	return Tool{
		Name: "list_protos",
		Description: `List all .proto files in the project with their package names, service count, and message count.

Scans the proto/ directory for all .proto files and extracts key information.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeListProtos(arguments json.RawMessage) (string, error) {
	var protoFiles []protoFileInfo

	err := filepath.Walk("proto", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		pfi, err := parseProtoFileInfo(path)
		if err != nil {
			pfi = &protoFileInfo{Path: path, Error: err.Error()}
		}
		protoFiles = append(protoFiles, *pfi)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to walk proto directory: %w", err)
	}

	if len(protoFiles) == 0 {
		return "No .proto files found in proto/ directory.\n", nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Found %d .proto file(s):\n\n", len(protoFiles)))

	for _, pf := range protoFiles {
		result.WriteString(fmt.Sprintf("  %s\n", pf.Path))
		if pf.Error != "" {
			result.WriteString(fmt.Sprintf("    Error: %s\n", pf.Error))
			continue
		}
		result.WriteString(fmt.Sprintf("    Package: %s\n", pf.Package))
		result.WriteString(fmt.Sprintf("    Services: %d\n", pf.ServiceCount))
		result.WriteString(fmt.Sprintf("    Messages: %d\n", pf.MessageCount))
		result.WriteString("\n")
	}

	return result.String(), nil
}

type protoFileInfo struct {
	Path         string
	Package      string
	ServiceCount int
	MessageCount int
	Error        string
}

func parseProtoFileInfo(path string) (*protoFileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info := &protoFileInfo{Path: path}
	scanner := bufio.NewScanner(file)

	packageRe := regexp.MustCompile(`^package\s+([^;]+);`)
	serviceRe := regexp.MustCompile(`^service\s+\w+`)
	messageRe := regexp.MustCompile(`^message\s+\w+`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if m := packageRe.FindStringSubmatch(line); m != nil {
			info.Package = m[1]
		}
		if serviceRe.MatchString(line) {
			info.ServiceCount++
		}
		if messageRe.MatchString(line) {
			info.MessageCount++
		}
	}

	return info, scanner.Err()
}

// getListServicesTool returns the list_services tool definition
func getListServicesTool() Tool {
	return Tool{
		Name: "list_services",
		Description: `List all services defined in the project's proto files with their RPC methods.

Scans proto/ for service definitions and extracts method signatures.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeListServices(arguments json.RawMessage) (string, error) {
	var services []serviceInfo

	err := filepath.Walk("proto", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		svcs, err := parseServicesFromProto(path)
		if err != nil {
			return nil // skip files we can't parse
		}
		services = append(services, svcs...)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to walk proto directory: %w", err)
	}

	if len(services) == 0 {
		return "No services found in proto/ directory.\n", nil
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Found %d service(s):\n\n", len(services)))

	for _, svc := range services {
		result.WriteString(fmt.Sprintf("  Service: %s\n", svc.Name))
		result.WriteString(fmt.Sprintf("  File: %s\n", svc.File))
		result.WriteString(fmt.Sprintf("  Package: %s\n", svc.Package))
		if len(svc.Methods) > 0 {
			result.WriteString("  Methods:\n")
			for _, m := range svc.Methods {
				result.WriteString(fmt.Sprintf("    - %s\n", m))
			}
		}
		result.WriteString("\n")
	}

	return result.String(), nil
}

type serviceInfo struct {
	Name    string
	File    string
	Package string
	Methods []string
}

func parseServicesFromProto(path string) ([]serviceInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	content := string(data)
	var services []serviceInfo

	// Extract package
	packageRe := regexp.MustCompile(`package\s+([^;]+);`)
	pkgMatch := packageRe.FindStringSubmatch(content)
	pkg := ""
	if pkgMatch != nil {
		pkg = pkgMatch[1]
	}

	// Find service blocks with brace-balanced extraction to handle multiline bodies
	serviceStartRe := regexp.MustCompile(`service\s+(\w+)\s*\{`)
	rpcRe := regexp.MustCompile(`rpc\s+(\w+)\s*\(([^)]*)\)\s*returns\s*\(([^)]*)\)`)

	for _, loc := range serviceStartRe.FindAllStringSubmatchIndex(content, -1) {
		serviceName := content[loc[2]:loc[3]]
		openBrace := loc[1] - 1 // position of the opening brace

		// Extract the service body by counting braces
		body := extractBraceBlock(content, openBrace)
		if body == "" {
			continue
		}

		svc := serviceInfo{
			Name:    serviceName,
			File:    path,
			Package: pkg,
		}

		for _, rpcMatch := range rpcRe.FindAllStringSubmatch(body, -1) {
			methodSig := fmt.Sprintf("%s(%s) returns (%s)",
				rpcMatch[1],
				strings.TrimSpace(rpcMatch[2]),
				strings.TrimSpace(rpcMatch[3]))
			svc.Methods = append(svc.Methods, methodSig)
		}

		services = append(services, svc)
	}

	return services, nil
}

// extractBraceBlock extracts the content between matched braces starting at pos.
// pos must point to the opening '{'. Returns the content between braces (exclusive).
func extractBraceBlock(s string, pos int) string {
	if pos >= len(s) || s[pos] != '{' {
		return ""
	}
	depth := 0
	for i := pos; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[pos+1 : i]
			}
		}
	}
	return ""
}

// getGetServiceContractTool returns the get_service_contract tool definition
func getGetServiceContractTool() Tool {
	return Tool{
		Name: "get_service_contract",
		Description: `Return the full proto definition for a specific service.

Finds the proto file containing the named service and returns its complete contents.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"service_name": map[string]interface{}{
					"type":        "string",
					"description": "Name of the service to look up (e.g., UserService, OrderService)",
				},
			},
			"required": []string{"service_name"},
		},
	}
}

func executeGetServiceContract(arguments json.RawMessage) (string, error) {
	var args struct {
		ServiceName string `json:"service_name"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	serviceRe := regexp.MustCompile(fmt.Sprintf(`service\s+%s\s*\{`, regexp.QuoteMeta(args.ServiceName)))

	var foundPath string
	err := filepath.Walk("proto", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() || !strings.HasSuffix(path, ".proto") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}

		if serviceRe.Match(data) {
			foundPath = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("failed to search proto files: %w", err)
	}

	if foundPath == "" {
		return fmt.Sprintf("Service '%s' not found in any proto file under proto/.\n", args.ServiceName), nil
	}

	data, err := os.ReadFile(foundPath)
	if err != nil {
		return "", fmt.Errorf("failed to read proto file: %w", err)
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Service: %s\n", args.ServiceName))
	result.WriteString(fmt.Sprintf("File: %s\n", foundPath))
	result.WriteString(strings.Repeat("=", 60))
	result.WriteString("\n\n")
	result.WriteString(string(data))

	return result.String(), nil
}

// getGetProjectConfigTool returns the get_project_config tool definition
func getGetProjectConfigTool() Tool {
	return Tool{
		Name: "get_project_config",
		Description: `Return the parsed project configuration as JSON.

Reads forge.yaml from the project root and returns it as formatted JSON.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{},
		},
	}
}

func executeGetProjectConfig(arguments json.RawMessage) (string, error) {
	// Try common config file names
	configFiles := []string{
		"forge.yaml",
		"forge.yml",
		"forge.project.yml",
	}

	var configPath string
	var configData []byte
	var readErr error

	for _, name := range configFiles {
		configData, readErr = os.ReadFile(name)
		if readErr == nil {
			configPath = name
			break
		}
	}

	if configPath == "" {
		return "No forge configuration file found. Looked for: " + strings.Join(configFiles, ", ") + "\n", nil
	}

	// Parse YAML
	var parsed interface{}
	if err := yaml.Unmarshal(configData, &parsed); err != nil {
		return "", fmt.Errorf("failed to parse %s: %w", configPath, err)
	}

	// Convert to JSON for structured output
	jsonData, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal config as JSON: %w", err)
	}

	var result strings.Builder
	result.WriteString(fmt.Sprintf("Config file: %s\n", configPath))
	result.WriteString(strings.Repeat("=", 60))
	result.WriteString("\n\n")
	result.WriteString(string(jsonData))
	result.WriteString("\n")

	return result.String(), nil
}