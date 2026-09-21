package templates

import (
	"strings"
	"testing"
)

// TestEveryEnvDeclaresTheFrontendCapability pins the UX property that the
// deploy templates exist to carry, and it is a UX property rather than a
// correctness one — which is exactly why it needs a test.
//
// forge.Frontend.deploy accepts FirebaseHosting, StaticSite or K8sCluster.
// That capability was real long before this test, and was declared in the DEV
// template only: staging and prod emitted no frontend workload at all. So the
// only way to discover it was to read kcl/schema.k, and a user who never did
// shipped a backend to prod and served their frontend by hand — never learning
// forge could do it. A capability nobody can find is one that does not exist.
//
// The assertion is deliberately about the SCAFFOLD's output rather than the
// schema: the schema already allowed this and that changed nothing.
func TestEveryEnvDeclaresTheFrontendCapability(t *testing.T) {
	t.Parallel()

	data := struct {
		ProjectName     string
		IngressEnabled  bool
		HasFrontend     bool
		PrimaryWorkload string
		FrontendName    string
	}{ProjectName: "acme", IngressEnabled: true, HasFrontend: true, PrimaryWorkload: "acme", FrontendName: "web"}

	for _, tmpl := range []string{
		"kcl/dev/main.k.tmpl",
		"kcl/staging/main.k.tmpl",
		"kcl/prod/main.k.tmpl",
	} {
		t.Run(tmpl, func(t *testing.T) {
			t.Parallel()

			out, err := DeployTemplates().Render(tmpl, data)
			if err != nil {
				t.Fatalf("rendering %s: %v", tmpl, err)
			}
			rendered := string(out)

			if !strings.Contains(rendered, "frontends = [forge.Frontend {") {
				t.Fatalf("%s declares NO frontend workload for a project that has one. "+
					"Staging and prod were silent about frontends for exactly this reason, "+
					"and the result was a capability only readable in kcl/schema.k", tmpl)
			}
			if !strings.Contains(rendered, `name = "web"`) {
				t.Fatalf("%s did not substitute FrontendName", tmpl)
			}
		})
	}
}

// TestFrontendBlockIsAbsentWithoutAFrontend is the counterweight. Without it
// the test above is satisfiable by emitting the block unconditionally, which
// would put a forge.Frontend naming an empty path into every backend-only
// project — a render error at best and a confusing one at worst.
func TestFrontendBlockIsAbsentWithoutAFrontend(t *testing.T) {
	t.Parallel()

	data := struct {
		ProjectName     string
		IngressEnabled  bool
		HasFrontend     bool
		PrimaryWorkload string
		FrontendName    string
	}{ProjectName: "acme", IngressEnabled: true, HasFrontend: false, PrimaryWorkload: "acme"}

	for _, tmpl := range []string{
		"kcl/dev/main.k.tmpl",
		"kcl/staging/main.k.tmpl",
		"kcl/prod/main.k.tmpl",
	} {
		out, err := DeployTemplates().Render(tmpl, data)
		if err != nil {
			t.Fatalf("rendering %s: %v", tmpl, err)
		}
		if strings.Contains(string(out), "frontends = [forge.Frontend {") {
			t.Fatalf("%s emitted a frontend workload for a project with no frontend", tmpl)
		}
	}
}

// TestFrontendScaffoldChoosesNoDeployTarget pins an absence that is a design
// decision rather than an omission.
//
// forge does not guess where a frontend belongs. A default would deploy
// somewhere plausible and cost money there, and the user would discover the
// choice by receiving a bill. The scaffolded block therefore names the
// options in comments and commits to none.
func TestFrontendScaffoldChoosesNoDeployTarget(t *testing.T) {
	t.Parallel()

	data := struct {
		ProjectName     string
		IngressEnabled  bool
		HasFrontend     bool
		PrimaryWorkload string
		FrontendName    string
	}{ProjectName: "acme", IngressEnabled: true, HasFrontend: true, PrimaryWorkload: "acme", FrontendName: "web"}

	for _, tmpl := range []string{"kcl/staging/main.k.tmpl", "kcl/prod/main.k.tmpl"} {
		out, err := DeployTemplates().Render(tmpl, data)
		if err != nil {
			t.Fatalf("rendering %s: %v", tmpl, err)
		}
		rendered := string(out)

		// Find the frontends block and confirm no ACTIVE deploy assignment
		// inside it. Commented mentions are the point; an uncommented one is
		// forge choosing for the user.
		idx := strings.Index(rendered, "frontends = [forge.Frontend {")
		if idx < 0 {
			t.Fatalf("%s: no frontend block", tmpl)
		}
		block := rendered[idx:]
		if end := strings.Index(block, "}]"); end > 0 {
			block = block[:end]
		}
		for _, line := range strings.Split(block, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			if strings.Contains(trimmed, "deploy") {
				t.Fatalf("%s picked a deploy target for the user: %q", tmpl, trimmed)
			}
		}
	}
}
