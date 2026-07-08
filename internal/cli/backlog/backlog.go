// Package backlog holds the `forge backlog` command group — list / add /
// close / open / migrate over the structured FORGE_BACKLOG.md.
//
// Dir-nested command group (the devspace idiom): the parent newCmd assembles
// the subcommands defined in the sibling files (list.go, add.go, close.go,
// open.go, migrate.go); the shared parse/rewrite core stays in this file.
// init() self-registers the group with internal/cli/factory so a blank import
// from internal/cli/groups.go attaches it to the root without a cycle.
package backlog

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
)

func init() { factory.Register(newCmd) }

// fileExists reports whether path names an existing non-directory file. A
// trivial os.Stat wrapper kept local to this package.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

// backlogFileName is the canonical backlog file at the forge repo root.
const backlogFileName = "FORGE_BACKLOG.md"

// Item is one entry in FORGE_BACKLOG.md.
//
// Items may have a yaml frontmatter block right under the section header;
// items without one are treated as legacy "untracked" entries (status: open,
// severity/area unknown). The Title is everything after the leading "## ".
type Item struct {
	ID       string `yaml:"id" json:"id,omitempty"`
	Severity string `yaml:"severity" json:"severity,omitempty"`
	Area     string `yaml:"area" json:"area,omitempty"`
	Status   string `yaml:"status" json:"status,omitempty"`
	FixedAt  string `yaml:"fixed_at" json:"fixed_at,omitempty"`

	Title string `json:"title"`
	Body  string `json:"body,omitempty"`

	// startLine / endLine are 1-indexed inclusive bounds of the item's
	// section in the backlog file. Used by `close`/`open` to rewrite the
	// frontmatter in place. Both are 0 for a missing item.
	startLine int
	endLine   int

	// hasFrontmatter records whether the original section already carried
	// a ```yaml ... ``` block. Determines whether close/open should patch
	// or insert.
	hasFrontmatter bool

	// frontmatterStart / frontmatterEnd are 1-indexed line numbers of the
	// "```yaml" and the closing "```" lines respectively when present.
	frontmatterStart int
	frontmatterEnd   int
}

// h2Re matches a backlog item header. We accept any "## " heading as an item;
// the file's existing top-level headings are "# Forge backlog ..." (h1) and
// "## Open" / "## Fixed in-session" (h2). The section-level h2s are filtered
// out by treating them as "category" markers (no body content of their own
// that resembles an item).
var (
	h2Re        = regexp.MustCompile(`^##\s+(.+?)\s*$`)
	yamlOpenRe  = regexp.MustCompile("^```ya?ml\\s*$")
	yamlCloseRe = regexp.MustCompile("^```\\s*$")
)

// categoryHeadings are the legacy section dividers in FORGE_BACKLOG.md.
// We don't treat them as items.
var categoryHeadings = map[string]bool{
	"open":             true,
	"fixed in-session": true,
	"fixed":            true,
}

// newCmd is the top-level `forge backlog` group.
func newCmd(f *factory.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backlog",
		Short: "Manage the structured FORGE_BACKLOG.md (list / add / close / open)",
	}
	cmd.AddCommand(newListCmd(f))
	cmd.AddCommand(newAddCmd(f))
	cmd.AddCommand(newCloseCmd(f))
	cmd.AddCommand(newOpenCmd(f))
	cmd.AddCommand(newMigrateCmd(f))
	return cmd
}

