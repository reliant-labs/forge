package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// RegisterBuilder adopts a named local builder without requiring a registry or
// cluster. Its endpoint is checked again before every maintenance pass.
func (r Runner) RegisterBuilder(ctx context.Context, path, builder string) error {
	if strings.TrimSpace(builder) == "" {
		return fmt.Errorf("builder name is required")
	}
	return WithLock(path, func() error {
		p, err := Load(path)
		if err != nil {
			return err
		}
		r.Policy = p
		if err := r.bindLocalContext(ctx); err != nil {
			return err
		}
		if err := r.localBuilder(ctx, builder); err != nil {
			return err
		}
		if !contains(r.Policy.Builders, builder) {
			r.Policy.Builders = append(r.Policy.Builders, builder)
		}
		return Save(path, r.Policy)
	})
}

func (r *Runner) bindLocalContext(ctx context.Context) error {
	if os.Getenv("DOCKER_HOST") != "" && os.Getenv("DOCKER_CONTEXT") == "" {
		return fmt.Errorf("storage registration requires a named Docker context instead of a DOCKER_HOST override")
	}
	b, err := r.command(ctx, "docker", "context", "show")
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(b))
	if current == "" {
		return fmt.Errorf("cannot establish current Docker context")
	}
	if r.Policy.DockerContext != "" && r.Policy.DockerContext != current {
		return fmt.Errorf("storage policy belongs to Docker context %s, current context is %s", r.Policy.DockerContext, current)
	}
	r.Policy.DockerContext = current
	return r.Local(ctx)
}

func (r Runner) localBuilder(ctx context.Context, builder string) error {
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
	if len(info.Nodes) == 0 {
		return fmt.Errorf("builder %s has no inspectable nodes", builder)
	}
	for _, n := range info.Nodes {
		if strings.TrimSpace(n.Endpoint) == "" {
			return fmt.Errorf("builder %s has an empty node endpoint", builder)
		}
		if strings.Contains(n.Endpoint, "://") {
			if !strings.HasPrefix(n.Endpoint, "unix://") && !strings.HasPrefix(n.Endpoint, "npipe://") {
				return fmt.Errorf("builder %s uses nonlocal endpoint %s", builder, n.Endpoint)
			}
		} else {
			probe := r
			probe.Policy.DockerContext = n.Endpoint
			if err := probe.Local(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r Runner) builderGC(ctx context.Context, builder string, apply bool) error {
	if err := r.localBuilder(ctx, builder); err != nil {
		return err
	}
	r.print("builder %s: evict unused cache older than %s toward %d GiB\n", builder, r.Policy.BuildCacheUnused, r.Policy.BuildCacheGiB)
	if !apply {
		return nil
	}
	b, err := r.docker(ctx, "buildx", "prune", "--builder", builder, "--force", "--max-used-space", fmt.Sprintf("%dB", r.Policy.BuildCacheGiB*GiB), "--filter", "until="+r.Policy.BuildCacheUnused)
	if err == nil {
		r.print("%s\n", b)
	}
	return err
}
