package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner executes maintenance using injectable process operations.
type Runner struct {
	Policy  Policy
	Out     io.Writer
	Command func(context.Context, string, ...string) ([]byte, error)
}

// Exec runs a command with a bounded lifetime.
func Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	b, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%s %v: %w: %s", name, args, err, strings.TrimSpace(string(b)))
	}
	return b, nil
}

func (r Runner) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	if r.Command != nil {
		return r.Command(ctx, name, args...)
	}
	return Exec(ctx, name, args...)
}
func (r Runner) docker(ctx context.Context, args ...string) ([]byte, error) {
	if r.Policy.DockerContext != "" {
		args = append([]string{"--context", r.Policy.DockerContext}, args...)
	}
	return r.command(ctx, "docker", args...)
}
func (r Runner) print(format string, args ...any) {
	if r.Out != nil {
		fmt.Fprintf(r.Out, format, args...)
	}
}

// Local refuses remote Docker endpoints.
func (r Runner) Local(ctx context.Context) error {
	b, err := r.docker(ctx, "context", "inspect")
	if err != nil {
		return err
	}
	var contexts []struct {
		Endpoints map[string]struct{ Host string }
	}
	if err := json.Unmarshal(b, &contexts); err != nil {
		return err
	}
	if len(contexts) != 1 {
		return fmt.Errorf("cannot establish Docker endpoint")
	}
	host := contexts[0].Endpoints["docker"].Host
	if !strings.HasPrefix(host, "unix://") && !strings.HasPrefix(host, "npipe://") {
		return fmt.Errorf("storage maintenance refuses nonlocal Docker endpoint %q", host)
	}
	return nil
}

// Status reports host, Docker, builder and kubelet storage usage.
func (r Runner) Status(ctx context.Context) error {
	disks, spaceErr := CheckSpace(r.Policy)
	for _, d := range disks {
		r.print("host %s: %.1f GiB available / %.1f GiB capacity\n", d.Path, float64(d.Available)/float64(GiB), float64(d.Capacity)/float64(GiB))
	}
	if spaceErr != nil {
		r.print("pressure: %v\n", spaceErr)
	}
	if err := r.Local(ctx); err != nil {
		return err
	}
	b, err := r.docker(ctx, "system", "df")
	if err != nil {
		return err
	}
	r.print("%s\n", b)
	for _, builder := range r.Policy.Builders {
		b, err = r.docker(ctx, "buildx", "du", "--builder", builder)
		if err != nil {
			return err
		}
		r.print("builder %s (budget %d GiB):\n%s\n", builder, r.Policy.BuildCacheGiB, b)
	}
	for _, cluster := range r.Policy.Clusters {
		b, err = r.command(ctx, "kubectl", "--context", cluster, "--request-timeout=15s", "get", "nodes", "-o", "json")
		if err != nil {
			return err
		}
		var nodes struct {
			Items []struct{ Metadata struct{ Name string } }
		}
		if err := json.Unmarshal(b, &nodes); err != nil {
			return err
		}
		for _, node := range nodes.Items {
			b, err = r.command(ctx, "kubectl", "--context", cluster, "--request-timeout=15s", "get", "--raw", "/api/v1/nodes/"+node.Metadata.Name+"/proxy/configz")
			if err != nil {
				return err
			}
			r.print("node %s GC configuration: %s\n", node.Metadata.Name, b)
		}
	}
	return nil
}

// WithLock serializes policy updates and maintenance on this machine.
func WithLock(policyPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(policyPath), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(policyPath+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := lock(f); err != nil {
		return fmt.Errorf("storage maintenance is already running: %w", err)
	}
	return fn()
}

// GC previews or applies the registered cache and registry policy.
func (r Runner) GC(ctx context.Context, apply bool) error {
	if err := r.Policy.Validate(); err != nil {
		return err
	}
	if err := r.Logs(apply); err != nil {
		return err
	}
	if err := r.Local(ctx); err != nil {
		return err
	}
	for _, builder := range r.Policy.Builders {
		// A local Docker context can still select a remote buildx builder.
		b, err := r.docker(ctx, "buildx", "inspect", builder, "--format", "{{json .}}")
		if err != nil {
			return err
		}
		var info struct {
			Driver string
			Nodes  []struct{ Endpoint string }
		}
		if err := json.Unmarshal(b, &info); err != nil {
			return err
		}
		if info.Driver != "docker" && info.Driver != "docker-container" {
			return fmt.Errorf("builder %s is not a local Docker builder", builder)
		}
		for _, n := range info.Nodes {
			if strings.Contains(n.Endpoint, "://") && !strings.HasPrefix(n.Endpoint, "unix://") && !strings.HasPrefix(n.Endpoint, "npipe://") {
				return fmt.Errorf("builder %s uses nonlocal endpoint %s", builder, n.Endpoint)
			}
			if !strings.Contains(n.Endpoint, "://") {
				probe := r
				probe.Policy.DockerContext = n.Endpoint
				if err := probe.Local(ctx); err != nil {
					return err
				}
			}
		}
		r.print("builder %s: evict unused cache older than %s toward %d GiB\n", builder, r.Policy.BuildCacheUnused, r.Policy.BuildCacheGiB)
		if apply {
			b, err = r.docker(ctx, "buildx", "prune", "--builder", builder, "--force", "--max-used-space", fmt.Sprintf("%dB", r.Policy.BuildCacheGiB*GiB), "--filter", "until="+r.Policy.BuildCacheUnused)
			if err != nil {
				return err
			}
			r.print("%s\n", b)
		}
	}
	for _, registry := range r.Policy.Registries {
		if err := r.RegistryGC(ctx, registry, apply); err != nil {
			return err
		}
	}
	r.print("persistent volumes, worktrees, running containers and application data are retained\n")
	return nil
}

// NodeConfigPath is stable across command exits; k3d bind mounts must never
// refer to a temporary file removed at the end of cluster creation.
func NodeConfigPath(policyPath string, p Policy) (string, error) {
	path := filepath.Join(filepath.Dir(policyPath), "kubelet-storage.conf")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, p.KubeletConfig(), 0644)
}
