// Package starters implements the starter-scaffold system: one-time
// copies of opinionated, working code into a project that the user
// owns thereafter. Unlike packs, starters have no install/upgrade
// lifecycle — `forge starter add` writes files and exits.
//
// Starters are the right shape for **business integrations** (Stripe
// billing, Twilio SMS, Clerk webhook user-sync) where every project
// customizes the code anyway and central maintenance creates more
// bugs than it prevents. Pure-infrastructure scaffolds (auth
// middleware, idempotency, JWKS rotation) stay as packs because their
// invariants benefit from forge keeping them up to date.
package starters

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/templates"
)

//go:embed all:stripe all:twilio all:clerk-webhook
var startersFS embed.FS

// FS exposes the embedded starter templates filesystem so external
// callers (notably tests) can read template bodies. The exported
// accessor keeps the embed.FS itself unexported.
func FS() fs.FS { return startersFS }

// Starter is a single one-time scaffold the user can copy into a
// project. It carries enough metadata to render templates, surface
// the dependency list (echoed to the user — NOT auto-installed), and
// print a post-scaffold note.
type Starter struct {
	// Name is the slug used by `forge starter add <name>`. Must match
	// the embed directory under internal/starters/<name>/.
	Name string `yaml:"name"`
	// Description is the one-line summary surfaced by `forge starter
	// list`.
	Description string `yaml:"description"`
	// Deps lists the language-keyed dependencies the user must add
	// after scaffolding. Forge does NOT auto-run `go get` / `npm
	// install` — starters are user-owned, so dependency churn is
	// theirs too.
	Deps StarterDeps `yaml:"deps"`
	// Files is the ordered list of template→destination pairs to
	// render. Both source and destination are Go-template strings so
	// callers can pass `--service` etc.
	Files []StarterFile `yaml:"files"`
	// Notes is a multi-line string printed after the scaffold lands.
	// Use it to flag environment variables, follow-up steps, or
	// version-pinning advice.
	Notes string `yaml:"notes"`
}

// StarterDeps groups dependency hints by language. Each list is
// printed verbatim — the user adds them to their own go.mod /
// package.json on their schedule.
type StarterDeps struct {
	Go  []string `yaml:"go"`
	NPM []string `yaml:"npm"`
}

// StarterFile maps one embedded template to one output path inside
// the project. Both fields are Go-template strings so the destination
// can route based on `--service <svc>` or other flags.
type StarterFile struct {
	Source      string `yaml:"source"`
	Destination string `yaml:"destination"`
}

// LoadStarter reads a starter manifest from the embedded filesystem.
// Returns a wrapped error if the directory is missing or the manifest
// fails to parse.
func LoadStarter(name string) (*Starter, error) {
	if !ValidStarterName(name) {
		return nil, fmt.Errorf("invalid starter name %q", name)
	}
	data, err := startersFS.ReadFile(path.Join(name, "starter.yaml"))
	if err != nil {
		return nil, fmt.Errorf("starter %q not found: %w", name, err)
	}
	var s Starter
	if err := yaml.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse starter %q manifest: %w", name, err)
	}
	if s.Name == "" {
		s.Name = name
	}
	return &s, nil
}

// ListStarters returns every starter shipped with this forge build,
// sorted alphabetically by name.
func ListStarters() ([]Starter, error) {
	entries, err := startersFS.ReadDir(".")
	if err != nil {
		return nil, fmt.Errorf("read starters root: %w", err)
	}
	var out []Starter
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := LoadStarter(e.Name())
		if err != nil {
			// A directory without a valid manifest is skipped (matches
			// the pack registry's leniency).
			continue
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// AddOptions configures a single `forge starter add` invocation.
type AddOptions struct {
	// ProjectDir is the project root (where forge.yaml lives).
	ProjectDir string
	// ModulePath is the Go module path, surfaced to templates as
	// {{.ModulePath}}.
	ModulePath string
	// ProjectName surfaces as {{.ProjectName}}.
	ProjectName string
	// Service is the target service slug used for routing destination
	// paths like "handlers/{{.Service}}/stripe_webhook.go". May be
	// empty if a starter doesn't need a service context.
	Service string
	// Force overwrites existing files instead of skipping them. Off
	// by default — starters are intentionally "once" by nature, the
	// user owns the file after the first copy.
	Force bool
	// Stdout is where progress messages are written. Defaults to
	// os.Stdout when nil.
	Stdout interface {
		Write(p []byte) (int, error)
	}
}

// Add renders every file in the starter into the project. The
// operation is intentionally minimal — no forge.yaml mutation, no
// dependency install, no migration allocation. The user owns the
// scaffold from the first byte forward.
//
// On a re-run, files that already exist are skipped (with a notice)
// unless opts.Force is set. This mirrors the "starters are one-time"
// contract: forge does not roll the user's customizations back.
func (s *Starter) Add(opts AddOptions) error {
	if opts.ProjectDir == "" {
		return fmt.Errorf("ProjectDir is required")
	}

	out := opts.Stdout
	if out == nil {
		out = os.Stdout
	}
	logf := func(format string, a ...any) { fmt.Fprintf(out, format, a...) }

	data := map[string]any{
		"ModulePath":  opts.ModulePath,
		"ProjectName": opts.ProjectName,
		"Service":     opts.Service,
	}

	for _, f := range s.Files {
		dest, err := renderPathTemplate(f.Destination, data)
		if err != nil {
			return fmt.Errorf("render destination %q: %w", f.Destination, err)
		}
		target := filepath.Join(opts.ProjectDir, dest)

		if !opts.Force {
			if _, err := os.Stat(target); err == nil {
				logf("  Skipping (exists): %s\n", dest)
				continue
			}
		}

		basePath := path.Join(s.Name, "templates")
		content, err := templates.RenderFromFS(startersFS, basePath, f.Source, data)
		if err != nil {
			return fmt.Errorf("render template %s: %w", f.Source, err)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create directory for %s: %w", dest, err)
		}
		if err := os.WriteFile(target, content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", dest, err)
		}
		logf("  Created: %s\n", dest)
	}

	if len(s.Deps.Go) > 0 {
		logf("\nGo dependencies (add to go.mod yourself, e.g. `go get`):\n")
		for _, d := range s.Deps.Go {
			logf("  - %s\n", d)
		}
	}
	if len(s.Deps.NPM) > 0 {
		logf("\nnpm dependencies (add to your frontend package.json):\n")
		for _, d := range s.Deps.NPM {
			logf("  - %s\n", d)
		}
	}
	if strings.TrimSpace(s.Notes) != "" {
		logf("\nNotes:\n%s\n", strings.TrimRight(s.Notes, "\n"))
	}
	return nil
}

// ValidStarterName mirrors packs.ValidPackName — the starter slug
// must be safe enough to use as a path segment.
func ValidStarterName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "_")
}

// renderPathTemplate is the same plain-string short-circuited Go
// template helper packs/pack.go uses, copied to avoid a cross-package
// dependency on a private symbol.
func renderPathTemplate(in string, data map[string]any) (string, error) {
	if !strings.Contains(in, "{{") {
		return in, nil
	}
	tmpl, err := template.New("path").Parse(in)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