// backlogFilePath returns the path to FORGE_BACKLOG.md. We look for it in:
//  1. the cwd
//  2. the project root (forge.yaml walk-up)
//  3. forge's own repo (best-effort: walk up looking for the file itself)
//
// Most callers will be in the forge repo or a forge project; either case
// resolves cleanly.
func backlogFilePath() (string, error) {
	// 1. cwd
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if p := filepath.Join(cwd, backlogFileName); fileExists(p) {
		return p, nil
	}
	// 2. project root
	if root, err := cmdutil.FindProjectRoot(); err == nil && root != "" {
		if p := filepath.Join(root, backlogFileName); fileExists(p) {
			return p, nil
		}
	}
	// 3. walk up looking for the file directly (forge dev workflow).
	dir := cwd
	for {
		p := filepath.Join(dir, backlogFileName)
		if fileExists(p) {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// fallback: cwd, even if missing — the caller decides whether to create it.
	return filepath.Join(cwd, backlogFileName), nil
}

// loadBacklog parses FORGE_BACKLOG.md into structured items. Sections without
// frontmatter become "open" with unknown severity/area — never an error.
func loadBacklog() ([]Item, error) {
	file, err := backlogFilePath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	return parseBacklog(string(data)), nil
}

// parseBacklog walks the markdown line-by-line, slicing it on "## " headings,
// then attempts to extract a yaml frontmatter block at the top of each
// section. Robust to absent frontmatter — that's the legacy shape.
func parseBacklog(content string) []Item {
	lines := strings.Split(content, "\n")
	var items []Item

	// Find all h2 starts.
	var sectionStarts []int
	for i, line := range lines {
		if h2Re.MatchString(line) {
			heading := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "##"))
			if categoryHeadings[strings.ToLower(heading)] {
				continue
			}
			sectionStarts = append(sectionStarts, i)
		}
	}

	for idx, start := range sectionStarts {
		end := len(lines) - 1
		if idx+1 < len(sectionStarts) {
			end = sectionStarts[idx+1] - 1
		}
		// Trim trailing blank lines off the end so close/open insertions
		// don't accumulate whitespace.
		for end > start && strings.TrimSpace(lines[end]) == "" {
			end--
		}

		header := lines[start]
		title := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "##"))

		item := Item{
			Title:     title,
			startLine: start + 1, // convert to 1-indexed
			endLine:   end + 1,
			Status:    "open", // default for legacy items
		}

		// Look for a ```yaml block within the first ~20 lines of the section.
		fmStart, fmEnd := findFrontmatter(lines, start+1, end)
		if fmStart > 0 {
			yamlBlock := strings.Join(lines[fmStart+1:fmEnd], "\n")
			parseYAMLFrontmatter(yamlBlock, &item)
			item.hasFrontmatter = true
			item.frontmatterStart = fmStart + 1
			item.frontmatterEnd = fmEnd + 1
		}

		// Body is everything after the header (and after the frontmatter
		// block if present). Used for full-text rendering / json round-trip.
		bodyStart := start + 1
		if item.hasFrontmatter {
			bodyStart = fmEnd + 1
		}
		if bodyStart <= end {
			item.Body = strings.TrimSpace(strings.Join(lines[bodyStart:end+1], "\n"))
		}

		if item.Status == "" {
			item.Status = "open"
		}
		items = append(items, item)
	}
	return items
}

// findFrontmatter searches the slice [from..to] (0-indexed inclusive) for a
// ```yaml ... ``` block at the top of the section. Returns -1, -1 if absent.
func findFrontmatter(lines []string, from, to int) (int, int) {
	// Skip blank lines after the header.
	i := from
	for i <= to && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	if i > to {
		return -1, -1
	}
	if !yamlOpenRe.MatchString(strings.TrimSpace(lines[i])) {
		return -1, -1
	}
	open := i
	for j := i + 1; j <= to; j++ {
		if yamlCloseRe.MatchString(strings.TrimSpace(lines[j])) {
			return open, j
		}
	}
	return -1, -1
}

// parseYAMLFrontmatter unmarshals the yaml block into the item. Any fields
// the user set on top of the canonical ones are preserved by the loader, but
// only the canonical fields are written back by `close` / `open`.
func parseYAMLFrontmatter(yamlBody string, item *Item) {
	type frontmatter struct {
		ID       string `yaml:"id"`
		Severity string `yaml:"severity"`
		Area     string `yaml:"area"`
		Status   string `yaml:"status"`
		FixedAt  string `yaml:"fixed_at"`
	}
	var fm frontmatter
	if err := yaml.Unmarshal([]byte(yamlBody), &fm); err != nil {
		return
	}
	item.ID = fm.ID
	item.Severity = fm.Severity
	item.Area = fm.Area
	if fm.Status != "" {
		item.Status = fm.Status
	}
	item.FixedAt = fm.FixedAt
}

