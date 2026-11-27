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
	Networks  []string          `json:"networks,omitempty"` // concrete network IDs attached to verified containers
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
		// Capture identities from a verified container, never from the project
		// label: independent stacks can use the same Compose project name.
		out, err := runner.Output(ctx, "docker", "inspect", id)
		if err != nil {
			return err
		}
		var rows []struct {
			NetworkSettings struct {
				Networks map[string]struct{ NetworkID string }
			}
		}
		if err := json.Unmarshal(out, &rows); err != nil {
			return err
		}
		if len(rows) != 1 {
			return fmt.Errorf("inspect %s returned %d containers", id, len(rows))
		}
		for _, network := range rows[0].NetworkSettings.Networks {
			if network.NetworkID != "" && !slices.Contains(record.Networks, network.NetworkID) {
				record.Networks = append(record.Networks, network.NetworkID)
			}
		}
	}
	slices.Sort(record.Networks)
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
// Networks with unproven exclusivity are retained: Compose's own labels and
// network IDs do not distinguish creation from reuse by another stack.
func (p ComposeProvider) StopOwned(ctx context.Context, env string) error {
	pattern := filepath.Join(filepath.Dir(composeOwnedPath(p.projectDir(), env, "_")), "*.json")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	var errs []error
	networks := map[string][]string{}
	unsettled := map[string]bool{}
	for _, path := range paths {
		rec, err := statefile.Read[composeOwned](path, "compose ownership")
		if err != nil {
			errs = append(errs, err)
			continue
		}
		project, settled, err := p.stopRecord(ctx, env, path)
		if err != nil {
			errs = append(errs, err)
		}
		if project == "" {
			continue
		}
		if !settled {
			unsettled[project] = true
		}
		if rec != nil {
			networks[project] = append(networks[project], rec.Networks...)
		}
	}
	// An unreadable record could belong to any project. Do not clean up
	// networks until every record's ownership and settlement is known.
	if len(errs) == 0 {
		for project, ids := range networks {
			if !unsettled[project] {
				p.cleanupOwnedNetworks(ctx, env, project, ids)
			}
		}
	}
	return errors.Join(errs...)
}

// cleanupOwnedNetworks is best effort: failure to prove safety retains the
// network. Compose labels and recorded attachment IDs alone do not prove
// exclusivity. A Compose network may opt into cleanup by declaring all three
// io.reliant.forge.network labels below: exclusive=true, checkout=<absolute
// checkout path>, env=<environment>. This is an explicit declaration that the
// network is private to that checkout/env, not a borrowed or shared network.
// Legacy networks without that declaration are deliberately left alone.
func (p ComposeProvider) cleanupOwnedNetworks(ctx context.Context, env, project string, recorded []string) {
	if len(recorded) == 0 {
		return
	}
	runner := p.runner()
	out, err := runner.Output(ctx, "docker", "container", "ls", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+project)
	if err != nil || strings.TrimSpace(string(out)) != "" {
		return
	}
	out, err = runner.Output(ctx, "docker", "network", "ls", "--quiet", "--filter", "label=com.docker.compose.project="+project)
	if err != nil {
		return
	}
	for _, id := range composeContainerIDs(out) {
		if !slices.Contains(recorded, id) {
			continue
		}
		data, err := runner.Output(ctx, "docker", "network", "inspect", id)
		if err != nil {
			continue
		}
		var rows []struct {
			Labels     map[string]string
			Containers map[string]json.RawMessage
		}
		if json.Unmarshal(data, &rows) != nil || len(rows) != 1 {
			continue
		}
		labels := rows[0].Labels
		if labels["com.docker.compose.project"] != project || labels["com.docker.compose.network"] == "" ||
			labels["io.reliant.forge.network.exclusive"] != "true" ||
			labels["io.reliant.forge.network.env"] != env ||
			labels["io.reliant.forge.network.checkout"] == "" ||
			!sameDirectory(labels["io.reliant.forge.network.checkout"], p.projectDir()) || len(rows[0].Containers) != 0 {
			continue
		}
		// Network inspect omits stopped containers' configured attachments.
		out, err := runner.Output(ctx, "docker", "container", "ls", "--all", "--quiet", "--filter", "network="+id)
		if err != nil || strings.TrimSpace(string(out)) != "" {
			continue
		}
		// Docker also refuses removal if an endpoint appeared since inspection.
		_ = runner.Run(ctx, "docker", "network", "rm", id)
	}
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
