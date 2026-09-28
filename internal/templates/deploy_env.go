package templates

import "fmt"

// The runtimes a scaffolded env can be born on — the value of
// `forge env new --runtime` and the template each selects. They name the
// env's DEFAULT runtime (`Bundle.runtime`); any env may still bind a single
// workload elsewhere, because the runtime is chosen per workload (ADR 0002).
const (
	EnvRuntimeHost    = "host"
	EnvRuntimeCluster = "cluster"
	EnvRuntimeHosted  = "hosted"
)

// EnvRuntimes lists the runtimes a scaffolded env can default to.
var EnvRuntimes = []string{EnvRuntimeHost, EnvRuntimeCluster, EnvRuntimeHosted}

// EnvTemplateData is the payload of the deploy/kcl/env/<runtime>.k.tmpl
// templates: one environment's main.k. The templates hold every binding and
// per-env value; this carries only what they cannot know.
type EnvTemplateData struct {
	ProjectName string
	EnvName     string
	// PrimaryWorkload names the workload the env's worked examples refine,
	// and on the host runtime the one whose port the frontend's dev config
	// reads. It should be a workload the project declares.
	PrimaryWorkload string
	// HasPrimaryService reports whether PrimaryWorkload is a real service
	// (a hosted frontend's runtime_config names it with forge.WorkloadURL,
	// which must resolve).
	HasPrimaryService bool
	IngressEnabled    bool
	HasFrontend       bool
	FrontendName      string
	// FrontendType is the frontend's forge.Frontend.type (nextjs|vite).
	FrontendType string
	// FrontendPublicDir is the static export's output directory.
	FrontendPublicDir string
	// HasDatabase declares a forge.ManagedDatabase on the hosted runtime.
	HasDatabase bool

	// Cluster capacity floor (cluster runtime only).
	Replicas         int
	CPURequest       string
	CPULimit         string
	MemoryRequestMiB int
	MemoryLimitMiB   int
}

// EnvTemplateName is the template that scaffolds an env on runtime.
func EnvTemplateName(runtime string) (string, error) {
	switch runtime {
	case EnvRuntimeHost, EnvRuntimeCluster, EnvRuntimeHosted:
		return "kcl/env/" + runtime + ".k.tmpl", nil
	}
	return "", fmt.Errorf("unknown env runtime %q: want one of %v", runtime, EnvRuntimes)
}

// withDefaults fills the capacity floor (prod-shaped, halved for any env
// named staging) and the frontend defaults, so a caller states only what it
// knows.
func (d EnvTemplateData) withDefaults() interface{} {
	if d.PrimaryWorkload == "" {
		d.PrimaryWorkload = d.ProjectName
	}
	if d.FrontendType == "" {
		d.FrontendType = "nextjs"
	}
	if d.FrontendPublicDir == "" {
		d.FrontendPublicDir = "out"
		if d.FrontendType == "vite" {
			d.FrontendPublicDir = "dist"
		}
	}
	if d.Replicas == 0 {
		d.Replicas, d.CPURequest, d.CPULimit, d.MemoryRequestMiB, d.MemoryLimitMiB = 3, "0.5", "2.0", 512, 1024
		if d.EnvName == "staging" {
			d.Replicas, d.CPURequest, d.CPULimit, d.MemoryRequestMiB, d.MemoryLimitMiB = 2, "0.25", "1.0", 256, 512
		}
	}
	return d
}