// filterBacklog applies area / status filters. Empty filter means "any".
func filterBacklog(items []Item, area, status string) []Item {
	var out []Item
	for _, it := range items {
		if area != "" && !strings.EqualFold(it.Area, area) {
			continue
		}
		if status != "" && !strings.EqualFold(it.Status, status) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// writeBacklogTable emits a tab-separated, human-friendly view.
func writeBacklogTable(w io.Writer, items []Item) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATUS\tSEVERITY\tAREA\tTITLE")
	for _, it := range items {
		id := it.ID
		if id == "" {
			id = "-"
		}
		sev := it.Severity
		if sev == "" {
			sev = "-"
		}
		area := it.Area
		if area == "" {
			area = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id, it.Status, sev, area, it.Title)
	}
	return tw.Flush()
}

func writeBacklogJSON(w io.Writer, items []Item) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
}

// nextBacklogID returns the next unused "forge-N" id by scanning existing
// items for the maximum numeric suffix.
func nextBacklogID(items []Item) string {
	highest := 0
	idRe := regexp.MustCompile(`^forge-(\d+)$`)
	for _, it := range items {
		m := idRe.FindStringSubmatch(it.ID)
		if len(m) != 2 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n > highest {
			highest = n
		}
	}
	return fmt.Sprintf("forge-%d", highest+1)
}

// capitalizeFirst capitalizes the first byte of a short ASCII label (auth,
// codegen, frontend) without pulling in golang.org/x/text/cases.
func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// renderNewItem produces the markdown section for a new backlog entry.
func renderNewItem(id, severity, area, today, title string) string {
	header := fmt.Sprintf("## [%s] %s", capitalizeFirst(area), title)
	yamlBlock := fmt.Sprintf(
		"```yaml\nid: %s\nseverity: %s\narea: %s\nstatus: open\ncreated_at: %s\n```",
		id, severity, area, today,
	)
	body := "TODO: describe the issue, reproduction, and remediation."
	return fmt.Sprintf("\n\n%s\n\n%s\n\n%s\n", header, yamlBlock, body)
}

