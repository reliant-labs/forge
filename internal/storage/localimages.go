package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A host image tagged for a local registry that no longer exists — a k3d
// registry deleted with its cluster, or one recreated on another port — can
// never be pushed to or pulled from again, yet each copy of a base image keeps
// its full size on the host. Nothing else reclaims them: BuildKit and the
// kubelet only know their own caches.

var (
	localRegistryHost = regexp.MustCompile(`^(?:localhost|registry\.localhost|k3d-[A-Za-z0-9._-]+):([0-9]+)$`)
	publishedHostPort = regexp.MustCompile(`:([0-9]+)->`)
)

// StaleLocalImage is one host image reference to be untagged.
type StaleLocalImage struct {
	Ref string
	ID  string
}

type hostImageRow struct {
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	ID         string `json:"ID"`
}

// StaleLocalImages lists host image tags whose repository host is a local
// registry alias (`localhost:<port>`, `registry.localhost:<port>`,
// `k3d-<name>:<port>`) that no running container backs, and whose image no
// container, running or stopped, uses.
//
// A `localhost` or `registry.localhost` alias is live when a running container
// publishes that host port. A `k3d-<name>` alias is live when a running
// container of that name exists. Anything unparseable is kept; a failed docker
// call fails the layer rather than guessing.
func (r Runner) StaleLocalImages(ctx context.Context) ([]StaleLocalImage, error) {
	b, err := r.docker(ctx, "ps", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	livePorts, liveNames := map[string]bool{}, map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var c struct{ Names, Ports string }
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("parse docker ps: %w", err)
		}
		for _, m := range publishedHostPort.FindAllStringSubmatch(c.Ports, -1) {
			livePorts[m[1]] = true
		}
		for _, name := range strings.Split(c.Names, ",") {
			liveNames[name] = true
		}
	}

	// An alias a registered, running registry answers to is live even when no
	// host port says so (registry.localhost:5000 is the in-cluster spelling).
	liveAliases := map[string]bool{}
	for _, reg := range r.Policy.Registries {
		if liveNames[reg.Container] {
			for _, alias := range reg.Aliases {
				liveAliases[alias] = true
			}
		}
	}

	used, err := r.containerImageUse(ctx)
	if err != nil {
		return nil, err
	}
	b, err = r.docker(ctx, "image", "ls", "--no-trunc", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	var stale []StaleLocalImage
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var row hostImageRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse docker image ls: %w", err)
		}
		host, _, ok := strings.Cut(row.Repository, "/")
		m := localRegistryHost.FindStringSubmatch(host)
		if !ok || m == nil || row.Tag == "" || row.Tag == "<none>" {
			continue
		}
		if liveAliases[host] {
			continue
		}
		if strings.HasPrefix(host, "k3d-") {
			name, _, _ := strings.Cut(host, ":")
			if liveNames[name] {
				continue
			}
		} else if livePorts[m[1]] {
			continue
		}
		ref := row.Repository + ":" + row.Tag
		if used[row.ID] || used[ref] {
			continue
		}
		stale = append(stale, StaleLocalImage{Ref: ref, ID: row.ID})
	}
	return stale, nil
}

// containerImageUse is every image ID and image reference any container,
// running or stopped, was created from.
func (r Runner) containerImageUse(ctx context.Context) (map[string]bool, error) {
	used := map[string]bool{}
	b, err := r.docker(ctx, "ps", "-aq")
	if err != nil {
		return nil, err
	}
	ids := strings.Fields(string(b))
	if len(ids) == 0 {
		return used, nil
	}
	b, err = r.docker(ctx, append([]string{"inspect", "--format", "{{.Image}} {{.Config.Image}}"}, ids...)...)
	if err != nil {
		return nil, err
	}
	for _, f := range strings.Fields(string(b)) {
		used[f] = true
	}
	return used, nil
}

// LocalImages untags every StaleLocalImage, or only lists them when apply is
// false. `docker rmi <ref>` removes the layers once the last tag is gone, and
// fails rather than force-removing an image a container has since started
// using, which is reported and does not stop the rest.
func (r Runner) LocalImages(ctx context.Context, apply bool) error {
	if err := r.Local(ctx); err != nil {
		return err
	}
	stale, err := r.StaleLocalImages(ctx)
	if err != nil {
		return err
	}
	var failed []string
	for _, img := range stale {
		if !apply {
			r.print("untag stale local-registry image %s\n", img.Ref)
			continue
		}
		if _, err := r.docker(ctx, "rmi", img.Ref); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", img.Ref, err))
			continue
		}
		r.print("untagged stale local-registry image %s\n", img.Ref)
	}
	r.print("local-registry images: %d for a registry that no longer exists\n", len(stale))
	if len(failed) > 0 {
		return fmt.Errorf("could not untag %d image(s): %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}
