// Package cli — `forge friction` command.
//
// Friction formalizes the downstream→upstream feedback loop: when an
// agent (or human) working in a forge project hits generator friction —
// a lint false-positive, a scaffold quirk, a missing helper — the
// observation is captured AT THE MOMENT OF FRICTION with one dumb,
// durable, local command:
//
//	forge friction add "wire_gen drops Deps fields added after scaffold" \
//	  --severity p1 --area codegen --source fix-validate-agent \
//	  --context pkg/app/wire_gen.go:42
//
// Records land in `.forge/friction.jsonl` — append-only JSONL, one
// self-contained object per line. JSONL is deliberate:
//
//   - appends are atomic-ish (single O_APPEND write, no read-modify-write)
//   - merge-friendly (git unions lines; no rewrite conflicts)
//   - immune to the markdown-rewrite failure mode that lost ~65 findings
//     in the field (an LLM batching prose appends got rate-limited
//     mid-rewrite and dropped the batch)
//
// No LLM, no network, no rewriting of existing lines, ever. Reading
// tolerates (skips + counts) malformed lines so a torn write can never
// brick the log.
//
// `forge friction list` filters/renders the log, `forge friction export`
// renders FRICTION.md-style markdown to stdout (projects that want a
// checked-in FRICTION.md redirect it; forge does not own that file), and
// `forge audit --json` surfaces a summary under the additive `friction`
// category so standing friction is visible where agents already look.
package cli

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// frictionFileRelPath is where the log lives, relative to the project
// root. It sits next to checksums.json and — like checksums.json — is
// negated back INTO version control by the project .gitignore template:
// friction is shared project state that travels with the repo.
const frictionFileRelPath = ".forge/friction.jsonl"

// frictionSeverities is the closed severity enum, in render order.
var frictionSeverities = []string{"p0", "p1", "p2", "note"}

// frictionSchemaVersion is stamped on every record. The record shape is
// an API: once entries flow upstream (harness sync, a future
// `forge friction submit`), producers and consumers version-skew freely.
// Contract: additive-only within a version — never repurpose or remove a
// tagged field; bump this integer only for a genuinely breaking reshape,
// and keep readers accepting all prior versions.
const frictionSchemaVersion = 1

// FrictionEntry is one record in .forge/friction.jsonl. Every entry is
// self-contained (no cross-line references) so lines can be unioned,
// reordered, or truncated without corrupting neighbours. Field tags are
// stable — downstream tooling parses this; see frictionSchemaVersion for
// the evolution contract.
type FrictionEntry struct {
	// Schema is the record-shape version (frictionSchemaVersion at write
	// time). Readers treat absent/zero as version 1.
	Schema int `json:"schema"`
	// ID is a short content hash ("fr-" + 10 hex chars) over the
	// recorded-at instant and the entry payload. Content-derived (not a
	// counter) so concurrent writers and merged branches can't collide
	// on allocation.
	ID string `json:"id"`
	// RecordedAt is the capture instant, RFC3339 UTC.
	RecordedAt time.Time `json:"recorded_at"`
	// ForgeVersion is the binary that recorded the entry (buildinfo) —
	// lets upstream triage check whether the friction predates a fix.
	ForgeVersion string `json:"forge_version"`
	// Severity is one of p0 | p1 | p2 | note.
	Severity string `json:"severity"`
	// Area is a free-form tag (codegen, frontend, deploy, ...).
	Area string `json:"area,omitempty"`
	// Source is a free-form origin tag (agent name, workflow id, human).
	Source string `json:"source,omitempty"`
	// Context holds file:line refs or commands that anchor the entry.
	Context []string `json:"context,omitempty"`
	// Text is the friction description.
	Text string `json:"text"`
}

// newFrictionCmd is the top-level `forge friction` group.
func newFrictionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "friction",
		Short: "Capture and inspect generator-friction reports (.forge/friction.jsonl)",
		Long: `Capture and inspect generator-friction reports.

When you hit forge friction (a generator bug, a lint false-positive, a
missing library helper), record it immediately:

  forge friction add "describe the friction" --severity p1 --area codegen

Records append to .forge/friction.jsonl — append-only JSONL that is
committed with the repo and never rewritten. Use 'list' to filter and
'export' to render FRICTION.md-style markdown to stdout.`,
	}
	cmd.AddCommand(newFrictionAddCmd())
	cmd.AddCommand(newFrictionListCmd())
	cmd.AddCommand(newFrictionExportCmd())
	return cmd
}