// appendToFile appends content to the named file, creating it if missing.
func appendToFile(path, content string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

// setBacklogStatus rewrites the named item's frontmatter to status=newStatus
// and fixed_at=fixedAt (empty string clears the field). For items missing a
// frontmatter block, one is inserted right after the section header.
func setBacklogStatus(id, newStatus, fixedAt string, out io.Writer) error {
	file, err := backlogFilePath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}
	items := parseBacklog(string(data))
	var target *Item
	for i := range items {
		if items[i].ID == id {
			target = &items[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("backlog item %q not found", id)
	}

	target.Status = newStatus
	target.FixedAt = fixedAt

	updated, err := rewriteItem(string(data), *target)
	if err != nil {
		return err
	}
	if err := os.WriteFile(file, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", file, err)
	}
	_, _ = fmt.Fprintf(out, "%s -> status=%s\n", id, newStatus)
	return nil
}

// rewriteItem replaces the yaml frontmatter block in `content` for the given
// item, returning the modified file content. If the item has no frontmatter,
// one is inserted immediately after the section header.
func rewriteItem(content string, item Item) (string, error) {
	lines := strings.Split(content, "\n")
	yamlBlock := renderFrontmatterFor(item)

	if item.hasFrontmatter {
		// Replace lines [frontmatterStart..frontmatterEnd] (1-indexed) with
		// the new block.
		head := lines[:item.frontmatterStart-1]
		tail := lines[item.frontmatterEnd:]
		var newLines []string
		newLines = append(newLines, head...)
		newLines = append(newLines, strings.Split(yamlBlock, "\n")...)
		newLines = append(newLines, tail...)
		return strings.Join(newLines, "\n"), nil
	}

	// Insert frontmatter immediately after the header (with one blank line
	// before and after for readability).
	insertAt := item.startLine // 1-indexed, this is the "## ..." header line
	head := lines[:insertAt]
	tail := lines[insertAt:]
	insertion := []string{"", yamlBlock, ""}
	var newLines []string
	newLines = append(newLines, head...)
	newLines = append(newLines, insertion...)
	newLines = append(newLines, tail...)
	return strings.Join(newLines, "\n"), nil
}

// renderFrontmatterFor produces the canonical ```yaml block for the item.
// Empty fields are omitted so we don't write `area:` or `severity:` for
// migrated legacy items where the value is unknown.
func renderFrontmatterFor(item Item) string {
	var b strings.Builder
	b.WriteString("```yaml\n")
	if item.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", item.ID)
	}
	if item.Severity != "" {
		fmt.Fprintf(&b, "severity: %s\n", item.Severity)
	}
	if item.Area != "" {
		fmt.Fprintf(&b, "area: %s\n", item.Area)
	}
	if item.Status != "" {
		fmt.Fprintf(&b, "status: %s\n", item.Status)
	}
	if item.FixedAt != "" {
		fmt.Fprintf(&b, "fixed_at: %s\n", item.FixedAt)
	}
	b.WriteString("```")
	return b.String()
}

// migrateBacklog walks every section without frontmatter and inserts a
// best-effort block. Severity/area are left blank when not extractable;
// status defaults to "open" but is set to "fixed" when the section sits
// under a "## Fixed in-session" / "## Fixed" heading.
//
// The function is idempotent: items that already have frontmatter are
// skipped. Legacy items get fresh forge-N ids assigned in document order.
func migrateBacklog() (int, error) {
	file, err := backlogFilePath()
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", file, err)
	}
	content := string(data)

	// Detect each item's status by tracking the most-recent category
	// heading above it.
	sections := splitByCategory(content)
	items := parseBacklog(content)

	// Items without frontmatter, in document order.
	var legacy []*Item
	for i := range items {
		if !items[i].hasFrontmatter {
			legacy = append(legacy, &items[i])
		}
	}
	// Assign ids starting after the largest existing.
	nextN := largestForgeN(items) + 1
	for _, it := range legacy {
		it.ID = fmt.Sprintf("forge-%d", nextN)
		nextN++
		it.Status = inferStatus(sections, it.startLine)
	}

	// Rewrite document bottom-up so line numbers stay valid for earlier items.
	sort.Slice(legacy, func(i, j int) bool {
		return legacy[i].startLine > legacy[j].startLine
	})
	for _, it := range legacy {
		updated, err := rewriteItem(content, *it)
		if err != nil {
			return 0, err
		}
		content = updated
	}

	if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
		return 0, fmt.Errorf("write %s: %w", file, err)
	}
	return len(legacy), nil
}

// categorySection records a slice of the file [startLine..endLine] (1-indexed)
// labeled with its category name (lower-cased).
type categorySection struct {
	name      string
	startLine int
	endLine   int
}

func splitByCategory(content string) []categorySection {
	lines := strings.Split(content, "\n")
	var sections []categorySection
	for i, line := range lines {
		if !h2Re.MatchString(line) {
			continue
		}
		heading := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "##"))
		if !categoryHeadings[strings.ToLower(heading)] {
			continue
		}
		// Close the previous section if any.
		if n := len(sections); n > 0 {
			sections[n-1].endLine = i // exclusive end at this header line
		}
		sections = append(sections, categorySection{
			name:      strings.ToLower(heading),
			startLine: i + 1,
			endLine:   len(lines),
		})
	}
	return sections
}

func inferStatus(sections []categorySection, line int) string {
	for _, s := range sections {
		if line >= s.startLine && line < s.endLine {
			if strings.HasPrefix(s.name, "fixed") {
				return "fixed"
			}
			return "open"
		}
	}
	return "open"
}

func largestForgeN(items []Item) int {
	highest := 0
	idRe := regexp.MustCompile(`^forge-(\d+)$`)
	for _, it := range items {
		m := idRe.FindStringSubmatch(it.ID)
		if len(m) != 2 {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		if n > highest {
			highest = n
		}
	}
	return highest
}
