// Copyright (c) 2025 Reliant Labs
package components

import (
	"strings"
	"testing"
)

func TestLibraryEmbedding(t *testing.T) {
	lib := NewLibrary()
	for _, entry := range lib.Registry() {
		content, err := componentsFS.ReadFile(entry.FilePath)
		if err != nil {
			t.Errorf("component %q (path %q): embed read failed: %v", entry.Name, entry.FilePath, err)
			continue
		}
		if len(content) == 0 {
			t.Errorf("component %q: embedded content is empty", entry.Name)
		}
	}
}

func TestLibraryRegistry(t *testing.T) {
	lib := NewLibrary()
	reg := lib.Registry()

	if len(reg) == 0 {
		t.Fatal("registry is empty")
	}
	if len(lib.ByName()) == 0 {
		t.Fatal("byName is empty")
	}

	for _, entry := range reg {
		if entry.Name == "" {
			t.Error("component with empty name")
		}
		if entry.Category == "" {
			t.Errorf("component %q has empty category", entry.Name)
		}
		if entry.Description == "" {
			t.Errorf("component %q has empty description", entry.Name)
		}
		if len(entry.Tags) == 0 {
			t.Errorf("component %q has no tags", entry.Name)
		}
		if entry.FilePath == "" {
			t.Errorf("component %q has empty file path", entry.Name)
		}
	}

	// No duplicate names
	seen := make(map[string]bool)
	for _, entry := range reg {
		if seen[entry.Name] {
			t.Errorf("duplicate component name: %q", entry.Name)
		}
		seen[entry.Name] = true
	}
}

func TestLibraryCategories(t *testing.T) {
	lib := NewLibrary()
	counts := make(map[Category]int)
	for _, entry := range lib.Registry() {
		counts[entry.Category]++
	}

	expected := map[Category]int{
		CategoryLayouts:  11,
		CategoryCharts:   6,
		CategoryDiagrams: 5,
		CategoryDeck:     7,
		CategoryUI:       32,
	}

	for cat, want := range expected {
		got := counts[cat]
		if got != want {
			t.Errorf("category %q: expected %d components, got %d", cat, want, got)
		}
	}
}

func TestLibraryGet(t *testing.T) {
	lib := NewLibrary()

	content, err := lib.Get("quadrant_chart")
	if err != nil {
		t.Fatalf("get quadrant_chart: %v", err)
	}
	if !strings.Contains(content, "QuadrantChart") {
		t.Error("quadrant_chart content should contain 'QuadrantChart'")
	}

	_, err = lib.Get("nonexistent")
	if err == nil {
		t.Error("get nonexistent should return error")
	}
}

func TestLibraryGetEntry(t *testing.T) {
	lib := NewLibrary()

	entry, ok := lib.GetEntry("sidebar_left")
	if !ok {
		t.Fatal("sidebar_left should exist")
	}
	if entry.Category != CategoryLayouts {
		t.Errorf("sidebar_left category = %q, want layouts", entry.Category)
	}

	_, ok = lib.GetEntry("nonexistent")
	if ok {
		t.Error("nonexistent should not exist")
	}
}

func TestLibrarySearch(t *testing.T) {
	lib := NewLibrary()

	// Search by tag keyword
	results := lib.Search("deck")
	found := false
	for _, r := range results {
		if r.Name == "slide_title" {
			found = true
			break
		}
	}
	if !found {
		t.Error("search 'deck' should find slide_title")
	}

	// Search by category keyword
	results = lib.Search("charts")
	found = false
	for _, r := range results {
		if r.Name == "quadrant_chart" {
			found = true
			break
		}
	}
	if !found {
		t.Error("search 'charts' should find quadrant_chart")
	}

	// Search by name keyword
	results = lib.Search("funnel")
	found = false
	for _, r := range results {
		if r.Name == "funnel_chart" {
			found = true
			break
		}
	}
	if !found {
		t.Error("search 'funnel' should find funnel_chart")
	}

	// Multi-word search (bag-of-words AND)
	results = lib.Search("crud admin")
	if len(results) == 0 {
		t.Error("search 'crud admin' should find components")
	}
	for _, r := range results {
		nameLower := strings.ToLower(r.Name)
		descLower := strings.ToLower(r.Description)
		tagStr := strings.ToLower(strings.Join(r.Tags, " "))
		catStr := string(r.Category)
		combined := nameLower + " " + descLower + " " + tagStr + " " + catStr
		if !strings.Contains(combined, "crud") || !strings.Contains(combined, "admin") {
			t.Errorf("search 'crud admin' returned %q which doesn't match both words", r.Name)
		}
	}

	// Search with no results
	results = lib.Search("xyznonexistent123")
	if len(results) != 0 {
		t.Errorf("search with no results should return empty, got %d", len(results))
	}

	// Empty search returns all
	results = lib.Search("")
	if len(results) != len(lib.Registry()) {
		t.Errorf("empty search should return all, got %d want %d", len(results), len(lib.Registry()))
	}
}

func TestLibraryList(t *testing.T) {
	lib := NewLibrary()

	// List all
	all := lib.List("", "")
	if len(all) != 61 {
		t.Errorf("list all should return 61 components, got %d", len(all))
	}

	// List filtered by category
	deck := lib.List("", "deck")
	if len(deck) != 7 {
		t.Errorf("list category=deck should return 7, got %d", len(deck))
	}
}

func TestLibraryFindSimilar(t *testing.T) {
	lib := NewLibrary()

	suggestions := lib.FindSimilar("slide")
	if len(suggestions) == 0 {
		t.Error("FindSimilar('slide') should return suggestions")
	}
}

func TestFormatComponentList(t *testing.T) {
	entries := []Entry{
		{Name: "test_chart", Category: CategoryCharts, Description: "A test chart", Tags: []string{"chart"}},
	}
	result := FormatComponentList(entries)
	if !strings.Contains(result, "Found 1 components") {
		t.Errorf("format should show count, got: %s", result)
	}
	if !strings.Contains(result, "test_chart") {
		t.Error("format should include component name")
	}
}