// ─── add ────────────────────────────────────────────────────────────────

func newFrictionAddCmd() *cobra.Command {
	var (
		severity string
		area     string
		source   string
		contexts []string
	)
	cmd := &cobra.Command{
		Use:   "add <text>",
		Short: "Append one friction record (use '-' to read text from stdin)",
		Long: `Append one friction record to .forge/friction.jsonl.

The text is a free-form description of the friction. Pass '-' as the
text argument to read it from stdin (long entries). The write is a
single append — no existing line is ever read back or rewritten.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := args[0]
			if text == "-" {
				data, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				text = string(data)
			}
			text = strings.TrimSpace(text)
			if text == "" {
				return fmt.Errorf("friction text is empty")
			}
			if !validFrictionSeverity(severity) {
				return fmt.Errorf("invalid --severity %q (want p0 | p1 | p2 | note)", severity)
			}

			entry := FrictionEntry{
				Schema:       frictionSchemaVersion,
				RecordedAt:   time.Now().UTC().Truncate(time.Second),
				ForgeVersion: buildinfo.Version(),
				Severity:     severity,
				Area:         area,
				Source:       source,
				Context:      contexts,
				Text:         text,
			}
			entry.ID = frictionID(entry)

			path, err := frictionFilePath()
			if err != nil {
				return err
			}
			if err := appendFrictionEntry(path, entry); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "recorded %s (severity=%s) -> %s\n",
				entry.ID, entry.Severity, frictionFileRelPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&severity, "severity", "note", "Severity: p0 | p1 | p2 | note")
	cmd.Flags().StringVar(&area, "area", "", "Free-form area tag (e.g. codegen, frontend, deploy)")
	cmd.Flags().StringVar(&source, "source", "", "Free-form source tag (e.g. agent name, workflow id)")
	cmd.Flags().StringArrayVar(&contexts, "context", nil, "file:line or command anchoring the entry (repeatable)")
	return cmd
}

func validFrictionSeverity(s string) bool {
	for _, v := range frictionSeverities {
		if s == v {
			return true
		}
	}
	return false
}

// frictionID derives the short content-hash id. The recorded-at nanos
// salt means two identical texts recorded at different moments get
// distinct ids; identical text in the same instant collapsing to one id
// is fine (it IS the same observation).
func frictionID(e FrictionEntry) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s|%s|%s", e.RecordedAt.UnixNano(), e.Severity, e.Area, e.Source, e.Text)
	return "fr-" + hex.EncodeToString(h.Sum(nil))[:10]
}

// frictionFilePath resolves <project root>/.forge/friction.jsonl,
// falling back to the cwd when no forge.yaml is found (capture must
// never fail just because the agent is in a subdirectory of nowhere).
func frictionFilePath() (string, error) {
	root, err := findProjectRoot()
	if err != nil {
		return "", err
	}
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(root, filepath.FromSlash(frictionFileRelPath)), nil
}

// appendFrictionEntry marshals the entry to one line and appends it.
// Single O_APPEND write; never reads or rewrites existing content.
func appendFrictionEntry(path string, entry FrictionEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal friction entry: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("append %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// loadFrictionEntries reads the JSONL log, skipping (and counting)
// malformed lines — a torn write must never brick the log. A missing
// file is zero entries, not an error.
func loadFrictionEntries(path string) (entries []FrictionEntry, malformed int, err error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	// Long entries (stdin-fed prose) can exceed the default 64KiB token.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var e FrictionEntry
		if uerr := json.Unmarshal([]byte(line), &e); uerr != nil || e.Text == "" {
			malformed++
			continue
		}
		entries = append(entries, e)
	}
	if serr := scanner.Err(); serr != nil {
		return entries, malformed, fmt.Errorf("scan %s: %w", path, serr)
	}
	return entries, malformed, nil
}

// ─── list ───────────────────────────────────────────────────────────────

func newFrictionListCmd() *cobra.Command {
	var (
		jsonOut  bool
		severity string
		area     string
		since    string
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List friction records (filterable by severity, area, recency)",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := frictionFilePath()
			if err != nil {
				return err
			}
			entries, malformed, err := loadFrictionEntries(path)
			if err != nil {
				return err
			}
			entries, err = filterFriction(entries, severity, area, since)
			if err != nil {
				return err
			}
			if malformed > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipped %d malformed line(s) in %s\n",
					malformed, frictionFileRelPath)
			}
			if jsonOut {
				return writeFrictionJSON(cmd.OutOrStdout(), entries)
			}
			return writeFrictionTable(cmd.OutOrStdout(), entries)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output a raw JSON array instead of a table")
	cmd.Flags().StringVar(&severity, "severity", "", "Filter by severity (p0 | p1 | p2 | note)")
	cmd.Flags().StringVar(&area, "area", "", "Filter by area tag")
	cmd.Flags().StringVar(&since, "since", "", "Only entries recorded after this RFC3339 timestamp or within this duration (e.g. 72h)")
	return cmd
}

// filterFriction applies severity/area/since filters. `since` accepts a
// Go duration ("72h") or an RFC3339 timestamp.
func filterFriction(entries []FrictionEntry, severity, area, since string) ([]FrictionEntry, error) {
	var cutoff time.Time
	if since != "" {
		if d, derr := time.ParseDuration(since); derr == nil {
			cutoff = time.Now().UTC().Add(-d)
		} else if ts, terr := time.Parse(time.RFC3339, since); terr == nil {
			cutoff = ts
		} else {
			return nil, fmt.Errorf("invalid --since %q (want RFC3339 timestamp or duration like 72h)", since)
		}
	}
	out := make([]FrictionEntry, 0, len(entries))
	for _, e := range entries {
		if severity != "" && !strings.EqualFold(e.Severity, severity) {
			continue
		}
		if area != "" && !strings.EqualFold(e.Area, area) {
			continue
		}
		if !cutoff.IsZero() && e.RecordedAt.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func writeFrictionJSON(w io.Writer, entries []FrictionEntry) error {
	if entries == nil {
		entries = []FrictionEntry{} // emit [] not null — consumers iterate
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}

func writeFrictionTable(w io.Writer, entries []FrictionEntry) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tRECORDED\tSEVERITY\tAREA\tSOURCE\tTEXT")
	for _, e := range entries {
		area := e.Area
		if area == "" {
			area = "-"
		}
		source := e.Source
		if source == "" {
			source = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.ID, e.RecordedAt.Format(time.RFC3339), e.Severity, area, source,
			truncateFrictionText(e.Text, 80))
	}
	return tw.Flush()
}

// truncateFrictionText flattens newlines and clips long prose for the
// table view; full text is always available via --json / export.
func truncateFrictionText(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// ─── export ─────────────────────────────────────────────────────────────

func newFrictionExportCmd() *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Render the friction log as markdown (grouped by severity, then area) to stdout",
		Long: `Render the friction log as human-readable markdown to stdout.

Projects that want a checked-in FRICTION.md can redirect the output;
forge does not own that file:

  forge friction export > FRICTION.md`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if format != "md" {
				return fmt.Errorf("unsupported --format %q (only: md)", format)
			}
			path, err := frictionFilePath()
			if err != nil {
				return err
			}
			entries, malformed, err := loadFrictionEntries(path)
			if err != nil {
				return err
			}
			if malformed > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: skipped %d malformed line(s) in %s\n",
					malformed, frictionFileRelPath)
			}
			return writeFrictionMarkdown(cmd.OutOrStdout(), entries)
		},
	}
	cmd.Flags().StringVar(&format, "format", "md", "Output format (md)")
	return cmd
}

// writeFrictionMarkdown groups entries severity-first (p0, p1, p2,
// note, then any out-of-enum severities alphabetically), area-second.
func writeFrictionMarkdown(w io.Writer, entries []FrictionEntry) error {
	_, _ = fmt.Fprintf(w, "# Friction log\n\n")
	_, _ = fmt.Fprintf(w, "%d entr%s rendered from `%s` by `forge friction export`. Do not edit by hand — add entries with `forge friction add`.\n",
		len(entries), pluralIES(len(entries)), frictionFileRelPath)

	bySeverity := map[string][]FrictionEntry{}
	for _, e := range entries {
		bySeverity[strings.ToLower(e.Severity)] = append(bySeverity[strings.ToLower(e.Severity)], e)
	}

	order := append([]string{}, frictionSeverities...)
	var extras []string
	for sev := range bySeverity {
		if !validFrictionSeverity(sev) {
			extras = append(extras, sev)
		}
	}
	sort.Strings(extras)
	order = append(order, extras...)

	for _, sev := range order {
		group := bySeverity[sev]
		if len(group) == 0 {
			continue
		}
		_, _ = fmt.Fprintf(w, "\n## %s\n", strings.ToUpper(sev))

		byArea := map[string][]FrictionEntry{}
		for _, e := range group {
			byArea[e.Area] = append(byArea[e.Area], e)
		}
		areas := make([]string, 0, len(byArea))
		for a := range byArea {
			areas = append(areas, a)
		}
		sort.Strings(areas)

		for _, a := range areas {
			label := a
			if label == "" {
				label = "(unspecified)"
			}
			_, _ = fmt.Fprintf(w, "\n### %s\n\n", label)
			for _, e := range byArea[a] {
				_, _ = fmt.Fprintf(w, "- **[%s]** %s\n", e.ID, e.Text)
				meta := []string{"recorded " + e.RecordedAt.Format(time.RFC3339), "forge " + e.ForgeVersion}
				if e.Source != "" {
					meta = append(meta, "source "+e.Source)
				}
				_, _ = fmt.Fprintf(w, "  - %s\n", strings.Join(meta, " · "))
				for _, c := range e.Context {
					_, _ = fmt.Fprintf(w, "  - context: `%s`\n", c)
				}
			}
		}
	}
	return nil
}

func pluralIES(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// ─── audit integration ──────────────────────────────────────────────────

// auditFriction summarizes .forge/friction.jsonl for `forge audit` —
// count by severity, newest entry timestamp, and the hint to run
// `forge friction list`. The category is additive per the audit-json
// contract (consumers tolerate new keys). Standing friction is OK-status
// by design: entries describe forge (the generator), not the project,
// so they must not trip CI gates. Malformed lines (torn writes) are the
// one warn condition — that is local data damage worth surfacing.
func auditFriction(projectDir string) AuditCategory {
	path := filepath.Join(projectDir, filepath.FromSlash(frictionFileRelPath))
	entries, malformed, err := loadFrictionEntries(path)
	if err != nil {
		return AuditCategory{
			Status:  AuditStatusWarn,
			Summary: fmt.Sprintf("friction log unreadable: %v", err),
		}
	}
	if len(entries) == 0 && malformed == 0 {
		return AuditCategory{
			Status:  AuditStatusOK,
			Summary: "no friction recorded",
			Details: map[string]any{
				"count": 0,
				"hint":  fmt.Sprintf("record generator friction the moment you hit it: `%s friction add <text> --severity p1 --area codegen`", Name()),
			},
		}
	}

	bySeverity := map[string]int{}
	var newest time.Time
	for _, e := range entries {
		bySeverity[strings.ToLower(e.Severity)]++
		if e.RecordedAt.After(newest) {
			newest = e.RecordedAt
		}
	}
	details := map[string]any{
		"count":       len(entries),
		"by_severity": bySeverity,
		"hint":        fmt.Sprintf("run `%s friction list` to view; `%s friction export` renders markdown", Name(), Name()),
	}
	if !newest.IsZero() {
		details["newest_recorded_at"] = newest.Format(time.RFC3339)
	}

	var sevBits []string
	for _, sev := range frictionSeverities {
		if n := bySeverity[sev]; n > 0 {
			sevBits = append(sevBits, fmt.Sprintf("%s: %d", sev, n))
		}
	}
	summary := fmt.Sprintf("%d friction entr%s recorded", len(entries), pluralIES(len(entries)))
	if len(sevBits) > 0 {
		summary += " (" + strings.Join(sevBits, ", ") + ")"
	}

	status := AuditStatusOK
	if malformed > 0 {
		status = AuditStatusWarn
		details["malformed_lines"] = malformed
		summary += fmt.Sprintf(" — %d malformed line(s) skipped", malformed)
	}
	return AuditCategory{Status: status, Summary: summary, Details: details}
}
