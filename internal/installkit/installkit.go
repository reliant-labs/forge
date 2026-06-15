// Package installkit holds the small set of genuinely-shared rendering
// primitives used by `internal/packs`.
//
// The packs subsystem owns a real install/upgrade lifecycle (collision
// detection, dep graphs, migrations, generate hooks, audit integration).
// What installkit provides is the unglamorous per-file plumbing —
// path-template evaluation, FS→template→disk writes with overwrite policy,
// name-slug validation, and proto-file detection.
//
// Extracting these primitives keeps that plumbing in one place (consistent
// log strings, validator character classes, and proto-detection rules)
// rather than duplicated across the pack-install paths.
package installkit

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/reliant-labs/forge/internal/templates"
)

// OverwritePolicy controls what RenderAndWrite does when the target file
// already exists on disk.
type OverwritePolicy int

const (
	// Always writes unconditionally — the existing file is clobbered.
	// This is the pack overwrite=always behaviour, suitable for files the
	// pack manages on every install (codegen-shaped outputs).
	Always OverwritePolicy = iota

	// OnceSkip writes only when the target is absent. On re-render the
	// existing file is left untouched and the caller logs a "skipping
	// (already exists)" notice. This is the default for starters (one-time
	// scaffolds) and for pack overwrite=once entries.
	OnceSkip

	// NeverSkip is identical to OnceSkip in behaviour — the existing file
	// is kept and a skip notice is logged. It exists as a distinct value
	// so callers can keep the pack overwrite=never log string distinct
	// from the overwrite=once one (pack code historically printed
	// "Skipping (exists)" for never and "Skipping (already exists)" for
	// once). The behavioural rule is the same; only the log message
	// differs, and that is the caller's responsibility.
	NeverSkip
)

// WriteOpts configures a single RenderAndWrite call.
type WriteOpts struct {
	// OverwritePolicy controls the on-existing-file behaviour. See
	// OverwritePolicy values.
	OverwritePolicy OverwritePolicy

	// LogFunc is the caller-supplied logger used for "Created" and
	// "Skipping" lines. Passing it in lets each caller keep its own log
	// surface stable (the pack and starter tests already grep specific
	// strings) without installkit having to know about every variant.
	//
	// Signature mirrors fmt.Printf. If nil, no log lines are emitted —
	// the caller can still inspect the returned Outcome to print its own.
	LogFunc func(format string, args ...any)

	// DirMode is the permission bits used when the parent directory must
	// be created. Defaults to 0o755 when zero.
	DirMode os.FileMode

	// FileMode is the permission bits used when the file is written.
	// Defaults to 0o644 when zero.
	FileMode os.FileMode
}

// Outcome reports what RenderAndWrite did. Callers use it to increment
// counters, surface follow-ups, and decide whether to mark side-state
// (e.g. PendingProtoGenerate when the output was a .proto file).
type Outcome struct {
	// Wrote is true when the file was rendered and written.
	Wrote bool
	// Skipped is true when the file already existed and the OverwritePolicy
	// instructed installkit to leave it in place.
	Skipped bool
	// ResolvedOutput is the post-template-evaluation relative output path
	// (i.e. the path the caller would log).
	ResolvedOutput string
	// AbsTarget is the absolute on-disk path written or skipped. Set even
	// on skip so callers that want to stat / fingerprint the existing file
	// don't have to recompute the join.
	AbsTarget string
}

