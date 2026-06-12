package cli

import (
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/config"
)

// lookupFromMap builds an os.LookupEnv-shaped function from a fixed map
// so the ENVIRONMENT-default tests never touch the real process env
// (no t.Setenv — these tests are parallel-safe).
func lookupFromMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// defaultRunEnvironment decides whether runProjectDev injects
// ENVIRONMENT=development into the server child's env. The default must
// fire ONLY when nothing else declared ENVIRONMENT (per-env config or
// the shell) and the user is running the default env ("dev").
func TestDefaultRunEnvironment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		envExtraEnv map[string]string
		shell       map[string]string
		env         string
		wantVal     string
		wantOK      bool
	}{
		{
			name:        "absent everywhere, env=dev: default to development",
			envExtraEnv: map[string]string{},
			shell:       map[string]string{},
			env:         "dev",
			wantVal:     "development",
			wantOK:      true,
		},
		{
			name:        "per-env config declares environment: config wins",
			envExtraEnv: map[string]string{"ENVIRONMENT": "staging"},
			shell:       map[string]string{},
			env:         "dev",
			wantOK:      false,
		},
		{
			name:        "shell declares ENVIRONMENT: explicit env wins",
			envExtraEnv: map[string]string{},
			shell:       map[string]string{"ENVIRONMENT": "production"},
			env:         "dev",
			wantOK:      false,
		},
		{
			name:        "shell declares ENVIRONMENT empty: still explicit, no default",
			envExtraEnv: map[string]string{},
			shell:       map[string]string{"ENVIRONMENT": ""},
			env:         "dev",
			wantOK:      false,
		},
		{
			name:        "non-dev --env: leave it to per-env config",
			envExtraEnv: map[string]string{},
			shell:       map[string]string{},
			env:         "staging",
			wantOK:      false,
		},
		{
			name:        "unrelated keys present everywhere: still defaults",
			envExtraEnv: map[string]string{"LOG_LEVEL": "debug"},
			shell:       map[string]string{"PATH": "/usr/bin"},
			env:         "dev",
			wantVal:     "development",
			wantOK:      true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := defaultRunEnvironment(tt.envExtraEnv, lookupFromMap(tt.shell), tt.env)
			if ok != tt.wantOK {
				t.Fatalf("defaultRunEnvironment ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantVal {
				t.Fatalf("defaultRunEnvironment = %q, want %q", got, tt.wantVal)
			}
		})
	}
}

// composeDevCORSOrigins builds the comma-separated dev default for
// CORS_ORIGINS (the format the generated config loader splits on ",").
// Per-frontend loopback origins first, then the dev-proxy hostnames.
func TestComposeDevCORSOrigins(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		frontends []config.FrontendConfig
		proxyPort int
		noProxy   bool
		want      string
	}{
		{
			name:      "no frontends: no dev default",
			frontends: nil,
			proxyPort: 8080,
			want:      "",
		},
		{
			name:      "single frontend with proxy",
			frontends: []config.FrontendConfig{{Name: "web", Port: 3000}},
			proxyPort: 8080,
			want:      "http://localhost:3000,http://web.localhost:8080,http://localhost:8080",
		},
		{
			name: "two frontends with proxy",
			frontends: []config.FrontendConfig{
				{Name: "web", Port: 3000},
				{Name: "admin", Port: 3001},
			},
			proxyPort: 8080,
			want:      "http://localhost:3000,http://localhost:3001,http://web.localhost:8080,http://admin.localhost:8080,http://localhost:8080",
		},
		{
			name:      "--no-proxy skips proxy origins",
			frontends: []config.FrontendConfig{{Name: "web", Port: 3000}},
			proxyPort: 8080,
			noProxy:   true,
			want:      "http://localhost:3000",
		},
		{
			name:      "portless frontend contributes nothing",
			frontends: []config.FrontendConfig{{Name: "web", Port: 0}},
			proxyPort: 8080,
			want:      "",
		},
		{
			name: "frontend port equal to proxy port deduplicates",
			frontends: []config.FrontendConfig{
				{Name: "web", Port: 8080},
			},
			proxyPort: 8080,
			want:      "http://localhost:8080,http://web.localhost:8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := composeDevCORSOrigins(tt.frontends, tt.proxyPort, tt.noProxy)
			if got != tt.want {
				t.Fatalf("composeDevCORSOrigins = %q, want %q", got, tt.want)
			}
		})
	}
}

