// Package doctor provides health checks for the forge development stack.
//
// It validates that all services (Docker containers, telemetry backends,
// debugger) are running and correctly connected.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Status represents the outcome of a single health check.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusWarn Status = "warn"
	StatusSkip Status = "skip"
)

// CheckResult is the outcome of a single health check.
type CheckResult struct {
	Name     string        `json:"name"`
	Status   Status        `json:"status"`
	Message  string        `json:"message"`
	Evidence string        `json:"evidence,omitempty"`
	Duration time.Duration `json:"duration_ms"`
}

// MarshalJSON customises the JSON output so duration is in milliseconds.
func (r CheckResult) MarshalJSON() ([]byte, error) {
	type alias CheckResult
	return json.Marshal(struct {
		alias
		Duration int64 `json:"duration_ms"`
	}{
		alias:    alias(r),
		Duration: r.Duration.Milliseconds(),
	})
}

// Report is the aggregate result of all checks.
type Report struct {
	Overall  Status        `json:"overall"`
	Duration time.Duration `json:"-"`
	Checks   []CheckResult `json:"checks"`
}

// MarshalJSON customises the JSON output so duration is in milliseconds.
func (r Report) MarshalJSON() ([]byte, error) {
	type alias Report
	return json.Marshal(struct {
		alias
		Duration int64 `json:"duration_ms"`
	}{
		alias:    alias(r),
		Duration: r.Duration.Milliseconds(),
	})
}

// CheckFunc is a function that performs a single health check.
// It receives the shared Environment (discovered ports, project config)
// and returns a result.
type CheckFunc func(ctx context.Context, env *Environment) CheckResult

// Environment holds runtime information discovered during checks,
// shared across all check functions.
type Environment struct {
	ProjectName string
	ProjectDir  string // directory containing docker-compose.yml

	mu    sync.RWMutex
	Ports map[string]string // "app:8080" -> "0.0.0.0:55010"
}

// SetPort stores a discovered host:port mapping.
func (e *Environment) SetPort(service string, containerPort int, hostAddr string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Ports == nil {
		e.Ports = make(map[string]string)
	}
	e.Ports[fmt.Sprintf("%s:%d", service, containerPort)] = hostAddr
}

// GetPort retrieves a discovered host address for a service port.
func (e *Environment) GetPort(service string, containerPort int) (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	addr, ok := e.Ports[fmt.Sprintf("%s:%d", service, containerPort)]
	return addr, ok
}

// Doctor orchestrates running health checks.
type Doctor struct {
	env    *Environment
	checks []namedCheck
}

type namedCheck struct {
	name string
	fn   CheckFunc
}

// New creates a Doctor for the given project.
func New(projectName, projectDir string) *Doctor {
	return &Doctor{
		env: &Environment{
			ProjectName: projectName,
			ProjectDir:  projectDir,
			Ports:       make(map[string]string),
		},
	}
}

// Register adds a check to the doctor.
func (d *Doctor) Register(name string, fn CheckFunc) {
	d.checks = append(d.checks, namedCheck{name: name, fn: fn})
}

// RunSequential runs checks that must complete before parallel checks
// (e.g., Docker check discovers ports needed by other checks).
// RunParallel runs remaining checks concurrently.
func (d *Doctor) Run(ctx context.Context, sequential []string) Report {
	start := time.Now()

	seqSet := make(map[string]bool, len(sequential))
	for _, name := range sequential {
		seqSet[name] = true
	}

	var results []CheckResult

	// Phase 1: sequential checks (order preserved).
	for _, c := range d.checks {
		if !seqSet[c.name] {
			continue
		}
		r := d.runCheck(ctx, c)
		results = append(results, r)
		// If a sequential check fails, still continue — other checks
		// will gracefully degrade when ports are missing.
	}

	// Phase 2: parallel checks.
	var parallel []namedCheck
	for _, c := range d.checks {
		if seqSet[c.name] {
			continue
		}
		parallel = append(parallel, c)
	}

	if len(parallel) > 0 {
		var mu sync.Mutex
		var wg sync.WaitGroup
		wg.Add(len(parallel))
		parallelResults := make([]CheckResult, len(parallel))
		for i, c := range parallel {
			go func(idx int, check namedCheck) {
				defer wg.Done()
				r := d.runCheck(ctx, check)
				mu.Lock()
				parallelResults[idx] = r
				mu.Unlock()
			}(i, c)
		}
		wg.Wait()
		results = append(results, parallelResults...)
	}

	report := Report{
		Overall:  StatusPass,
		Duration: time.Since(start),
		Checks:   results,
	}

	for _, r := range results {
		if r.Status == StatusFail {
			report.Overall = StatusFail
			break
		}
		if r.Status == StatusWarn && report.Overall == StatusPass {
			report.Overall = StatusWarn
		}
	}

	return report
}

func (d *Doctor) runCheck(ctx context.Context, c namedCheck) CheckResult {
	start := time.Now()
	r := c.fn(ctx, d.env)
	r.Name = c.name
	r.Duration = time.Since(start)
	return r
}

// ANSI helpers.
const (
	colorGreen  = "\033[32m"
	colorRed    = "\033[31m"
	colorYellow = "\033[33m"
	colorGray   = "\033[90m"
	colorReset  = "\033[0m"
)

func statusIcon(s Status) string {
	switch s {
	case StatusPass:
		return colorGreen + "✓" + colorReset
	case StatusFail:
		return colorRed + "✗" + colorReset
	case StatusWarn:
		return colorYellow + "!" + colorReset
	case StatusSkip:
		return colorGray + "–" + colorReset
	default:
		return "?"
	}
}

// PrintReport writes a human-readable report to w.
func PrintReport(w io.Writer, report Report, verbose bool) {
	for _, r := range report.Checks {
		icon := statusIcon(r.Status)
		name := fmt.Sprintf("%-20s", r.Name)
		fmt.Fprintf(w, "  %s %s %s", icon, name, r.Message)
		if r.Duration > 0 {
			fmt.Fprintf(w, "  %s(%s)%s", colorGray, r.Duration.Round(time.Millisecond), colorReset)
		}
		fmt.Fprintln(w)

		if verbose && r.Evidence != "" {
			for _, line := range strings.Split(r.Evidence, "\n") {
				fmt.Fprintf(w, "    %s%s%s\n", colorGray, line, colorReset)
			}
		}
	}
	fmt.Fprintln(w)

	switch report.Overall {
	case StatusPass:
		fmt.Fprintf(w, "  %s All checks passed %s(%s)%s\n", colorGreen+"✓"+colorReset, colorGray, report.Duration.Round(time.Millisecond), colorReset)
	case StatusWarn:
		fmt.Fprintf(w, "  %s Some checks have warnings %s(%s)%s\n", colorYellow+"!"+colorReset, colorGray, report.Duration.Round(time.Millisecond), colorReset)
	case StatusFail:
		var failures int
		for _, r := range report.Checks {
			if r.Status == StatusFail {
				failures++
			}
		}
		fmt.Fprintf(w, "  %s %d check(s) failed %s(%s)%s\n", colorRed+"✗"+colorReset, failures, colorGray, report.Duration.Round(time.Millisecond), colorReset)
	}
}

// PrintJSON writes the report as JSON to w.
func PrintJSON(w io.Writer, report Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
