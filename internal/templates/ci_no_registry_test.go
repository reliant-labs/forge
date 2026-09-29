package templates

import (
	"regexp"
	"strings"
	"testing"
)

// A registry is DECLARED as part of a workload's (or frontend's) `image` in
// the project's KCL, and nowhere else — an environment does not have one. A
// scaffolded workflow therefore never names a registry: it logs in with
// `forge registry login <env>`, builds with `forge build <env> --push`, and
// reads the pushed ref back with `forge registry ref <env>` — each of which
// reads the images' own references. A registry host in a workflow, a REGISTRY
// env/var, or a --push carrying a value is a second source of truth that
// drifts from the one the deploy pulls from.
var workflowRegistryLeaks = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a registry host literal", regexp.MustCompile(`(?i)\bghcr\.io\b|\bpkg\.dev\b|\bamazonaws\.com\b|\bdocker\.io\b|\blocalhost:50\d\d\b|\bregistry\.localhost\b`)},
	{"REGISTRY as an env/vars key or reference", regexp.MustCompile(`\b[A-Z_]*REGISTRY[A-Z_]*\s*:|\$\{?[A-Z_]*REGISTRY[A-Z_]*\b|(?:env|vars|secrets)\.[A-Z_]*REGISTRY`)},
	{"--push followed by a value", regexp.MustCompile(`--push[= ]+(?:["'$<]|[A-Za-z0-9-]+[.:/])`)},
	{"docker/login-action (it takes the registry as an input)", regexp.MustCompile(`docker/login-action`)},
	{"docker/metadata-action (it takes the image repository as an input)", regexp.MustCompile(`docker/metadata-action`)},
}

func TestScaffoldedWorkflowsNameNoRegistry(t *testing.T) {
	for name, body := range renderedWorkflows(t) {
		for i, line := range strings.Split(string(body), "\n") {
			code := line
			// A comment may explain what the declaration is for (e.g. "the
			// registry each image names"); only a registry
			// VALUE in the workflow is a leak. Comments still may not carry a
			// host literal: an example host in a comment is copied into code.
			if idx := strings.Index(line, "#"); idx >= 0 && strings.TrimSpace(line[:idx]) == "" {
				code = ""
			}
			for _, leak := range workflowRegistryLeaks {
				target := code
				if leak.what == "a registry host literal" {
					target = line
				}
				if m := leak.re.FindString(target); m != "" {
					t.Errorf("%s:%d names %s (%q) — the registry is declared in the env's KCL, never in a workflow:\n    %s",
						name, i+1, leak.what, m, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestScaffoldedWorkflowsGoThroughForgeForTheRegistry: the three places a
// workflow needs the registry are forge commands that read the declaration.
func TestScaffoldedWorkflowsGoThroughForgeForTheRegistry(t *testing.T) {
	w := renderedWorkflows(t)
	for _, tc := range []struct{ name, want string }{
		{"build-images", `forge registry login "$FORGE_ENV" --username`},
		{"build-images", `forge build "$FORGE_ENV" --target demo --push --tag "$tag"`},
		{"build-images", `forge registry ref "$FORGE_ENV" --github-output`},
		{"build-images", `image-ref: ${{ needs.build-push.outputs.ref }}`},
		{"build-images", `run: cosign sign --yes "$IMAGE_REF"`},
		{"deploy", `forge registry login "${{ matrix.env }}" --username`},
		{"deploy", `run: forge build "${{ matrix.env }}" --push` + "\n"},
	} {
		if !strings.Contains(string(w[tc.name]), tc.want) {
			t.Errorf("%s.yml lacks %q", tc.name, tc.want)
		}
	}
}
