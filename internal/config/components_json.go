package config

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ComponentsFileName is the project-root file that holds the per-component
// entities as the authored single source of truth. forge.yaml carries only
// GLOBAL project state (name, module_path, forge_version, frontends, and the
// section blocks); the components a project builds and runs live here.
//
// This is the "forge.yaml per-service, as JSON" — hand- or CLI-authored
// SOURCE, not a generated projection. (The deploy layer's
// deploy/kcl/components_gen.json is a separate, generated DENORMALIZED
// projection of the same data with deploy-only fields; do not confuse the
// two.)
const ComponentsFileName = "components.json"

// componentsDoc is the on-disk shape of components.json: a thin wrapper
// around a denormalized array of component entities. The wrapper (rather
// than a bare top-level array) leaves room for a future schema_version /
// project pin without a breaking reshape, and reads naturally as
// `{"components": [ ... ]}`.
type componentsDoc struct {
	// Components is the denormalized array of component entities. Each entry
	// carries exactly the authored fields a ComponentConfig holds (name,
	// kind, path, ports, schedule, proto_packages, webhooks, and the
	// operator group/version/crds). Field order in the JSON is irrelevant.
	Components []componentJSON `json:"components"`
}

// componentJSON is the JSON encoding of one ComponentConfig. It mirrors the
// YAML surface ComponentConfig used to carry in forge.yaml's `components:`
// block, transposed to JSON tags. Ports is an object keyed by port name
// (http/grpc/metrics/…) so the terse `"http": 8080` and the full
// `"http": {"port": 8080, "expose": true}` forms both round-trip.
type componentJSON struct {
	Name          string                  `json:"name"`
	Kind          string                  `json:"kind"`
	Path          string                  `json:"path,omitempty"`
	Ports         map[string]portSpecJSON `json:"ports,omitempty"`
	Schedule      string                  `json:"schedule,omitempty"`
	ProtoPackages []string                `json:"proto_packages,omitempty"`
	Webhooks      []webhookJSON           `json:"webhooks,omitempty"`
	Group         string                  `json:"group,omitempty"`
	Version       string                  `json:"version,omitempty"`
	CRDs          []crdJSON               `json:"crds,omitempty"`
}

// portSpecJSON encodes a PortSpec. It unmarshals from EITHER a bare JSON
// number (`"http": 8080`) or an object (`"http": {"port": 8080, "protocol":
// "tcp", "expose": true}`), mirroring the YAML PortSpec sugar.
type portSpecJSON struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol,omitempty"`
	Expose   bool   `json:"expose,omitempty"`
}

// UnmarshalJSON accepts a bare number (the common single-port case) or a
// full object, so `"http": 8080` and `"http": {"port": 8080}` both decode.
func (p *portSpecJSON) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed != "" && (trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9')) {
		var n int
		if err := json.Unmarshal(data, &n); err != nil {
			return err
		}
		p.Port = n
		return nil
	}
	type rawPortSpec portSpecJSON
	var raw rawPortSpec
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*p = portSpecJSON(raw)
	return nil
}

// MarshalJSON emits the terse number form (`8080`) when protocol and expose
// are at their defaults, and the full object otherwise — so a freshly
// authored components.json stays as terse as the single-port case allows.
func (p portSpecJSON) MarshalJSON() ([]byte, error) {
	if p.Protocol == "" && !p.Expose {
		return json.Marshal(p.Port)
	}
	type rawPortSpec portSpecJSON
	return json.Marshal(rawPortSpec(p))
}

type webhookJSON struct {
	Name string `json:"name"`
}

type crdJSON struct {
	Name    string `json:"name"`
	Group   string `json:"group,omitempty"`
	Version string `json:"version,omitempty"`
	Shape   string `json:"shape,omitempty"`
}

// ParseComponentsJSON decodes a components.json byte stream into the
// canonical []ComponentConfig. Empty input (no file) yields a nil slice and
// no error — a project with no components (a pure library) is valid.
func ParseComponentsJSON(data []byte) ([]ComponentConfig, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var doc componentsDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("%s: %w", ComponentsFileName, err)
	}
	out := make([]ComponentConfig, 0, len(doc.Components))
	for _, c := range doc.Components {
		out = append(out, c.toConfig())
	}
	return out, nil
}

