package deploytarget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/internal/statefile"
)

// composeOwned records concrete containers returned by compose after up, not
// declarations that could later select a different project. No credentials.
type composeOwned struct {
	Directory string            `json:"directory"`
	Project   string            `json:"project"`
	Checkout  string            `json:"checkout"`
	Env       string            `json:"env"`
	Service   string            `json:"service"`
	IDs       []string          `json:"ids"`
	Services  map[string]string `json:"services,omitempty"` // includes dependencies created by up
}

func composeOwnedPath(dir, env, service string) string {
	return statefile.Path(dir, filepath.Join("compose-owned", statefile.SafeSegment(env), statefile.SafeSegment(service)+".json"))
}

func (p ComposeProvider) recordOwned(ctx context.Context, runner commandRunner, overlay map[string]string, spec *ComposeSpec, env, service string, before []string) error {
	if spec.Shared {
		return nil
	} // borrowed shared infra is not this stack's child
	args := append(composeArgs(spec), "ps", "--all", "--quiet")
	out, err := outputWithEnv(ctx, runner, overlay, "docker", args...)
	if err != nil {
		return err
	}
	ids := composeContainerIDs(out)
	record := composeOwned{Directory: composeProjectDir(spec), Checkout: p.projectDir(), Env: env, Service: service, Services: map[string]string{}}
	preexisting := map[string]bool{}
	for _, id := range before {
		preexisting[id] = true
	}
	// A repeated up retains ownership only for IDs already recorded by this
	// checkout/env. Same-directory containers started by hand are not adopted.
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(composeOwnedPath(p.projectDir(), env, "_")), "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		prev, err := statefile.Read[composeOwned](path, "compose ownership")
		if err != nil {
			return err
		}
		if prev != nil && prev.Env == env && sameDirectory(prev.Checkout, p.projectDir()) {
			for _, id := range prev.IDs {
				delete(preexisting, id)
			}
		}
	}
	for _, id := range ids {
		if preexisting[id] {
			continue
		}
		labels, err := containerLabels(ctx, runner, id)
		if err != nil {
			return err
		}
		if !sameDirectory(labels["com.docker.compose.project.working_dir"], record.Directory) || labels["com.docker.compose.service"] == "" || labels["com.docker.compose.project"] == "" {
			return fmt.Errorf("compose %s: container %s has no matching ownership labels", service, id)
		}
		if record.Project != "" && record.Project != labels["com.docker.compose.project"] {
			return fmt.Errorf("compose %s: inconsistent project labels", service)
		}
		record.Project = labels["com.docker.compose.project"]
		record.IDs = append(record.IDs, id)
		record.Services[id] = labels["com.docker.compose.service"]
	}
	if len(record.IDs) == 0 {
		return nil
	}
	return statefile.Write(composeOwnedPath(p.projectDir(), env, service), "compose ownership", record)
}

// containerIDRe matches one line of `compose ps --quiet`: a container id
// (hex, from docker or podman) and nothing else.
var containerIDRe = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z_.-]*$`)

// composeContainerIDs reads the container ids out of `compose ps --quiet`
// output, one per line. The runner captures stderr with stdout, and compose
// writes its diagnostics there — `WARN[0000] The "API_KEY" variable is not
// set. Defaulting to a blank string.` for an unset ${VAR} — so splitting the
// whole output on whitespace took each word of the warning for a container,
// failed the ownership check on "time=…", and left every container
// unrecorded for `env down` (NS FG-17). A line with anything but one id on
// it is a diagnostic.
func composeContainerIDs(out []byte) []string {
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); containerIDRe.MatchString(line) {
			ids = append(ids, line)
		}
	}
	return ids
}

func containerLabels(ctx context.Context, runner commandRunner, id string) (map[string]string, error) {
	out, err := runner.Output(ctx, "docker", "inspect", id)
	if err != nil {
		// A previously removed ID is safe to forget; an unavailable daemon or
		// permission error is not proof of absence and must preserve the record.
		if containerAbsent(out) {
			return nil, nil
		}
		return nil, err
	}
	var rows []struct {
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fmt.Errorf("inspect %s returned %d containers", id, len(rows))
	}
	return rows[0].Config.Labels, nil
}

