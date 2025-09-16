package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// GetTaskfileTool returns the taskfile tool definition
func GetTaskfileTool() Tool {
	return Tool{
		Name: "taskfile",
		Description: `Run Taskfile.yml tasks safely without Bash access. This is the primary way to build, test, lint, and run Forge projects.

Available tasks include:
- build: Build the forge binary
- test: Run all tests with coverage
- test-short: Run short tests
- lint: Run linters (golangci-lint, buf lint, proto method enforcement)
- proto: Generate code from proto files
- run: Run forge locally
- dev: Run with hot reload
- clean: Remove build artifacts
- fmt: Format code
- check: Run lint + test + build
- ci: Full CI pipeline

This tool prevents arbitrary command execution while allowing safe, predefined operations.`,
		InputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"task": map[string]interface{}{
					"type":        "string",
					"description": "Task name to execute (e.g., 'build', 'test', 'lint')",
				},
				"args": map[string]interface{}{
					"type":        "array",
					"description": "Additional arguments to pass to the task",
					"items": map[string]interface{}{
						"type": "string",
					},
				},
				"verbose": map[string]interface{}{
					"type":        "boolean",
					"description": "Show verbose output",
					"default":     false,
				},
			},
			"required": []string{"task"},
		},
	}
}

// allowedTasks is the set of task names that may be executed.
var allowedTasks = map[string]bool{
	"build":      true,
	"test":       true,
	"test-short": true,
	"lint":       true,
	"proto":      true,
	"run":        true,
	"dev":        true,
	"clean":      true,
	"fmt":        true,
	"check":      true,
	"ci":         true,
}

// shellMetachars contains characters that could enable shell injection.
const shellMetachars = ";|&$`\\\"'(){}[]<>!#~\n\r"

// allowedTaskNames returns a sorted list of allowed task names.
func allowedTaskNames() []string {
	names := make([]string, 0, len(allowedTasks))
	for k := range allowedTasks {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// validateTaskArgs checks that no argument contains shell metacharacters.
func validateTaskArgs(args []string) error {
	for _, arg := range args {
		if strings.ContainsAny(arg, shellMetachars) {
			return fmt.Errorf("argument %q contains forbidden shell metacharacters", arg)
		}
	}
	return nil
}

func executeTaskfile(arguments json.RawMessage) (string, error) {
	var args struct {
		Task    string   `json:"task"`
		Args    []string `json:"args"`
		Verbose bool     `json:"verbose"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// Validate task name against allowlist.
	if !allowedTasks[args.Task] {
		return "", fmt.Errorf("task %q is not in the allowed list: %v", args.Task, allowedTaskNames())
	}

	// Reject arguments containing shell metacharacters.
	if err := validateTaskArgs(args.Args); err != nil {
		return "", err
	}

	// Build task command
	cmdArgs := []string{args.Task}
	if args.Verbose {
		cmdArgs = append(cmdArgs, "--verbose")
	}
	cmdArgs = append(cmdArgs, args.Args...)

	cmd := exec.Command("task", cmdArgs...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	// Combine output
	var result strings.Builder
	result.WriteString(fmt.Sprintf("Task: %s\n", args.Task))
	result.WriteString(strings.Repeat("=", 60))
	result.WriteString("\n\n")

	if stdout.Len() > 0 {
		result.WriteString("STDOUT:\n")
		result.WriteString(stdout.String())
		result.WriteString("\n")
	}

	if stderr.Len() > 0 {
		result.WriteString("STDERR:\n")
		result.WriteString(stderr.String())
		result.WriteString("\n")
	}

	if err != nil {
		result.WriteString(fmt.Sprintf("\nTask failed with error: %v\n", err))
		return result.String(), nil // Return error in output, not as error
	}

	result.WriteString("\nTask completed successfully\n")
	return result.String(), nil
}