// RenderPathTemplate evaluates a Go-template string against data. Plain
// inputs (no `{{`) short-circuit so the common case has no parse cost.
//
// This replaces the inline copy in packs.renderPathTemplate and the
// "copied to avoid a cross-package dependency" copy in
// starters.renderPathTemplate.
func RenderPathTemplate(in string, data map[string]any) (string, error) {
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

// RenderAndWrite is the canonical "render one template, write it to one
// disk path" primitive shared by packs and starters. It:
//
//  1. Renders the output-path template (so callers can write into
//     frontends/{{.FrontendName}}/... without hardcoding a name).
//  2. Joins against projectDir to get the absolute target.
//  3. Honours OverwritePolicy: returns Outcome{Skipped: true} when the
//     target exists and the policy is OnceSkip or NeverSkip.
//  4. Reads the source template body via templates.RenderFromFS, which
//     understands .tmpl suffixes, the shared FuncMap, and //go:build
//     ignore stripping.
//  5. Creates the parent directory and writes the file with the
//     configured modes.
//  6. Logs "Created: <path>" through opts.LogFunc on success.
//
// Caller-specific concerns (collision detection, migration ID
// allocation, post-install tidy, dependency installs) stay with the
// caller — this primitive is only responsible for the per-file path
// from template-to-disk.
func RenderAndWrite(
	fsys fs.FS,
	basePath, tmpl, outRel string,
	projectDir string,
	data map[string]any,
	opts WriteOpts,
) (Outcome, error) {
	resolved, err := RenderPathTemplate(outRel, data)
	if err != nil {
		return Outcome{}, fmt.Errorf("render output path %q: %w", outRel, err)
	}
	target := filepath.Join(projectDir, resolved)
	out := Outcome{ResolvedOutput: resolved, AbsTarget: target}

	if opts.OverwritePolicy == OnceSkip || opts.OverwritePolicy == NeverSkip {
		if _, statErr := os.Stat(target); statErr == nil {
			out.Skipped = true
			return out, nil
		}
	}

	// templates.RenderFromFS joins basePath/name with forward slashes; pass
	// the template name through unchanged so it can land verbatim under
	// "<basePath>/<tmpl>" in the embed.FS.
	content, err := templates.RenderFromFS(fsys, basePath, tmpl, data)
	if err != nil {
		return out, fmt.Errorf("render template %s: %w", tmpl, err)
	}

	dirMode := opts.DirMode
	if dirMode == 0 {
		dirMode = 0o755
	}
	fileMode := opts.FileMode
	if fileMode == 0 {
		fileMode = 0o644
	}

	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return out, fmt.Errorf("create directory for %s: %w", resolved, err)
	}
	if err := os.WriteFile(target, content, fileMode); err != nil {
		return out, fmt.Errorf("write %s: %w", resolved, err)
	}

	if opts.LogFunc != nil {
		opts.LogFunc("  Created: %s\n", resolved)
	}
	out.Wrote = true
	return out, nil
}

// ValidSlug reports whether name is safe to use as a path segment / pack /
// starter identifier. The rule:
//
//   - non-empty
//   - characters in [a-z0-9_-]
//   - first character is not '-' or '_'
//
// This replaces both packs.ValidPackName and starters.ValidStarterName
// (which were already character-for-character identical).
func ValidSlug(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit && c != '-' && c != '_' {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "_")
}

// IsProtoFile reports whether the supplied output path is a `.proto`
// source file. Pack and starter installers use this to flag the
// PendingProtoGenerate side-channel: a freshly-emitted proto file means
// the project's `buf generate` / `forge generate` step must run before
// `go mod tidy` can resolve the (not-yet-generated) gen/<ns>/v1 imports.
//
// Centralising the check keeps the two installers honest if the proto
// suffix or routing convention ever shifts (e.g. "proto/" prefix
// requirement, additional extensions).
func IsProtoFile(out string) bool {
	return strings.HasSuffix(out, ".proto")
}

// FirstByteIndex returns the first index of c in s, or -1 if absent.
// Inlined to avoid pulling in `strings` just for one call from the CLI
// helpers that truncate descriptions on the first newline.
func FirstByteIndex(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// joinForFS is unused outside the package but kept for symmetry with
// templates.RenderFromFS which already does the forward-slash join. If
// callers ever need a stable basePath/name resolution outside
// RenderAndWrite they should reach for path.Join with forward slashes
// (embed.FS is forward-slash-only regardless of host OS).
var _ = path.Join
