package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Tag is a registry alias and its last push timestamp.
type Tag struct {
	Repository string
	Name       string
	Digest     string
	Pushed     time.Time
}

// Version identifies one content-addressed registry manifest.
type Version struct {
	Repository string
	Digest     string
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var releaseTagPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+$`)

// RegistryCandidates protects aliases at digest granularity: deleting one
// manifest also removes every tag that points to it.
func RegistryCandidates(tags []Tag, p Policy, reg Registry, protected map[string]map[string]bool, now time.Time) []Version {
	var result []Version
	for _, repo := range reg.Repositories {
		var rows []Tag
		for _, t := range tags {
			if t.Repository == repo {
				rows = append(rows, t)
			}
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Pushed.Equal(rows[j].Pushed) {
				return rows[i].Digest < rows[j].Digest
			}
			return rows[i].Pushed.After(rows[j].Pushed)
		})
		keep := map[string]bool{}
		ranked := map[string]bool{}
		for _, t := range rows {
			if !ranked[t.Digest] {
				ranked[t.Digest] = true
				if len(ranked) <= p.RegistryKeep {
					keep[t.Digest] = true
				}
			}
			if now.Sub(t.Pushed) <= time.Duration(p.RegistryDays)*24*time.Hour || protected["*"][t.Digest] || protected[repo][t.Name] || protected[repo][t.Digest] || releaseTagPattern.MatchString(t.Name) || t.Name == "dev" || t.Name == "e2e" || t.Name == "latest" || t.Name == "stable" || t.Name == "main" {
				keep[t.Digest] = true
			}
		}
		for digest := range ranked {
			if !keep[digest] {
				result = append(result, Version{repo, digest})
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Repository == result[j].Repository {
			return result[i].Digest < result[j].Digest
		}
		return result[i].Repository < result[j].Repository
	})
	return result
}

func imageRefs(value any) []string {
	var result []string
	switch x := value.(type) {
	case map[string]any:
		for k, v := range x {
			if k == "image" || k == "imageID" {
				if s, ok := v.(string); ok {
					result = append(result, strings.TrimPrefix(s, "docker-pullable://"))
				}
			}
			result = append(result, imageRefs(v)...)
		}
	case []any:
		for _, v := range x {
			result = append(result, imageRefs(v)...)
		}
	}
	return result
}

func (r Runner) protected(ctx context.Context, reg Registry) (map[string]map[string]bool, error) {
	refs := append([]string{}, r.Policy.Pins...)
	for _, cluster := range reg.Contexts {
		b, err := r.command(ctx, "kubectl", "--context", cluster, "--request-timeout=20s", "get", "pods,deployments,statefulsets,daemonsets,replicasets,jobs,cronjobs,replicationcontrollers", "-A", "-o", "json")
		if err != nil {
			return nil, fmt.Errorf("refusing registry cleanup: protected set for %s unavailable: %w", cluster, err)
		}
		var v any
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, err
		}
		refs = append(refs, imageRefs(v)...)
	}
	containerRefs, err := r.containerReferences(ctx)
	if err != nil {
		return nil, err
	}
	refs = append(refs, containerRefs...)

	protected := map[string]map[string]bool{}
	for _, ref := range refs {
		if digestPattern.MatchString(ref) {
			if protected["*"] == nil {
				protected["*"] = map[string]bool{}
			}
			protected["*"][ref] = true
			continue
		}
		host, path, ok := strings.Cut(ref, "/")
		if !ok || !contains(reg.Aliases, host) {
			continue
		}
		repo, version, ok := strings.Cut(path, "@")
		if ok {
			if i := strings.LastIndex(repo, ":"); i >= 0 {
				repo = repo[:i]
			}
		} else {
			repo = path
			version = "latest"
			if i := strings.LastIndex(path, ":"); i >= 0 {
				repo, version = path[:i], path[i+1:]
			}
		}
		if protected[repo] == nil {
			protected[repo] = map[string]bool{}
		}
		protected[repo][version] = true
	}
	return protected, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (r Runner) inventory(ctx context.Context, container string) ([]Tag, error) {
	script := `find /var/lib/registry/docker/registry/v2/repositories -path '*/_manifests/tags/*/current/link' -type f -exec sh -ec 'for p do printf "%s\t%s\t%s\n" "$(stat -c %Y "$p")" "$p" "$(cat "$p")"; done' sh {} +`
	b, err := r.docker(ctx, "exec", container, "sh", "-ec", script)
	if err != nil {
		return nil, err
	}
	var tags []Tag
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 3 || !digestPattern.MatchString(cols[2]) {
			return nil, fmt.Errorf("invalid registry tag inventory")
		}
		stamp, err := strconv.ParseInt(cols[0], 10, 64)
		if err != nil {
			return nil, err
		}
		path := strings.TrimPrefix(cols[1], "/var/lib/registry/docker/registry/v2/repositories/")
		repo, tag, ok := strings.Cut(path, "/_manifests/tags/")
		if !ok {
			return nil, fmt.Errorf("invalid registry path")
		}
		tags = append(tags, Tag{repo, strings.TrimSuffix(tag, "/current/link"), cols[2], time.Unix(stamp, 0)})
	}
	return tags, nil
}

// revisions inventories every manifest link, including untagged indexes. Their
// children must remain pullable even when a child has its own expired tag.
func (r Runner) revisions(ctx context.Context, container string) ([]Version, error) {
	script := `find /var/lib/registry/docker/registry/v2/repositories -path '*/_manifests/revisions/sha256/*/link' -type f -exec sh -ec 'for p do printf "%s\t%s\n" "$p" "$(cat "$p")"; done' sh {} +`
	b, err := r.docker(ctx, "exec", container, "sh", "-ec", script)
	if err != nil {
		return nil, err
	}
	var versions []Version
	const prefix = "/var/lib/registry/docker/registry/v2/repositories/"
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 2 || !strings.HasPrefix(cols[0], prefix) || !digestPattern.MatchString(cols[1]) {
			return nil, fmt.Errorf("invalid registry revision inventory")
		}
		repo, revision, ok := strings.Cut(strings.TrimPrefix(cols[0], prefix), "/_manifests/revisions/sha256/")
		if !ok || repo == "" || revision != strings.TrimPrefix(cols[1], "sha256:")+"/link" {
			return nil, fmt.Errorf("invalid registry revision path")
		}
		versions = append(versions, Version{repo, cols[1]})
	}
	return versions, nil
}

type containerInfo struct {
	Image           string
	State           struct{ Running bool }
	Config          struct{ Cmd []string }
	Mounts          []struct{ Type, Destination string }
	NetworkSettings struct {
		Ports    map[string][]struct{ HostPort string }
		Networks map[string]any
	}
}

func (r Runner) inspect(ctx context.Context, name string) (containerInfo, error) {
	b, err := r.docker(ctx, "inspect", name)
	if err != nil {
		return containerInfo{}, err
	}
	var result []containerInfo
	if err := json.Unmarshal(b, &result); err != nil {
		return containerInfo{}, err
	}
	if len(result) != 1 {
		return containerInfo{}, fmt.Errorf("expected one registry container")
	}
	return result[0], nil
}
func (r Runner) endpoint(ctx context.Context, name string) (string, error) {
	c, err := r.inspect(ctx, name)
	if err != nil {
		return "", err
	}
	ports := c.NetworkSettings.Ports["5000/tcp"]
	if len(ports) == 0 {
		return "", fmt.Errorf("registry %s has no published port", name)
	}
	return "http://127.0.0.1:" + ports[0].HostPort, nil
}

func registryRequest(ctx context.Context, base, path, method string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json")
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 20 * time.Second}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("registry %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func (r Runner) registryPlan(ctx context.Context, reg Registry, container, base string) ([]Version, error) {
	protected, err := r.protected(ctx, reg)
	if err != nil {
		return nil, err
	}
	tags, err := r.inventory(ctx, container)
	if err != nil {
		return nil, err
	}
	revisions, err := r.revisions(ctx, container)
	if err != nil {
		return nil, err
	}
	result, err := registryGraphPlan(tags, revisions, r.Policy, reg, protected, time.Now(), func(v Version) ([]byte, error) {
		return registryRequest(ctx, base, "/v2/"+v.Repository+"/manifests/"+v.Digest, http.MethodGet)
	})
	if err != nil {
		return nil, err
	}
	for _, v := range result {
		r.print("expire %s/%s@%s\n", reg.Container, v.Repository, v.Digest)
	}
	r.print("registry %s: %d eligible manifests; %d tags; retain %d days and %d versions\n", reg.Container, len(result), len(tags), r.Policy.RegistryDays, r.Policy.RegistryKeep)
	return result, nil
}

// registryGraphPlan preserves the full graph of every retained manifest, not
// only tagged roots. fetch errors abort the complete plan before deletion.
func registryGraphPlan(tags []Tag, revisions []Version, policy Policy, reg Registry, protected map[string]map[string]bool, now time.Time, fetch func(Version) ([]byte, error)) ([]Version, error) {
	selected := map[Version]bool{}
	for _, v := range RegistryCandidates(tags, policy, reg, protected, now) {
		selected[v] = true
	}
	var roots []Version
	// A complete filesystem revision inventory lets us retain untagged indexes,
	// which cannot be enumerated through the Distribution tags API. Global pins
	// are applied by RegistryCandidates before selecting any tagged versions.
	for _, v := range revisions {
		if !selected[v] {
			roots = append(roots, v)
		}
	}
	// Include retained tag roots as well: inconsistent inventory must fail closed
	// on manifest reads rather than silently losing protection.
	for _, t := range tags {
		v := Version{t.Repository, t.Digest}
		if !selected[v] {
			roots = append(roots, v)
		}
	}
	for repo, refs := range protected {
		for ref := range refs {
			if digestPattern.MatchString(ref) {
				roots = append(roots, Version{repo, ref})
			}
		}
	}
	visited := map[Version]bool{}
	for len(roots) > 0 {
		v := roots[len(roots)-1]
		roots = roots[:len(roots)-1]
		if visited[v] || !contains(reg.Repositories, v.Repository) {
			continue
		}
		visited[v] = true
		delete(selected, v)
		b, err := fetch(v)
		if err != nil {
			return nil, err
		}
		var manifest struct{ Manifests []struct{ Digest string } }
		if err := json.Unmarshal(b, &manifest); err != nil {
			return nil, err
		}
		for _, child := range manifest.Manifests {
			if !digestPattern.MatchString(child.Digest) {
				return nil, fmt.Errorf("invalid child digest")
			}
			roots = append(roots, Version{v.Repository, child.Digest})
		}
	}
	var result []Version
	for v := range selected {
		result = append(result, v)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Repository == result[j].Repository {
			return result[i].Digest < result[j].Digest
		}
		return result[i].Repository < result[j].Repository
	})
	return result, nil
}

// RegistryGC deletes eligible manifests and collects blobs with all writers stopped.
func (r Runner) RegistryGC(ctx context.Context, reg Registry, apply bool) (err error) {
	info, err := r.inspect(ctx, reg.Container)
	if err != nil {
		return err
	}
	if !info.State.Running {
		r.print("registry %s stopped; skipping\n", reg.Container)
		return nil
	}
	volume := false
	for _, m := range info.Mounts {
		if m.Type == "volume" && m.Destination == "/var/lib/registry" {
			volume = true
		}
	}
	if !volume || len(info.Config.Cmd) != 1 || info.Config.Cmd[0] != "/etc/docker/registry/config.yml" {
		return fmt.Errorf("registry %s is not a supported standalone k3d filesystem registry", reg.Container)
	}
	if err := r.verifyConsumers(ctx, reg, info); err != nil {
		return err
	}
	var b []byte

	base, err := r.endpoint(ctx, reg.Container)
	if err != nil {
		return err
	}
	plan, err := r.registryPlan(ctx, reg, reg.Container, base)
	if err != nil || !apply || len(plan) == 0 {
		return err
	}
	helper, gc := reg.Container+"-forge-retention", reg.Container+"-forge-gc"
	for _, name := range []string{helper, gc} {
		b, err = r.docker(ctx, "ps", "-aq", "--filter", "name=^/"+name+"$")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(b)) != "" {
			return fmt.Errorf("stale maintenance container %s: inspect before retrying", name)
		}
	}
	// Cleanup has its own context: cancellation of the caller must not leave a
	// live GC writer overlapping a restarted public registry.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, name := range []string{gc, helper} {
			b, e := r.docker(cleanup, "ps", "-aq", "--filter", "name=^/"+name+"$")
			if e != nil {
				err = fmt.Errorf("maintenance cleanup failed; registry remains stopped: %w", e)
				return
			}
			if strings.TrimSpace(string(b)) != "" {
				if _, e = r.docker(cleanup, "stop", name); e != nil {
					err = fmt.Errorf("stop maintenance writer before restarting registry: %w", e)
					return
				}
			}
		}
		if _, e := r.docker(cleanup, "start", reg.Container); e != nil {
			err = fmt.Errorf("restart registry: %w", e)
		}
	}()
	if _, err = r.docker(ctx, "stop", "--time", "30", reg.Container); err != nil {
		return err
	}
	if _, err = r.docker(ctx, "run", "-d", "--rm", "--pull=never", "--name", helper, "--volumes-from", reg.Container+":rw", "-p", "127.0.0.1::5000", "-e", "REGISTRY_STORAGE_DELETE_ENABLED=true", info.Image); err != nil {
		return err
	}
	base, err = r.endpoint(ctx, helper)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 30; attempt++ {
		if _, err = registryRequest(ctx, base, "/v2/", http.MethodGet); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		return err
	}
	plan, err = r.registryPlan(ctx, reg, helper, base)
	if err != nil {
		return err
	}
	for _, v := range plan {
		if _, err = registryRequest(ctx, base, "/v2/"+v.Repository+"/manifests/"+v.Digest, http.MethodDelete); err != nil {
			return err
		}
	}
	if _, err = r.docker(ctx, "stop", helper); err != nil {
		return err
	}
	// Preserve untagged manifests: they can be index children or OCI artifacts.
	b, err = r.docker(ctx, "run", "--rm", "--pull=never", "--network", "none", "--name", gc, "--volumes-from", reg.Container+":rw", info.Image, "garbage-collect", "/etc/docker/registry/config.yml")
	r.print("%s\n", b)
	return err
}

func (r Runner) containerReferences(ctx context.Context) ([]string, error) {
	var refs []string
	b, err := r.docker(ctx, "ps", "-aq")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(b))
	if len(ids) == 0 {
		return nil, nil
	}
	b, err = r.docker(ctx, append([]string{"inspect"}, ids...)...)
	if err != nil {
		return nil, err
	}
	var containers []struct {
		Config struct{ Image string }
		Image  string
	}
	if err := json.Unmarshal(b, &containers); err != nil {
		return nil, err
	}
	var images []string
	seen := map[string]bool{}
	for _, c := range containers {
		refs = append(refs, c.Config.Image)
		if !seen[c.Image] {
			images = append(images, c.Image)
			seen[c.Image] = true
		}
	}
	if len(images) > 0 {
		b, err = r.docker(ctx, append([]string{"image", "inspect"}, images...)...)
		if err != nil {
			return nil, err
		}
		var inspected []struct{ RepoDigests []string }
		if err := json.Unmarshal(b, &inspected); err != nil {
			return nil, err
		}
		for _, im := range inspected {
			refs = append(refs, im.RepoDigests...)
		}
	}
	return refs, nil
}

func (r Runner) verifyConsumers(ctx context.Context, reg Registry, info containerInfo) error {
	// Every connected k3d cluster must be checked, including stopped clusters;
	// sharing the registry cannot silently introduce an unprotected consumer.
	b, err := r.docker(ctx, "ps", "-aq", "--filter", "label=k3d.role=server")
	if err != nil {
		return err
	}
	if ids := strings.Fields(string(b)); len(ids) > 0 {
		b, err = r.docker(ctx, append([]string{"inspect"}, ids...)...)
		if err != nil {
			return err
		}
		var nodes []struct {
			Config          struct{ Labels map[string]string }
			NetworkSettings struct{ Networks map[string]any }
		}
		if err := json.Unmarshal(b, &nodes); err != nil {
			return err
		}
		for _, n := range nodes {
			for network := range n.NetworkSettings.Networks {
				if _, shared := info.NetworkSettings.Networks[network]; shared {
					cluster := "k3d-" + n.Config.Labels["k3d.cluster"]
					if !contains(reg.Contexts, cluster) {
						return fmt.Errorf("registry %s also serves %s; register this context before cleanup", reg.Container, cluster)
					}
				}
			}
		}
	}
	return nil
}
