package cli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/config"
)

// googleapisBufDep is the BSR module that holds google/api/annotations.proto,
// the import every google.api.http annotation needs.
const googleapisBufDep = "buf.build/googleapis/googleapis"

var (
	// restListenerOff / restListenerOn match the graph-mode providers.go's
	// serverkit.ListenerDeps field, born `REST: <api.rest at scaffold>`.
	restListenerOff = regexp.MustCompile(`\bREST:\s*false\b`)
	restListenerOn  = regexp.MustCompile(`\bREST:\s*true\b`)
	// restHandlerCall is the legacy serve.go's REST swap.
	restHandlerCall = regexp.MustCompile(`\bRESTHandler\(`)
	httpAnnotation  = regexp.MustCompile(`\(\s*google\.api\.http\s*\)`)
)

// restWiringWarnings reports, for a project with `api.rest: true`, each piece
// of REST wiring that is missing, naming the manual edit that adds it.
//
// Every piece lives in a file the project owns: the server's REST switch
// (internal/app/providers.go, or cmd/<bin>/cmd/serve.go in a project that
// predates the composition graph) is scaffolded once with the api.rest value
// of its birth, buf.yaml is Tier-2 (hand edits respected), and the
// annotations are the user's proto. Flipping api.rest on after scaffold
// therefore changes none of them, and forge does not rewrite them — so
// generate says what to change instead of claiming REST is on (obs 4e9ce6c3).
//
// Advisory: a check that cannot read its file reports nothing.
func restWiringWarnings(projectDir string, cfg *config.ProjectConfig, hasServices bool) []string {
	if cfg == nil || !cfg.API.REST || !hasServices {
		return nil
	}
	var warnings []string
	if w := restServerWarning(projectDir); w != "" {
		warnings = append(warnings, w)
	}
	if w := restBufDepWarning(projectDir); w != "" {
		warnings = append(warnings, w)
	}
	if w := restAnnotationWarning(projectDir); w != "" {
		warnings = append(warnings, w)
	}
	return warnings
}

// restServerWarning: the server serves REST when some Go file under
// internal/app/ or cmd/ sets `REST: true` or calls RESTHandler. It is unwired
// when a file sets `REST: false` and none turns it on, or when neither
// spelling appears anywhere (a legacy serve.go born without the swap).
func restServerWarning(projectDir string) string {
	var off, on bool
	offAt := ""
	for _, dir := range []string{filepath.Join("internal", "app"), "cmd"} {
		_ = filepath.WalkDir(filepath.Join(projectDir, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			if restListenerOn.Match(src) || restHandlerCall.Match(src) {
				on = true
			}
			if loc := restListenerOff.FindIndex(src); loc != nil {
				off = true
				if offAt == "" {
					rel, _ := filepath.Rel(projectDir, p)
					offAt = fmt.Sprintf("%s:%d", filepath.ToSlash(rel), 1+strings.Count(string(src[:loc[0]]), "\n"))
				}
			}
			return nil
		})
	}
	switch {
	case on:
		return ""
	case off:
		return fmt.Sprintf("api.rest: true, but the server does not serve REST: %s sets `REST: false` in serverkit.ListenerDeps. "+
			"Set it to `REST: true` (the file is yours; forge never rewrites it).", offAt)
	default:
		return "api.rest: true, but nothing under cmd/ or internal/app/ serves REST. " +
			"In cmd/<bin>/cmd/serve.go, after the services are mounted, add " +
			"`if rest := serverkit.RESTHandler(srv.Mux, mounted, logger); rest != nil { srv.Handler = rest }` " +
			"(or `REST: true` in internal/app/providers.go's serverkit.ListenerDeps, in a composition-graph project)."
	}
}

// restBufDepWarning: google/api/annotations.proto resolves only when buf.yaml
// declares the googleapis dep and buf.lock pins it.
func restBufDepWarning(projectDir string) string {
	data, err := os.ReadFile(filepath.Join(projectDir, "buf.yaml"))
	if err != nil {
		return ""
	}
	var buf struct {
		Deps []string `yaml:"deps"`
	}
	if yaml.Unmarshal(data, &buf) != nil {
		return ""
	}
	for _, d := range buf.Deps {
		if strings.TrimSpace(strings.SplitN(d, ":", 2)[0]) == googleapisBufDep {
			if fileExists(filepath.Join(projectDir, "buf.lock")) {
				return ""
			}
			return "api.rest: true, and buf.yaml declares " + googleapisBufDep + ", but there is no buf.lock: " +
				"run `buf dep update` so `import \"google/api/annotations.proto\"` resolves."
		}
	}
	return "api.rest: true, but buf.yaml has no " + googleapisBufDep + " dep, so `import \"google/api/annotations.proto\"` cannot resolve. " +
		"Add `deps: [" + googleapisBufDep + "]` to buf.yaml (yours; forge does not rewrite an edited buf.yaml), then run `buf dep update` to write buf.lock."
}

// restAnnotationWarning: vanguard routes only rpcs that carry a
// google.api.http option; with none, REST serves no routes.
func restAnnotationWarning(projectDir string) string {
	found := false
	_ = filepath.WalkDir(filepath.Join(projectDir, "proto", "services"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || found || d.IsDir() || !strings.HasSuffix(p, ".proto") {
			return nil
		}
		if src, err := os.ReadFile(p); err == nil && httpAnnotation.Match(src) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	if found || !dirExists(filepath.Join(projectDir, "proto", "services")) {
		return ""
	}
	return "api.rest: true, but no rpc under proto/services/ has a `google.api.http` option, so REST serves no routes. " +
		"Add `option (google.api.http) = { get: \"/v1/<entities>/{id}\" };` (and so on) to each rpc to expose, " +
		"with `import \"google/api/annotations.proto\";` — `forge skill load api-rest` lists the CRUD defaults."
}
