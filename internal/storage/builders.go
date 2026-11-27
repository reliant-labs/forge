package storage

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
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
		if _, err := r.localBuilder(ctx, builder); err != nil {
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

// builderInfo is what `docker buildx inspect <name>` reports about a builder.
type builderInfo struct {
	Driver    string
	Endpoints []string // one per node, in the order buildx lists them
}

// parseBuildxInspect reads buildx's human output. It is the ONLY output: buildx
// has no machine format for inspect (v0.37 rejects `--format` as an unknown
// flag), so the stable `Key: value` lines are the contract. The top-level
// Driver comes before the `Nodes:` section; every `Endpoint:` after it belongs
// to a node. A missing Driver is an error rather than an empty string so an
// unrecognised layout cannot pass the local-driver check by omission.
func parseBuildxInspect(out []byte) (builderInfo, error) {
	var info builderInfo
	inNodes := false
	for _, line := range strings.Split(string(out), "\n") {
		// Only column-0 keys are structure. Nested sections (Labels:,
		// Devices:, GC Policy rules) are indented, and Devices carries its own
		// ` Name: docker.com/gpu=webgpu` lines that must not read as nodes.
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch {
		case key == "Nodes" && value == "":
			inNodes = true
		case key == "Driver" && !inNodes && info.Driver == "":
			info.Driver = value
		case key == "Name" && inNodes:
			// Every node block opens with Name. Record the node now with an
			// empty endpoint, so a node that never prints an Endpoint still
			// counts — and fails the empty-endpoint check — instead of
			// silently vanishing.
			info.Endpoints = append(info.Endpoints, "")
		case key == "Endpoint" && inNodes && len(info.Endpoints) > 0:
			info.Endpoints[len(info.Endpoints)-1] = value
		}
	}
	if info.Driver == "" {
		return info, fmt.Errorf("unrecognised `docker buildx inspect` output: no Driver line")
	}
	return info, nil
}

func (r Runner) localBuilder(ctx context.Context, builder string) (builderInfo, error) {
	b, err := r.docker(ctx, "buildx", "inspect", builder)
	if err != nil {
		return builderInfo{}, err
	}
	info, err := parseBuildxInspect(b)
	if err != nil {
		return info, fmt.Errorf("builder %s: %w", builder, err)
	}
	if info.Driver != "docker" && info.Driver != "docker-container" {
		return info, fmt.Errorf("builder %s is not a local Docker builder", builder)
	}
	if len(info.Endpoints) == 0 {
		return info, fmt.Errorf("builder %s has no inspectable nodes", builder)
	}
	for _, endpoint := range info.Endpoints {
		if endpoint == "" {
			return info, fmt.Errorf("builder %s has an empty node endpoint", builder)
		}
		if strings.Contains(endpoint, "://") {
			if !strings.HasPrefix(endpoint, "unix://") && !strings.HasPrefix(endpoint, "npipe://") {
				return info, fmt.Errorf("builder %s uses nonlocal endpoint %s", builder, endpoint)
			}
		} else {
			probe := r
			probe.Policy.DockerContext = endpoint
			if err := probe.Local(ctx); err != nil {
				return info, err
			}
		}
	}
	return info, nil
}

// builderRunner returns the Runner a builder's own commands must run through.
//
// A `docker`-driver builder is not a free-standing BuildKit: it is the built-in
// builder OF one Docker context — its single node's endpoint names that
// context — and buildx refuses `du`/`prune --builder <it>` from any other
// context ("use `docker --context=<it> buildx` to switch"). On Docker Desktop
// the current context is `desktop-linux`, so the `default` builder was
// unreachable from the default invocation and both `storage status` and the
// BuildKit budget failed on it. A docker-container builder is reachable from
// any context and keeps the policy's.
//
// localBuilder has already proven that endpoint is a LOCAL context, so pinning
// to it cannot redirect maintenance at a remote daemon.
func (r Runner) builderRunner(info builderInfo) Runner {
	if info.Driver == "docker" && len(info.Endpoints) == 1 && !strings.Contains(info.Endpoints[0], "://") {
		bound := r
		bound.Policy.DockerContext = info.Endpoints[0]
		return bound
	}
	return r
}

// BuildCacheFloorEnv overrides the policy's build_cache_floor for one process
// and the processes it starts: a duration of at least 1h, or "off" for the
// earlier age-gated budget. Never persisted.
const BuildCacheFloorEnv = "FORGE_STORAGE_BUILD_CACHE_FLOOR"

const (
	// defaultBuildCacheFloor is the build-cache budget's safety floor. The
	// Go-cache trim's floor is 2h for the same reason: long enough to cover a
	// build that just finished and the next one that reuses its layers.
	defaultBuildCacheFloor = 2 * time.Hour
	minBuildCacheFloor     = time.Hour
	buildCacheFloorOff     = "off"
)

// parseBuildCacheFloor reads a build_cache_floor value: the floor, whether the
// size rule is on, or an error. Empty is the default floor.
func parseBuildCacheFloor(v string) (time.Duration, bool, error) {
	v = strings.TrimSpace(v)
	switch strings.ToLower(v) {
	case "":
		return defaultBuildCacheFloor, true, nil
	case buildCacheFloorOff:
		return 0, false, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < minBuildCacheFloor {
		return 0, false, fmt.Errorf("want a duration of at least %s, or %q, got %q", minBuildCacheFloor, buildCacheFloorOff, v)
	}
	return d, true, nil
}

// buildCachePrune is what one builder's prune asks BuildKit for.
type buildCachePrune struct {
	// MaxUsed is --max-used-space: BuildKit removes least recently used
	// records until the cache is no larger.
	MaxUsed uint64
	// Until is the --filter until= age: no record used more recently is
	// removed.
	Until string
	// SizeRule is false for the earlier, age-gated budget (floor "off").
	SizeRule bool
}

// buildCachePrune resolves the prune from the policy, $BuildCacheFloorEnv and
// the pass's pressure.
//
// The budget used to apply only to cache unused for build_cache_unused (7
// days). BuildKit removes a record only when it is both older than the
// filter AND over the budget, so a cache that a week of builds filled — every
// record younger than 7 days — was never pruned, however far over budget: on a
// shared machine at 92% disk that pass freed 0 B. The size rule enforces the
// budget against everything older than the floor.
func (r Runner) buildCachePrune() buildCachePrune {
	floor, sizeRule, err := parseBuildCacheFloor(r.Policy.BuildCacheFloor)
	if err != nil { // Validate refuses this; a hand-built Policy may not have run it
		floor, sizeRule = defaultBuildCacheFloor, true
	}
	if v := os.Getenv(BuildCacheFloorEnv); strings.TrimSpace(v) != "" {
		if d, on, err := parseBuildCacheFloor(v); err != nil {
			r.print("warning: %s: %v; using the policy's floor\n", BuildCacheFloorEnv, err)
		} else {
			floor, sizeRule = d, on
		}
	}
	p := buildCachePrune{MaxUsed: r.Policy.BuildCacheGiB * GiB, Until: r.Policy.BuildCacheUnused, SizeRule: sizeRule}
	if r.Pressure {
		// Half the budget, but not below 1 GiB, and never above the budget
		// itself: pressure only ever asks for less cache than the policy does.
		p.MaxUsed = min(max(p.MaxUsed/2, GiB), p.MaxUsed)
	}
	if !sizeRule {
		return p
	}
	// The floor never protects more than the age rule it replaces did.
	if unused, err := time.ParseDuration(r.Policy.BuildCacheUnused); err == nil && unused < floor {
		floor = unused
	}
	p.Until = compactDuration(floor)
	return p
}

// compactDuration prints whole hours as "2h" rather than "2h0m0s".
func compactDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}

func (r Runner) builderGC(ctx context.Context, builder string, apply bool) error {
	info, err := r.localBuilder(ctx, builder)
	if err != nil {
		return err
	}
	p := r.buildCachePrune()
	pressure := ""
	if r.Pressure {
		pressure = " (disk pressure: half of build_cache_gib)"
	}
	if p.SizeRule {
		r.print("builder %s: evict least recently used cache toward %.1f GiB%s, never cache used in the last %s\n", builder, float64(p.MaxUsed)/float64(GiB), pressure, p.Until)
	} else {
		r.print("builder %s: evict unused cache older than %s toward %.1f GiB%s (build_cache_floor off)\n", builder, p.Until, float64(p.MaxUsed)/float64(GiB), pressure)
	}
	if !apply {
		return nil
	}
	b, err := r.builderRunner(info).docker(ctx, "buildx", "prune", "--builder", builder, "--force", "--max-used-space", fmt.Sprintf("%dB", p.MaxUsed), "--filter", "until="+p.Until)
	if err == nil {
		r.print("%s\n", b)
	}
	return err
}

// builderUsage is `docker buildx du` for one builder, run in its own context.
func (r Runner) builderUsage(ctx context.Context, builder string) ([]byte, error) {
	info, err := r.localBuilder(ctx, builder)
	if err != nil {
		return nil, err
	}
	return r.builderRunner(info).docker(ctx, "buildx", "du", "--builder", builder)
}
