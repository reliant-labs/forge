package templates

import "strings"

// EnvTemplateData is the payload of the env main.k templates
// (deploy/kcl/dev/main.k.tmpl for the local loop, deploy/kcl/cloud/main.k.tmpl
// for staging and prod). An env is a list of per-workload bindings, so the
// templates hold every binder and per-env value; this carries only what they
// cannot know — including Bindings, the one line per workload the generator
// derives from the project's components.
type EnvTemplateData struct {
	ProjectName string
	EnvName     string
	// PrimaryIdent is the KCL identifier of the workload the env's worked
	// examples rebind (`_hosted(wl.<ident>)`).
	PrimaryIdent string
	// PrimaryWorkload names that workload; on the host loop its port is the
	// one the frontend's dev config reads.
	PrimaryWorkload string
	IngressEnabled  bool
	HasFrontend     bool
	FrontendName    string
	// FrontendIdent is the KCL identifier of that frontend: the cloud env
	// declares it once as `_<ident>_frontend` and binds it on one line
	// (`_on_bucket(_<ident>_frontend)`), the line `forge env new --bind`
	// rewrites.
	FrontendIdent string
	// Bindings is the body of the env's `_workloads = [...]` list: one
	// `        _<binder>(wl.<ident>)` line per workload. EnvBinding renders one.
	Bindings string
	// CloudRegistry is the registry literal a cloud env declares on its
	// ClusterTarget. Empty writes the visible ghcr.io/OWNER placeholder. It is
	// a scaffold-time default the author edits in main.k — forge reads the
	// registry from that declaration and nowhere else.
	CloudRegistry string

	// Cluster capacity floor (cloud envs only).
	Replicas         int
	CPURequest       string
	CPULimit         string
	MemoryRequestMiB int
	MemoryLimitMiB   int
}

// withDefaults fills the capacity floor (prod-shaped, halved for an env
// named staging) and the example workload, so a caller states only what it
// knows.
func (d EnvTemplateData) withDefaults() interface{} {
	if d.PrimaryWorkload == "" {
		d.PrimaryWorkload = d.ProjectName
	}
	if d.PrimaryIdent == "" {
		d.PrimaryIdent = d.PrimaryWorkload
	}
	if d.FrontendIdent == "" {
		d.FrontendIdent = strings.NewReplacer("-", "_", ".", "_").Replace(d.FrontendName)
	}
	if d.Replicas == 0 {
		d.Replicas, d.CPURequest, d.CPULimit, d.MemoryRequestMiB, d.MemoryLimitMiB = 3, "0.5", "2.0", 512, 1024
		if d.EnvName == "staging" {
			d.Replicas, d.CPURequest, d.CPULimit, d.MemoryRequestMiB, d.MemoryLimitMiB = 2, "0.25", "1.0", 256, 512
		}
	}
	return d
}
