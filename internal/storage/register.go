package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Register derives registry ownership from declared image repositories and live
// k3d metadata. It never makes an arbitrary remote registry a cleanup target.
func Register(ctx context.Context, path string, contexts, repositories, pins []string) error {
	return WithLock(path, func() error {
		p, err := Load(path)
		if err != nil {
			return err
		}
		r := Runner{Policy: p}
		if err := r.bindLocalContext(ctx); err != nil {
			return err
		}
		p = r.Policy
		for _, c := range contexts {
			if strings.HasPrefix(c, "k3d-") && !contains(p.Clusters, c) {
				p.Clusters = append(p.Clusters, c)
			}
		}
		for _, pin := range pins {
			if !contains(p.Pins, pin) {
				p.Pins = append(p.Pins, pin)
			}
		}
		b, err := r.docker(ctx, "ps", "-aq", "--filter", "label=k3d.role=registry")
		if err != nil {
			return err
		}
		for _, id := range strings.Fields(string(b)) {
			data, err := r.docker(ctx, "inspect", id)
			if err != nil {
				return err
			}
			var containers []struct {
				Name            string
				NetworkSettings struct {
					Ports map[string][]struct{ HostPort string }
				}
			}
			if err := json.Unmarshal(data, &containers); err != nil {
				return err
			}
			for _, c := range containers {
				name := strings.TrimPrefix(c.Name, "/")
				ports := c.NetworkSettings.Ports["5000/tcp"]
				if len(ports) == 0 {
					continue
				}
				aliases := []string{"localhost:" + ports[0].HostPort, "127.0.0.1:" + ports[0].HostPort, name + ":5000", strings.TrimPrefix(name, "k3d-") + ".localhost:" + ports[0].HostPort, "registry.localhost:5000", "registry.localhost:" + ports[0].HostPort}
				reg := Registry{Container: name, Aliases: aliases, Contexts: append([]string{}, p.Clusters...)}
				index := -1
				for i, old := range p.Registries {
					if old.Container == name {
						reg = old
						index = i
						break
					}
				}
				for _, context := range contexts {
					if strings.HasPrefix(context, "k3d-") && !contains(reg.Contexts, context) {
						reg.Contexts = append(reg.Contexts, context)
					}
				}
				for _, repository := range repositories {
					host, repo, ok := strings.Cut(repository, "/")
					if ok && contains(aliases, host) && !contains(reg.Repositories, repo) {
						reg.Repositories = append(reg.Repositories, repo)
					}
				}
				if len(reg.Repositories) == 0 || len(reg.Contexts) == 0 {
					continue
				}
				if index >= 0 {
					p.Registries[index] = reg
				} else {
					p.Registries = append(p.Registries, reg)
				}
			}
		}
		if err := Save(path, p); err != nil {
			return fmt.Errorf("register local storage ownership: %w", err)
		}
		return nil
	})
}