// MarshalComponentsJSON renders []ComponentConfig as the canonical
// components.json byte stream (indented, trailing newline). This is the
// write path the CLI mutation methods (forge add) and `forge new` use.
func MarshalComponentsJSON(components []ComponentConfig) ([]byte, error) {
	doc := componentsDoc{Components: make([]componentJSON, 0, len(components))}
	for _, c := range components {
		doc.Components = append(doc.Components, componentToJSON(c))
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", ComponentsFileName, err)
	}
	return append(data, '\n'), nil
}

func (c componentJSON) toConfig() ComponentConfig {
	out := ComponentConfig{
		Name:          c.Name,
		Kind:          c.Kind,
		Path:          c.Path,
		Schedule:      c.Schedule,
		ProtoPackages: c.ProtoPackages,
		Group:         c.Group,
		Version:       c.Version,
	}
	if len(c.Ports) > 0 {
		out.Ports = make(map[string]PortSpec, len(c.Ports))
		for name, p := range c.Ports {
			out.Ports[name] = PortSpec(p)
		}
	}
	for _, w := range c.Webhooks {
		out.Webhooks = append(out.Webhooks, WebhookConfig(w))
	}
	for _, crd := range c.CRDs {
		out.CRDs = append(out.CRDs, CRDConfig(crd))
	}
	return out
}

func componentToJSON(c ComponentConfig) componentJSON {
	out := componentJSON{
		Name:          c.Name,
		Kind:          c.Kind,
		Path:          c.Path,
		Schedule:      c.Schedule,
		ProtoPackages: c.ProtoPackages,
		Group:         c.Group,
		Version:       c.Version,
	}
	if len(c.Ports) > 0 {
		out.Ports = make(map[string]portSpecJSON, len(c.Ports))
		for name, p := range c.Ports {
			out.Ports[name] = portSpecJSON(p)
		}
	}
	for _, w := range c.Webhooks {
		out.Webhooks = append(out.Webhooks, webhookJSON(w))
	}
	for _, crd := range c.CRDs {
		out.CRDs = append(out.CRDs, crdJSON(crd))
	}
	return out
}

// DeriveProjectKind infers the project kind from its components — kind is no
// longer a forge.yaml field. The rule:
//
//   - any server/worker/cron/operator component  → "service"
//     (server-shaped: they need handlers/bootstrap/deploy)
//   - only binary component(s), no server-shaped  → "cli"
//     (a binary-only project is a cobra CLI — the binary IS the cli main)
//   - components.json present but empty           → "service"
//     (the canonical "service shell": `forge new` with no --service. The
//     binary boots an empty appkit table and the user grows it with
//     `forge add service`. The presence of the file is the service signal.)
//   - no components.json at all                   → "library"
//     (a pure Go module with no buildable entrypoint)
//
// hasComponentsFile distinguishes the empty-service-shell (file present,
// zero entries → service) from a pure library (no file → library). It is
// the ONE filesystem fact the kind decision needs that the slice alone
// can't carry; the loader supplies it.
//
// An unknown/empty component kind counts as server (EffectiveKind defaults
// to server), so it pulls the project toward "service".
func DeriveProjectKind(components []ComponentConfig, hasComponentsFile bool) string {
	if len(components) == 0 {
		if hasComponentsFile {
			return ProjectKindService
		}
		return ProjectKindLibrary
	}
	sawBinary := false
	for _, c := range components {
		switch c.EffectiveKind() {
		case ComponentKindBinary:
			sawBinary = true
		default:
			// server/worker/cron/operator — server-shaped → service.
			return ProjectKindService
		}
	}
	if sawBinary {
		return ProjectKindCLI
	}
	return ProjectKindService
}

// SortComponentsForWrite returns a stable ordering for components.json:
// declaration order is not meaningful, so a deterministic name sort keeps
// the file diff-friendly across adds. (Kept separate from the mutation
// methods so they can append without re-sorting mid-flow.)
func SortComponentsForWrite(components []ComponentConfig) []ComponentConfig {
	out := make([]ComponentConfig, len(components))
	copy(out, components)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
