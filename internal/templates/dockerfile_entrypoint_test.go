package templates

import (
	"regexp"
	"strings"
	"testing"
)

// TestDockerfile_ArgsSelectTheSubcommand pins the contract every runtime
// relies on (ADR 0002) for the debug image: a workload's `args` select a
// subcommand of the project binary. In a pod, Kubernetes `args` REPLACE the
// image's CMD, so the binary rides in the ENTRYPOINT (here, after dlv's `--`)
// and only the default subcommand in CMD.
//
// The project image's half of the contract is forge.yaml's `image:` block
// (default entrypoint /app/<binary>, scaffolded cmd [server]); see
// internal/generator's TestScaffoldDeclaresTheProjectImage.
func TestDockerfile_ArgsSelectTheSubcommand(t *testing.T) {
	src := string(renderProject(t, "Dockerfile.tmpl", projectData()))
	debug := src[strings.Index(src, "AS debug\n"):]
	if !strings.Contains(debug, `"/app/demo", "--"]`) || !regexp.MustCompile(`(?m)^CMD \["server"\]$`).MatchString(debug) {
		t.Errorf("debug stage must pass the subcommand (CMD) to the program after dlv's `--`:\n%s", debug)
	}
}

// The project image is assembled from forge.yaml, so the scaffolded
// Dockerfile must not carry a production stage: two recipes for one image
// drift, and `forge build` would never read this one.
func TestDockerfile_HasNoProductionStage(t *testing.T) {
	src := string(renderProject(t, "Dockerfile.tmpl", projectData()))
	if strings.Contains(src, "AS production") {
		t.Errorf("scaffolded Dockerfile still has a production stage:\n%s", src)
	}
}
