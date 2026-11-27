package cli

import (
	"fmt"
	"strings"
)

// MLStackOutput is one `output.ml_stacks` entry: a declared forge.MLStack
// (kcl/ml.k), projected so tools read the stack and its decisions from
// `forge env render <env> --json` without parsing KCL. Every key is always
// present (empty when unset); keys are only ever added.
type MLStackOutput struct {
	// Cluster is the stack's kube context (ClusterTarget.cluster).
	Cluster string `json:"cluster"`
	// Namespace is the stack's ClusterTarget.namespace.
	Namespace string `json:"namespace"`
	// Observed are the component kinds the last `forge ml discover` found
	// running that forge did not install.
	Observed []string `json:"observed"`
	// ObservedAt is when that discovery ran (YYYY-MM-DD), or empty.
	ObservedAt string `json:"observed_at"`
	// Accelerators are the stack's accelerator vendors.
	Accelerators []MLAcceleratorsOutput `json:"accelerators"`
	// Components are every adopted or installed component, in slot order.
	Components []MLComponentOutput `json:"components"`
	// Decisions are the recommendations the stack dismisses.
	Decisions []MLDecisionOutput `json:"decisions"`
}

// MLAcceleratorsOutput is one forge.Accelerators of a stack.
type MLAcceleratorsOutput struct {
	// Vendor is nvidia, amd, tpu, neuron, gaudi or generic.
	Vendor string `json:"vendor"`
	// Resource is the extended resource its device plugin advertises,
	// defaulted from the vendor.
	Resource string `json:"resource"`
	// NodeSelector narrows the nodes that carry it.
	NodeSelector map[string]string `json:"node_selector"`
}

// MLComponentOutput is one component of a stack: a forge.Adopt or a
// forge.Install in one of its slots.
type MLComponentOutput struct {
	// Slot is accelerators, node_health, queue, training, serving,
	// monitoring or pull_cache.
	Slot string `json:"slot"`
	// Vendor is the Accelerators vendor of an accelerators-slot component,
	// else empty.
	Vendor string `json:"vendor"`
	// Mode is "adopt" (read where it runs) or "install" (forge's own chart).
	Mode string `json:"mode"`
	// Kind is the component id, e.g. dcgm-exporter.
	Kind string `json:"kind"`
	// Namespace is where an adopted component runs, or the Install chart's
	// namespace.
	Namespace string `json:"namespace"`
	// Version is what discovery found for an adoption, or the Install
	// chart's pinned version.
	Version string `json:"version"`
	// Selector is the pod selector of the component's own scrape. Deprecated
	// (v1.14.0): a scrape is a forge.Telemetry entry in
	// Observability.telemetry; the key stays until a future major.
	Selector map[string]string `json:"selector"`
	// Port is the metrics port of the component's own scrape (0: none).
	// Deprecated (v1.14.0), as Selector.
	Port int `json:"port"`
	// Allow are the exposition names the component's own scrape keeps.
	// Deprecated (v1.14.0), as Selector.
	Allow []string `json:"allow"`
	// Note is an adoption's reviewer note.
	Note string `json:"note"`
	// ServiceName is the service.name of the component's own scrape when it
	// renders (a port, and the env's observability on), else empty.
	// Deprecated (v1.14.0), as Selector.
	ServiceName string `json:"service_name"`
	// Chart is the Install chart's name, else empty.
	Chart string `json:"chart"`
	// Reason is why forge installs it (required for an adopt-only kind).
	Reason string `json:"reason"`
}

// MLDecisionOutput is one forge.RecommendationDecision.
type MLDecisionOutput struct {
	// ID is the recommendation rule's id.
	ID string `json:"id"`
	// Decision is "dismiss".
	Decision string `json:"decision"`
	// Reason is the claim a reviewer can check.
	Reason string `json:"reason"`
	// Expires is the YYYY-MM-DD past which the recommendation shows again.
	Expires string `json:"expires"`
}

// mlScrapeDeprecationVersion is the release that deprecated the scrape
// fields of forge.Adopt and forge.Install.
const mlScrapeDeprecationVersion = "v1.14.0"

// usesScrapeFields reports whether a component sets the deprecated scrape
// fields of forge.Adopt / forge.Install: a selector or a port (allow and
// service_name need a port, so they never appear alone).
func (c MLComponentOutput) usesScrapeFields() bool {
	return c.Port != 0 || len(c.Selector) > 0
}

// mlScrapeDeprecations are the render's warnings for components that scrape
// through their own fields: one per component, naming the stack, the
// component and the forge.Telemetry that replaces the fields. A warning
// only: the fields keep working within major 1.
func mlScrapeDeprecations(entities *KCLEntities) []string {
	if entities == nil {
		return nil
	}
	var out []string
	for _, s := range entities.MLStacks {
		for _, c := range s.Components {
			if !c.usesScrapeFields() {
				continue
			}
			schema := "forge.Adopt"
			if c.Mode == "install" {
				schema = "forge.Install"
			}
			var fields []string
			if len(c.Selector) > 0 {
				fields = append(fields, "selector")
			}
			if c.Port != 0 {
				fields = append(fields, "port")
			}
			if len(c.Allow) > 0 {
				fields = append(fields, "allow")
			}
			msg := fmt.Sprintf("warning: ml stack %q: %s %q (%s) sets %s, deprecated in %s: declare the scrape as a forge.Telemetry entry in Observability.telemetry "+
				"(service_name, scope = \"platform\", cluster = %q, namespaces, selector, metrics = [forge.MetricsScrape {port = ...}]) and drop the fields from the %s. They still work; removal only in a future major",
				s.Cluster, schema, c.Kind, c.Slot, strings.Join(fields, ", "), mlScrapeDeprecationVersion, s.Cluster, schema)
			if c.ServiceName != "" {
				msg += fmt.Sprintf(". Until then it scrapes as service %q, and a forge.Telemetry of that service_name replaces it", c.ServiceName)
			}
			out = append(out, msg)
		}
	}
	return out
}