// frontendDevEnv composes the env for a frontend child: PORT and
// NEXT_PUBLIC_BASE_PATH are force-injected from forge.yaml (the source
// of truth for dev/prod parity) and must override stale parent values.
func TestFrontendDevEnv(t *testing.T) {
	t.Parallel()
	base := []string{"PATH=/usr/bin", "PORT=9999", "NEXT_PUBLIC_BASE_PATH=/stale"}
	tests := []struct {
		name        string
		fe          config.FrontendConfig
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "port forces PORT",
			fe:          config.FrontendConfig{Name: "web", Port: 3000},
			wantPresent: []string{"PORT=3000"},
			wantAbsent:  []string{"PORT=9999"},
		},
		{
			name:        "base_path forces NEXT_PUBLIC_BASE_PATH",
			fe:          config.FrontendConfig{Name: "web", BasePath: "/app"},
			wantPresent: []string{"NEXT_PUBLIC_BASE_PATH=/app"},
			wantAbsent:  []string{"NEXT_PUBLIC_BASE_PATH=/stale"},
		},
		{
			name:        "port and base_path compose",
			fe:          config.FrontendConfig{Name: "web", Port: 3000, BasePath: "/app"},
			wantPresent: []string{"PORT=3000", "NEXT_PUBLIC_BASE_PATH=/app"},
			wantAbsent:  []string{"PORT=9999", "NEXT_PUBLIC_BASE_PATH=/stale"},
		},
		{
			name:        "neither declared: base passes through untouched",
			fe:          config.FrontendConfig{Name: "web"},
			wantPresent: []string{"PATH=/usr/bin", "PORT=9999", "NEXT_PUBLIC_BASE_PATH=/stale"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := frontendDevEnv(base, tt.fe)
			env := strings.Join(got, "\n")
			for _, want := range tt.wantPresent {
				if !containsEnvEntry(got, want) {
					t.Errorf("frontendDevEnv missing %q:\n%s", want, env)
				}
			}
			for _, absent := range tt.wantAbsent {
				if containsEnvEntry(got, absent) {
					t.Errorf("frontendDevEnv must not keep %q:\n%s", absent, env)
				}
			}
		})
	}
}

func containsEnvEntry(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

// diagnosePortConflict must name the held port when something is bound
// to it, stay silent for a free port, and skip unknown ports (<= 0).
func TestDiagnosePortConflict(t *testing.T) {
	t.Parallel()

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	msg := diagnosePortConflict(port)
	if msg == "" {
		t.Fatalf("diagnosePortConflict(%d) = %q, want a conflict message while the port is held", port, msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("port %d is already in use", port)) {
		t.Fatalf("diagnosePortConflict(%d) = %q, want it to name the port", port, msg)
	}

	_ = ln.Close()
	if msg := diagnosePortConflict(port); msg != "" {
		t.Fatalf("diagnosePortConflict(%d) after release = %q, want empty", port, msg)
	}

	if msg := diagnosePortConflict(0); msg != "" {
		t.Fatalf("diagnosePortConflict(0) = %q, want empty for unknown port", msg)
	}
}

// describeChildExit / childExitError shape the loud failure surfaced
// when a child dies before shutdown was requested.
func TestDescribeChildExitAndError(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sh", "-c", "exit 3")
	waitErr := cmd.Run() // non-nil: *exec.ExitError with status 3

	desc := describeChildExit("web", waitErr)
	if !strings.Contains(desc, `"web"`) || !strings.Contains(desc, "exit status 3") {
		t.Fatalf("describeChildExit = %q, want it to name the process and exit status", desc)
	}

	descClean := describeChildExit("web", nil)
	if !strings.Contains(descClean, `"web"`) || !strings.Contains(descClean, "status 0") {
		t.Fatalf("describeChildExit(nil) = %q, want process name and status 0", descClean)
	}

	if err := childExitError("web", waitErr); err == nil {
		t.Fatal("childExitError must be non-nil so forge run exits nonzero")
	} else if !strings.Contains(err.Error(), "web") {
		t.Fatalf("childExitError = %v, want it to name the process", err)
	}
	if err := childExitError("web", nil); err == nil {
		t.Fatal("childExitError(nil exitErr) must still be non-nil — a clean exit before shutdown is still a dead dev server")
	}
}

// superviseChild owns the single cmd.Wait for a managed child: it must
// record the exit on the managedProcess, close done, and notify exitCh.
func TestSuperviseChild_ReportsExit(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sh", "-c", "exit 3")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	p := &managedProcess{name: "web", cmd: cmd, done: make(chan struct{})}
	exitCh := make(chan *managedProcess, 1)

	var noStreams sync.WaitGroup
	superviseChild(p, &noStreams, exitCh)

	select {
	case got := <-exitCh:
		if got != p {
			t.Fatalf("exitCh delivered %v, want the supervised process", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superviseChild never notified exitCh for a dead child")
	}

	select {
	case <-p.done:
	default:
		t.Fatal("p.done must be closed once the child has been reaped")
	}

	ee, ok := p.exitErr.(*exec.ExitError)
	if !ok || ee.ExitCode() != 3 {
		t.Fatalf("p.exitErr = %v, want *exec.ExitError with code 3", p.exitErr)
	}
}
