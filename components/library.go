// Copyright (c) 2025 Reliant Labs
package components

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed components/*/*.tsx
var componentsFS embed.FS

// Category groups components by type.
type Category string

const (
	CategoryLayouts  Category = "layouts"
	CategoryCharts   Category = "charts"
	CategoryDiagrams Category = "diagrams"
	CategoryDeck     Category = "deck"
	CategoryUI       Category = "ui"
)

// Entry describes a single component in the library.
type Entry struct {
	Name        string   `json:"name"`
	Category    Category `json:"category"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	FilePath    string   `json:"-"` // internal embed path
}

// registry holds metadata for every component.
var registry = []Entry{
	// ── Layouts ──────────────────────────────────────────────────────────
	{Name: "hero_centered", Category: CategoryLayouts, Description: "Hero section with centered content, headline, CTA buttons, and gradient background", Tags: []string{"layout", "landing", "marketing", "hero"}},
	{Name: "sidebar_left", Category: CategoryLayouts, Description: "Fixed left sidebar with navigation and main content area", Tags: []string{"layout", "dashboard", "admin", "portal", "crm"}},
	{Name: "sidebar_right", Category: CategoryLayouts, Description: "Fixed right sidebar with main content and contextual panel", Tags: []string{"layout", "blog", "docs", "portal"}},
	{Name: "dashboard_grid", Category: CategoryLayouts, Description: "Responsive grid layout with metric cards and main content area", Tags: []string{"layout", "dashboard", "analytics", "admin", "crm"}},
	{Name: "card_grid", Category: CategoryLayouts, Description: "Responsive card grid with configurable columns and tags", Tags: []string{"layout", "gallery", "catalog", "marketing", "landing"}},
	{Name: "split_view", Category: CategoryLayouts, Description: "Two-pane layout with configurable ratio for comparison views", Tags: []string{"layout", "editor", "diff", "portal"}},
	{Name: "kanban_board", Category: CategoryLayouts, Description: "Multi-column board with cards for task management", Tags: []string{"layout", "kanban", "project", "crm", "portal"}},
	{Name: "form_wizard", Category: CategoryLayouts, Description: "Multi-step form with progress indicator", Tags: []string{"layout", "form", "onboarding", "wizard", "portal"}},
	{Name: "timeline", Category: CategoryLayouts, Description: "Vertical timeline with date markers and content blocks", Tags: []string{"layout", "timeline", "history", "marketing", "landing"}},
	{Name: "masonry", Category: CategoryLayouts, Description: "CSS columns masonry grid with variable-height items", Tags: []string{"layout", "gallery", "portfolio", "marketing"}},
	{Name: "sidebar_layout", Category: CategoryLayouts, Description: "Admin layout with collapsible sidebar, navigation sections, user profile area, and header bar.", Tags: []string{"layout", "admin", "dashboard", "navigation"}},

	// ── Charts ───────────────────────────────────────────────────────────
	{Name: "quadrant_chart", Category: CategoryCharts, Description: "2x2 quadrant/matrix chart with positioned items, axis labels, and highlighted item. Items use 0-1 normalized coordinates — all pixel math is internal.", Tags: []string{"chart", "competitive", "matrix", "deck", "marketing", "comparison"}},
	{Name: "concentric_circles", Category: CategoryCharts, Description: "Nested concentric circles for TAM/SAM/SOM or layered metrics. Rings auto-space and labels position in visible bands.", Tags: []string{"chart", "market", "tam", "deck", "marketing"}},
	{Name: "funnel_chart", Category: CategoryCharts, Description: "Vertical funnel visualization with tapering stages, conversion annotations, and alert highlighting for problem stages.", Tags: []string{"chart", "funnel", "sales", "conversion", "deck", "marketing", "crm"}},
	{Name: "bar_chart", Category: CategoryCharts, Description: "Horizontal or vertical bar chart with stacked segments, auto-color, and value labels.", Tags: []string{"chart", "bar", "data", "dashboard", "analytics", "deck"}},
	{Name: "donut_chart", Category: CategoryCharts, Description: "Ring/donut chart with segments, center label, and legend. Uses SVG stroke-dasharray.", Tags: []string{"chart", "donut", "pie", "data", "dashboard", "analytics"}},
	{Name: "radar_chart", Category: CategoryCharts, Description: "Spider/radar chart with multiple overlaid datasets, configurable axes, and grid rings.", Tags: []string{"chart", "radar", "spider", "comparison", "dashboard", "analytics"}},

	// ── Diagrams ─────────────────────────────────────────────────────────
	{Name: "flow_horizontal", Category: CategoryDiagrams, Description: "Horizontal flow/pipeline with connected steps, status indicators, and optional loop-back arrow.", Tags: []string{"diagram", "flow", "pipeline", "process", "deck", "marketing"}},
	{Name: "comparison_matrix", Category: CategoryDiagrams, Description: "Feature comparison table with products as columns, grouped features, check/cross indicators, and highlighted column.", Tags: []string{"diagram", "comparison", "features", "pricing", "marketing", "landing"}},
	{Name: "process_steps", Category: CategoryDiagrams, Description: "Numbered process steps with completed/active/pending states. Supports horizontal and vertical layouts.", Tags: []string{"diagram", "process", "steps", "onboarding", "marketing", "landing"}},
	{Name: "architecture_diagram", Category: CategoryDiagrams, Description: "System architecture diagram with grouped service boxes and SVG arrow connections.", Tags: []string{"diagram", "architecture", "system", "technical", "docs"}},
	{Name: "org_chart", Category: CategoryDiagrams, Description: "Organizational hierarchy chart with recursive tree layout, avatar circles, and CSS connector lines.", Tags: []string{"diagram", "org", "hierarchy", "team", "portal"}},

	// ── Deck (Pitch Deck Slides) ─────────────────────────────────────────
	{Name: "slide_title", Category: CategoryDeck, Description: "Title/opening slide (1280x720) with centered company name, tagline, and optional logo.", Tags: []string{"deck", "slide", "title", "presentation"}},
	{Name: "slide_stat_hero", Category: CategoryDeck, Description: "Big statistic hero slide (1280x720) with giant gradient number, headline, and supporting text.", Tags: []string{"deck", "slide", "stat", "hero", "presentation"}},
	{Name: "slide_two_column", Category: CategoryDeck, Description: "Two-column content slide (1280x720) with title bar and equal left/right content areas.", Tags: []string{"deck", "slide", "two-column", "presentation"}},
	{Name: "slide_card_grid", Category: CategoryDeck, Description: "Card grid slide (1280x720) with 2-4 cards, icon areas, badges, and highlight borders.", Tags: []string{"deck", "slide", "cards", "grid", "presentation"}},
	{Name: "slide_comparison", Category: CategoryDeck, Description: "Before/After comparison slide (1280x720) with red 'before' and green 'after' panels.", Tags: []string{"deck", "slide", "comparison", "before-after", "presentation"}},
	{Name: "slide_quote", Category: CategoryDeck, Description: "Quote/testimonial slide (1280x720) with decorative quote marks and attribution.", Tags: []string{"deck", "slide", "quote", "testimonial", "presentation"}},
	{Name: "slide_metrics_grid", Category: CategoryDeck, Description: "Metrics/KPI grid slide (1280x720) with 2x3 metric cards, trend indicators, and highlight.", Tags: []string{"deck", "slide", "metrics", "kpi", "presentation"}},

	// ── UI Components ────────────────────────────────────────────────────
	{Name: "pricing_table", Category: CategoryUI, Description: "3-tier pricing comparison with highlighted tier, feature checklists, badges, and CTA buttons.", Tags: []string{"ui", "pricing", "saas", "marketing", "landing"}},
	{Name: "stat_grid", Category: CategoryUI, Description: "Statistics grid with large numbers, labels, icons, and trend indicators (up/down/flat).", Tags: []string{"ui", "stats", "metrics", "dashboard", "analytics"}},
	{Name: "feature_comparison", Category: CategoryUI, Description: "Product feature comparison table with sticky header, grouped features, and highlighted column.", Tags: []string{"ui", "comparison", "features", "pricing", "marketing", "landing"}},
	{Name: "testimonial_cards", Category: CategoryUI, Description: "Customer testimonial cards with quotes, star ratings, avatars, and attribution.", Tags: []string{"ui", "testimonials", "social-proof", "marketing", "landing"}},
	{Name: "navigation_header", Category: CategoryUI, Description: "Responsive navigation header with brand, links, CTA, and mobile hamburger menu.", Tags: []string{"ui", "navigation", "header", "landing", "portal", "dashboard"}},
	{Name: "footer", Category: CategoryUI, Description: "Multi-column site footer with link groups, social icons, and copyright.", Tags: []string{"ui", "footer", "landing", "portal", "marketing"}},
	{Name: "hero_section", Category: CategoryUI, Description: "Marketing hero section with headline, CTAs, and optional media area.", Tags: []string{"ui", "hero", "marketing", "landing"}},
	{Name: "login_form", Category: CategoryUI, Description: "Authentication form with email/password, social login, and sign-up link.", Tags: []string{"ui", "auth", "login", "form", "portal"}},
	{Name: "data_table", Category: CategoryUI, Description: "Sortable, filterable data table with column headers, row selection, pagination, loading skeleton, and empty state.", Tags: []string{"ui", "crud", "admin", "table", "dashboard"}},
	{Name: "stat_cards", Category: CategoryUI, Description: "Row of stat cards showing key metrics with icon, label, value, and color-coded trend indicators.", Tags: []string{"ui", "dashboard", "analytics", "admin", "stats"}},
	{Name: "detail_view", Category: CategoryUI, Description: "Structured detail/show view for a single entity with field groups, multiple field types, and action buttons.", Tags: []string{"ui", "crud", "admin", "detail"}},
	{Name: "crud_form", Category: CategoryUI, Description: "Form component for create/edit operations with typed fields, validation errors, and submit/cancel buttons.", Tags: []string{"ui", "crud", "admin", "form"}},
	{Name: "command_bar", Category: CategoryUI, Description: "Command palette / search bar (⌘K style) with filterable results grouped by category and keyboard navigation.", Tags: []string{"ui", "admin", "search", "navigation"}},
	{Name: "confirmation_dialog", Category: CategoryUI, Description: "Confirmation dialog for destructive actions with danger/warning variants, backdrop overlay, and loading state.", Tags: []string{"ui", "crud", "admin", "modal", "dialog"}},
	{Name: "empty_state", Category: CategoryUI, Description: "Empty state placeholder with icon, title, description, and call-to-action button.", Tags: []string{"ui", "crud", "admin"}},
	{Name: "breadcrumb", Category: CategoryUI, Description: "Breadcrumb navigation component with separators and current page indicator.", Tags: []string{"ui", "navigation", "admin"}},
	{Name: "filter_bar", Category: CategoryUI, Description: "Filter/search bar for list pages with search input, filter dropdowns, active filter chips, and clear all.", Tags: []string{"ui", "crud", "admin", "search", "filter"}},
	{Name: "page_header", Category: CategoryUI, Description: "Page title with breadcrumb navigation and action buttons for top of every page.", Tags: []string{"ui", "crud", "admin", "header", "navigation", "breadcrumb"}},
	{Name: "toast_notification", Category: CategoryUI, Description: "Toast notification system with success, error, warning, and info variants. Auto-dismiss with configurable duration.", Tags: []string{"ui", "notification", "toast", "feedback", "admin"}},
	{Name: "dropdown_menu", Category: CategoryUI, Description: "Context menu / action dropdown with grouped items, icons, keyboard dismiss, and danger variant.", Tags: []string{"ui", "menu", "dropdown", "actions", "admin", "crud"}},
	{Name: "avatar", Category: CategoryUI, Description: "User avatar with image, initials fallback, and online/offline/busy status indicator.", Tags: []string{"ui", "avatar", "user", "profile", "admin"}},
	{Name: "badge", Category: CategoryUI, Description: "Status badge with success, warning, error, info, and neutral variants. Supports dot indicator and removable mode.", Tags: []string{"ui", "badge", "status", "tag", "admin", "crud"}},
	{Name: "tabs", Category: CategoryUI, Description: "Tab navigation with underline, pills, and boxed variants. Supports icons, badges, and render-prop children.", Tags: []string{"ui", "tabs", "navigation", "admin", "dashboard"}},
	{Name: "key_value_list", Category: CategoryUI, Description: "Labeled field display for detail views with grouped Key: Value pairs, clipboard copy, and multi-column layouts.", Tags: []string{"ui", "detail", "crud", "admin", "fields"}},
	{Name: "pagination", Category: CategoryUI, Description: "Standalone pagination controls with page numbers, prev/next, and item count display.", Tags: []string{"ui", "pagination", "table", "crud", "admin"}},
	{Name: "search_input", Category: CategoryUI, Description: "Search input with magnifying glass icon, clear button, keyboard shortcut support, and multiple sizes.", Tags: []string{"ui", "search", "input", "filter", "admin"}},
	{Name: "modal", Category: CategoryUI, Description: "Generic modal dialog with header, body, footer sections. Supports ESC close, overlay click, and multiple sizes.", Tags: []string{"ui", "modal", "dialog", "overlay", "admin", "crud"}},
	{Name: "skeleton_loader", Category: CategoryUI, Description: "Configurable skeleton loading states for text, cards, table rows, list items, form fields, and custom shapes.", Tags: []string{"ui", "skeleton", "loading", "placeholder", "admin"}},
	{Name: "alert_banner", Category: CategoryUI, Description: "Info, success, warning, and error banner for top of page. Dismissible with optional action button.", Tags: []string{"ui", "alert", "banner", "notification", "admin"}},
	{Name: "toggle_switch", Category: CategoryUI, Description: "Toggle/switch input with label, description, disabled state, and multiple sizes.", Tags: []string{"ui", "toggle", "switch", "input", "form", "admin"}},
	{Name: "activity_feed", Category: CategoryUI, Description: "Timeline of recent activity/events with user avatars, relative timestamps, and connecting lines.", Tags: []string{"ui", "activity", "feed", "timeline", "admin", "dashboard"}},
	{Name: "metric_card", Category: CategoryUI, Description: "Single metric display with trend indicator, sparkline chart, and optional link.", Tags: []string{"ui", "metric", "stat", "dashboard", "analytics", "admin"}},
}

// byName provides O(1) lookup.
var byName map[string]*Entry

func init() {
	byName = make(map[string]*Entry, len(registry))
	for i := range registry {
		c := &registry[i]
		c.FilePath = fmt.Sprintf("components/%s/%s.tsx", c.Category, c.Name)
		byName[c.Name] = c
	}
}

// Library provides access to the component library.
type Library struct{}

// NewLibrary creates a new component library instance.
func NewLibrary() *Library {
	return &Library{}
}

// Registry returns all component entries.
func (l *Library) Registry() []Entry {
	return registry
}

// ByName returns the name-to-entry lookup map.
func (l *Library) ByName() map[string]*Entry {
	return byName
}

// Search finds components using unified keyword search. The query string is
// split into words and each word is matched against the component's name,
// tags, category, and description. A component matches if ALL words match
// (each word can match in any field — bag-of-words AND semantics).
// An empty query returns all components.
func (l *Library) Search(query string) []Entry {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return registry
	}

	words := strings.Fields(query)
	var results []Entry
	for _, c := range registry {
		if matchesAllWords(c, words) {
			results = append(results, c)
		}
	}
	return results
}

// Get retrieves the source code for a component by name.
func (l *Library) Get(name string) (string, error) {
	entry, exists := byName[name]
	if !exists {
		return "", fmt.Errorf("component '%s' not found", name)
	}

	content, err := componentsFS.ReadFile(entry.FilePath)
	if err != nil {
		return "", fmt.Errorf("failed to read component: %w", err)
	}

	return string(content), nil
}

// GetEntry retrieves the metadata entry for a component by name.
func (l *Library) GetEntry(name string) (*Entry, bool) {
	entry, exists := byName[name]
	return entry, exists
}

// List returns all components, optionally filtered by tag and/or category.
func (l *Library) List(tag, category string) []Entry {
	tag = strings.ToLower(strings.TrimSpace(tag))
	category = strings.ToLower(strings.TrimSpace(category))

	if tag == "" && category == "" {
		return registry
	}

	var filtered []Entry
	for _, c := range registry {
		if category != "" && string(c.Category) != category {
			continue
		}
		if tag != "" && !hasTag(c.Tags, tag) {
			continue
		}
		filtered = append(filtered, c)
	}
	return filtered
}

// FindSimilar returns up to 5 component names similar to the given name.
func (l *Library) FindSimilar(name string) []string {
	name = strings.ToLower(name)
	var matches []string
	for _, c := range registry {
		cName := strings.ToLower(c.Name)
		if strings.Contains(cName, name) || strings.Contains(name, cName) {
			matches = append(matches, c.Name)
		}
		prefix := commonPrefix(name, cName)
		if len(prefix) >= 3 {
			matches = append(matches, c.Name)
		}
	}
	seen := make(map[string]bool)
	var unique []string
	for _, m := range matches {
		if !seen[m] {
			seen[m] = true
			unique = append(unique, m)
		}
	}
	if len(unique) > 5 {
		unique = unique[:5]
	}
	return unique
}

// FormatComponentList formats a list of entries into a human-readable string
// grouped by category.
func FormatComponentList(entries []Entry) string {
	grouped := make(map[Category][]Entry)
	for _, e := range entries {
		grouped[e.Category] = append(grouped[e.Category], e)
	}

	order := []Category{CategoryLayouts, CategoryCharts, CategoryDiagrams, CategoryDeck, CategoryUI}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d components:\n", len(entries)))

	for _, cat := range order {
		items, ok := grouped[cat]
		if !ok {
			continue
		}
		sb.WriteString(fmt.Sprintf("\n## %s (%d)\n", strings.ToUpper(string(cat)), len(items)))
		for _, item := range items {
			sb.WriteString(fmt.Sprintf("  • %s — %s\n", item.Name, item.Description))
			sb.WriteString(fmt.Sprintf("    Tags: %s\n", strings.Join(item.Tags, ", ")))
		}
	}

	sb.WriteString("\nUse action='get' with name='<component_name>' to retrieve the full source code.")
	return sb.String()
}

// ── Helpers ──────────────────────────────────────────────────────────────

func hasTag(tags []string, target string) bool {
	for _, t := range tags {
		if strings.EqualFold(t, target) {
			return true
		}
	}
	return false
}

func matchesAllWords(c Entry, words []string) bool {
	nameLower := strings.ToLower(c.Name)
	descLower := strings.ToLower(c.Description)
	catLower := string(c.Category)

	for _, word := range words {
		if !matchesWord(c, word, nameLower, descLower, catLower) {
			return false
		}
	}
	return true
}

func matchesWord(c Entry, word, nameLower, descLower, catLower string) bool {
	if strings.Contains(nameLower, word) {
		return true
	}
	if strings.Contains(descLower, word) {
		return true
	}
	if strings.Contains(catLower, word) {
		return true
	}
	for _, tag := range c.Tags {
		if strings.Contains(strings.ToLower(tag), word) {
			return true
		}
	}
	return false
}

func commonPrefix(a, b string) string {
	maxLen := len(a)
	if len(b) < maxLen {
		maxLen = len(b)
	}
	i := 0
	for i < maxLen && a[i] == b[i] {
		i++
	}
	return a[:i]
}