// containerAbsent reports whether a failed `docker inspect` says the object
// does not exist. The match is case-insensitive because the CLI changed the
// spelling: Docker ≤ 28 prints "Error: No such object: <id>", Docker 29 prints
// "error: no such object: <id>". A case-sensitive match turned every
// container compose had already replaced into a hard error, and env down
// stopped before reaching the rest of the stack.
func containerAbsent(out []byte) bool {
	msg := strings.ToLower(string(out))
	return strings.Contains(msg, "no such object") || strings.Contains(msg, "no such container")
}

// OwnedRunning discovers a compose-only stack without relying on a heartbeat
// or on a listener belonging to some unrelated process.
func (p ComposeProvider) OwnedRunning(ctx context.Context, env string) (bool, error) {
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(composeOwnedPath(p.projectDir(), env, "_")), "*.json"))
	if err != nil {
		return false, err
	}
	for _, path := range paths {
		rec, err := statefile.Read[composeOwned](path, "compose ownership")
		if err != nil {
			return false, err
		}
		if rec == nil || rec.Env != env || !sameDirectory(rec.Checkout, p.projectDir()) {
			continue
		}
		for _, id := range rec.IDs {
			labels, err := containerLabels(ctx, p.runner(), id)
			if err != nil {
				return false, err
			}
			if !sameDirectory(labels["com.docker.compose.project.working_dir"], rec.Directory) || labels["com.docker.compose.project"] != rec.Project {
				continue
			}
			out, err := p.runner().Output(ctx, "docker", "inspect", "--format", "{{.State.Running}}", id)
			if err != nil {
				return false, err
			}
			if strings.TrimSpace(string(out)) == "true" {
				return true, nil
			}
		}
	}
	return false, nil
}

// StopOwned removes only this checkout/env's recorded, revalidated container
// IDs. Volumes are deliberately retained (docker rm without -v); no compose
// down, prune, name inference, or recreation from changed YAML is involved.
//
// One container that cannot be verified or removed does not stop the
// teardown: every other record and ID is still processed, and the failures
// are returned together. A failed ID is never removed (its ownership is
// unproven) and stays in its record, so the next env down retries exactly
// that ID; a record whose every ID was settled is deleted.
//
// Once every record of a compose project is settled, the project's empty
// networks go too (removeEmptyProjectNetworks). Compose creates one per
// declared network on up, and removing the containers alone left each of them
// behind: 31 empty forge-*-dev networks built up on one shared host in six
// days and exhausted docker's predefined address pools, after which every
// `docker network create` on the machine failed.
func (p ComposeProvider) StopOwned(ctx context.Context, env string) error {
	pattern := filepath.Join(filepath.Dir(composeOwnedPath(p.projectDir(), env, "_")), "*.json")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	var errs []error
	// settled maps each compose project seen to whether every one of its
	// records was settled: one unsettled record keeps the project's networks.
	settled := map[string]bool{}
	for _, path := range paths {
		project, done, err := p.stopRecord(ctx, env, path)
		if err != nil {
			errs = append(errs, err)
		}
		if project != "" {
			prev, seen := settled[project]
			settled[project] = done && (!seen || prev)
		}
	}
	projects := make([]string, 0, len(settled))
	for project, done := range settled {
		if done {
			projects = append(projects, project)
		}
	}
	slices.Sort(projects)
	for _, project := range projects {
		p.removeEmptyProjectNetworks(ctx, project)
	}
	return errors.Join(errs...)
}

