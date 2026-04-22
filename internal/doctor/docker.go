package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// composeService represents one service from `docker compose ps --format json`.
type composeService struct {
	Name       string             `json:"Name"`
	Service    string             `json:"Service"`
	State      string             `json:"State"`
	Health     string             `json:"Health"`
	Status     string             `json:"Status"`
	Publishers []composePublisher `json:"Publishers"`
}

type composePublisher struct {
	PublishedPort int    `json:"PublishedPort"`
	TargetPort    int    `json:"TargetPort"`
	URL           string `json:"URL"`
}

// portQuery defines a service/port pair we want to discover.
type portQuery struct {
	service string
	port    int
}

var defaultPortQueries = []portQuery{
	{"app", 8080},
	{"app", 6060},
	{"lgtm", 3000},
	{"postgres", 5432},
	{"app-debug", 2345},
	{"app-debug", 8080},
}

// CheckDocker verifies that Docker Compose services are running and discovers
// published ports for use by downstream checks.
func CheckDocker(ctx context.Context, env *Environment) CheckResult {
	// Run docker compose ps --format json in the project directory.
	cmd := exec.CommandContext(ctx, "docker", "compose", "ps", "--format", "json")
	cmd.Dir = env.ProjectDir
	out, err := cmd.Output()
	if err != nil {
		return CheckResult{
			Status:  StatusFail,
			Message: "Docker Compose is not running",
			Evidence: fmt.Sprintf(
				"Failed to run 'docker compose ps': %v\nHint: run 'docker compose up -d' in %s",
				err, env.ProjectDir,
			),
		}
	}

	output := strings.TrimSpace(string(out))
	if output == "" {
		return CheckResult{
			Status:   StatusFail,
			Message:  "No Docker Compose services found",
			Evidence: fmt.Sprintf("'docker compose ps' returned empty output in %s", env.ProjectDir),
		}
	}

	// Parse one JSON object per line.
	var services []composeService
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var svc composeService
		if err := json.Unmarshal([]byte(line), &svc); err != nil {
			continue
		}
		services = append(services, svc)
	}

	if len(services) == 0 {
		return CheckResult{
			Status:   StatusFail,
			Message:  "No Docker Compose services found",
			Evidence: "Could not parse any service from 'docker compose ps' output",
		}
	}

	// Tally service states.
	var healthy, unhealthy, running, total int
	var evidence strings.Builder
	for _, svc := range services {
		total++
		state := strings.ToLower(svc.State)
		health := strings.ToLower(svc.Health)

		switch {
		case health == "healthy":
			healthy++
		case health == "unhealthy":
			unhealthy++
		case state == "running":
			running++
		}

		fmt.Fprintf(&evidence, "  %-20s state=%-10s health=%-10s %s\n",
			svc.Service, svc.State, svc.Health, svc.Status)
	}

	// Discover ports.
	for _, pq := range defaultPortQueries {
		addr := discoverPort(ctx, env.ProjectDir, pq.service, pq.port)
		if addr != "" {
			env.SetPort(pq.service, pq.port, addr)
		}
	}

	// Determine overall status.
	status := StatusPass
	var msg string

	switch {
	case unhealthy > 0:
		status = StatusWarn
		msg = fmt.Sprintf("%d/%d services running (%d unhealthy)", healthy+running, total, unhealthy)
	default:
		msg = fmt.Sprintf("%d/%d services healthy/running", healthy+running, total)
	}

	return CheckResult{
		Status:   status,
		Message:  msg,
		Evidence: strings.TrimRight(evidence.String(), "\n"),
	}
}

// discoverPort runs `docker compose port <service> <port>` and returns the
// host address (e.g. "0.0.0.0:55010") or "" if the service/port is not available.
func discoverPort(ctx context.Context, projectDir, service string, port int) string {
	cmd := exec.CommandContext(ctx, "docker", "compose", "port", service, fmt.Sprintf("%d", port))
	cmd.Dir = projectDir

	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}
