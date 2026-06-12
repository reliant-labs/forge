package codegen

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/config"
)

// ComponentsJSONRelPath is the project-relative path of the generated
// denormalized component data. The per-env `deploy/kcl/<env>/main.k`
// imports it and lets the forge KCL Component schema hierarchy expand
// each entry into k8s resources. JSON carries no inline forge:hash
// marker, so the write is self-certified via `.forge/hashes.json`
// (the Tier-1 unstampable fallback) by checksums.WriteGeneratedFile.
const ComponentsJSONRelPath = "deploy/kcl/components_gen.json"

// componentPortJSON is the denormalized projection of one named port.
// KCL's Server subtype maps these to ServicePort + containerPort.
type componentPortJSON struct {
	Name     string `json:"name"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	Expose   bool   `json:"expose"`
}

// componentJSON is the denormalized BASE shape of one component. It
// carries ZERO Kubernetes knowledge — no Deployment/Service/CronJob
// concepts. The forge KCL `Component` schema (and its kind-selected
// subtypes Server/Worker/Cron/Operator/Binary) own ALL normalization
// into k8s resources. forge JSON and the KCL schemas are deliberately
// NOT 1:1: KCL inheritance/defaults do the expansion.
//
// `command` is the denormalized run command. It is populated only for
// binary components today — `["/app/<proj>", "<name>"]`, the cobra
// subcommand the shared image runs. Server/worker/cron run the image's
// default entrypoint, so their command is empty and KCL fills it.
type componentJSON struct {
	Name     string              `json:"name"`
	Kind     string              `json:"kind"`
	Ports    []componentPortJSON `json:"ports"`
	Env      map[string]string   `json:"env"`
	Command  []string            `json:"command"`
	Schedule string              `json:"schedule"`
	Group    string              `json:"group"`
	Version  string              `json:"version"`
	CRDs     []string            `json:"crds"`
}

// componentsDoc is the top-level shape of components_gen.json.
type componentsDoc struct {
	// Project is the project name. Binary components run
	// `["/app/<project>", "<name>"]`, so KCL needs the project name to
	// build that command without a second data channel.
	Project    string          `json:"project"`
	Components []componentJSON `json:"components"`
}

// ComponentsToJSON projects the forge.yaml component list to the
// denormalized JSON document. Deterministic: ports are sorted by name
// and components keep forge.yaml order so re-generation is idempotent.
func ComponentsToJSON(projectName string, components []config.ComponentConfig) ([]byte, error) {
	doc := componentsDoc{Project: projectName, Components: []componentJSON{}}
	for _, c := range components {
		cj := componentJSON{
			Name:     c.Name,
			Kind:     c.EffectiveKind(),
			Ports:    []componentPortJSON{},
			Env:      map[string]string{},
			Command:  []string{},
			Schedule: c.Schedule,
			Group:    c.Group,
			Version:  c.Version,
			CRDs:     []string{},
		}

		// Ports — emit a stable, name-sorted list. The scalar `protocol`
		// default ("" in the struct) projects as "tcp" so KCL never has
		// to special-case the terse scalar form.
		names := make([]string, 0, len(c.Ports))
		for name := range c.Ports {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			ps := c.Ports[name]
			proto := ps.Protocol
			if proto == "" {
				proto = "tcp"
			}
			cj.Ports = append(cj.Ports, componentPortJSON{
				Name:     name,
				Port:     ps.Port,
				Protocol: proto,
				Expose:   ps.Expose,
			})
		}

		// Binary components run a cobra subcommand off the shared image:
		// `["/app/<project>", "<name>"]`. This is the one denormalized
		// command forge knows at config time; everything else is the
		// image's default entrypoint and KCL fills it per kind.
		if cj.Kind == config.ComponentKindBinary {
			cj.Command = []string{
				fmt.Sprintf("/app/%s", projectName),
				c.Name,
			}
		}

		for _, crd := range c.CRDs {
			cj.CRDs = append(cj.CRDs, crd.Name)
		}

		doc.Components = append(doc.Components, cj)
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal components_gen.json: %w", err)
	}
	out = append(out, '\n')
	return out, nil
}

// GenerateComponentsJSON writes deploy/kcl/components_gen.json from the
// project's component list and self-certifies it via the checksums
// chokepoint (which routes JSON through the .forge/hashes.json
// unstampable fallback). When cs is nil the file is written directly
// (fixture/test paths that don't track state).
func GenerateComponentsJSON(projectDir, projectName string, components []config.ComponentConfig, cs *checksums.FileChecksums) error {
	content, err := ComponentsToJSON(projectName, components)
	if err != nil {
		return err
	}
	if cs != nil {
		if _, err := checksums.WriteGeneratedFile(projectDir, ComponentsJSONRelPath, content, cs, true); err != nil {
			return fmt.Errorf("write %s: %w", ComponentsJSONRelPath, err)
		}
		return nil
	}
	dest := filepath.Join(projectDir, ComponentsJSONRelPath)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0o644)
}