// removeEmptyProjectNetworks removes the networks compose created for project
// once nothing uses them. project comes from a settled ownership record, whose
// name was read from the labels of containers proven ours by their working
// directory; compose labels a network only with com.docker.compose.project,
// .network, .version and .config-hash (no working directory), so the project
// label is the only tie a network has to its stack, and it is checked on the
// network itself rather than trusted from the list filter.
//
// A network is removed only when it carries that project label and a compose
// network label (an external network is never one compose created), has no
// container attached in any state, and no container of the project is left at
// all: a stopped container still references its networks, and removing one
// would make `docker start` on it fail. Removal is one `docker network rm` per
// network, never a prune; volumes are untouched.
//
// Nothing here fails env down. The containers are already gone, which is what
// env down promises; a network still in use (a shared stack, a container
// started meanwhile) is kept, a network already gone needs nothing, and a
// docker failure (daemon down, permission) keeps the network. The settled
// records are deleted by then, so env down does not come back for a network
// kept here; that is the price of never removing what is unproven.
func (p ComposeProvider) removeEmptyProjectNetworks(ctx context.Context, project string) {
	runner := p.runner()
	filter := "label=com.docker.compose.project=" + project
	out, err := runner.Output(ctx, "docker", "container", "ls", "--all", "--quiet", "--filter", filter)
	if err != nil || len(composeContainerIDs(out)) > 0 {
		return
	}
	out, err = runner.Output(ctx, "docker", "network", "ls", "--quiet", "--filter", filter)
	if err != nil {
		return
	}
	// `network ls --quiet` prints one ID per line, the shape `compose ps
	// --quiet` has, diagnostics included.
	for _, id := range composeContainerIDs(out) {
		if !networkUnused(ctx, runner, id, project) {
			continue
		}
		_ = runner.Run(ctx, "docker", "network", "rm", id) // in use, gone or unreachable: kept, see above
	}
}

// networkUnused reports whether network id is compose's network for project
// with no container attached, running or stopped. Anything it cannot
// establish counts as in use.
func networkUnused(ctx context.Context, runner commandRunner, id, project string) bool {
	out, err := runner.Output(ctx, "docker", "network", "inspect", id)
	if err != nil {
		return false
	}
	var rows []struct {
		Labels     map[string]string
		Containers map[string]json.RawMessage // attached endpoints: running containers only
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		return false
	}
	net := rows[0]
	if net.Labels["com.docker.compose.project"] != project || net.Labels["com.docker.compose.network"] == "" || len(net.Containers) > 0 {
		return false
	}
	// A stopped container has no endpoint, so inspect does not list it; the
	// network filter matches on the container's own configuration.
	out, err = runner.Output(ctx, "docker", "container", "ls", "--all", "--quiet", "--filter", "network="+id)
	return err == nil && len(composeContainerIDs(out)) == 0
}

// stopRecord settles one ownership record: each recorded ID is removed when
// its labels still prove it is ours, forgotten when it is gone or now belongs
// to someone else, and kept when neither could be established. It returns the
// record's compose project, and whether the record was settled (and deleted).
func (p ComposeProvider) stopRecord(ctx context.Context, env, path string) (project string, settled bool, _ error) {
	rec, err := statefile.Read[composeOwned](path, "compose ownership")
	if err != nil {
		return "", false, err
	}
	if rec == nil {
		return "", false, nil
	}
	if rec.Env != env || !sameDirectory(rec.Checkout, p.projectDir()) {
		return "", false, fmt.Errorf("compose ownership record %s belongs to another checkout/environment; refusing removal", path)
	}
	var errs []error
	var unsettled []string
	for _, id := range rec.IDs {
		service := rec.Service
		if rec.Services[id] != "" {
			service = rec.Services[id]
		}
		labels, err := containerLabels(ctx, p.runner(), id)
		if err != nil {
			errs = append(errs, fmt.Errorf("verify owned container %s (%s): %w", id, service, err))
			unsettled = append(unsettled, id)
			continue
		}
		if !sameDirectory(labels["com.docker.compose.project.working_dir"], rec.Directory) || labels["com.docker.compose.project"] != rec.Project || labels["com.docker.compose.service"] != service {
			continue // removed already (nil labels), or the ID is no longer ours
		}
		if err := p.runner().Run(ctx, "docker", "rm", "-f", id); err != nil {
			errs = append(errs, fmt.Errorf("remove owned container %s (%s): %w", id, service, err))
			unsettled = append(unsettled, id)
		}
	}
	if len(unsettled) == 0 {
		if err := os.Remove(path); err != nil {
			return rec.Project, false, err
		}
		return rec.Project, true, nil
	}
	rec.IDs = unsettled
	for id := range rec.Services {
		if !slices.Contains(unsettled, id) {
			delete(rec.Services, id)
		}
	}
	if err := statefile.Write(path, "compose ownership", rec); err != nil {
		errs = append(errs, err)
	}
	return rec.Project, false, errors.Join(errs...)
}
