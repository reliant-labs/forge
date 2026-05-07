package templates

import (
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestSkillsDoNotReferenceRemovedGenFiles asserts that the live skill catalog
// (everything under project/skills/forge/) never names a `*_gen.go` filename
// that forge no longer emits. This is the meta-test that guards against the
// `middleware_gen.go` / `tracing_gen.go` / `metrics_gen.go` class of drift the
// 2026-05-06 dogfood review caught.
//
// Migration skills (those under migration/v0.x-to-*) are allow-listed because
// their entire job is to describe the OLD shape so users can detect and remove
// it. Touching them here would defeat their purpose.
//
// Allowed `*_gen.go` filenames are intentionally listed — when forge starts
// emitting a new `<X>_gen.go`, add it here. When forge stops emitting one,
// remove it. The list is the contract.
func TestSkillsDoNotReferenceRemovedGenFiles(t *testing.T) {
	t.Parallel()

	allowedGenFiles := map[string]struct{}{
		// Per-package mock generation off contract.go.
		"mock_gen.go": {},
		// Per-handler-directory codegen off proto descriptors.
		"handlers_gen.go":              {},
		"handlers_crud_gen.go":         {},
		"handlers_crud_test_gen.go":    {},
		"handlers_scaffold_test.go":    {}, // post-1.x scaffolded test file
		"authorizer_gen.go":            {},
		"tenant_gen.go":                {},
		"webhook_routes_gen.go":        {},
		// Pack outputs (under pkg/middleware/<pack>/...).
		"auth_gen.go":     {}, // jwt-auth pack output (see pack-development SKILL.md)
		"audit_gen.go":    {}, // audit-log pack output
		"frontend_gen.go": {}, // pack-development SKILL.md references the frontend codegen helper
		// ORM / entity wrapper generation.
		"<entity>_orm_gen.go": {}, // placeholder pattern referenced in skills
		// Generic suffix references like `*_gen.go` are valid family-level
		// references (not specific filenames) — handled below by the regex.
	}

	// Filenames forge no longer emits but which may appear in skills strictly
	// in historical / removed context. Mentions outside such context still
	// fail. Detected by paragraph-scope keyword.
	historicalOnly := map[string]struct{}{
		"middleware_gen.go": {},
		"tracing_gen.go":    {},
		"metrics_gen.go":    {},
	}
	historicalKeywords := []string{
		"removed",
		"pre-1.",
		"no longer",
		"have been removed",
	}

	// Migration skills are off-limits to this test — they describe historical
	// shapes by design.
	migrationAllowlist := []string{
		// "forge/migration/v0.x-to-y" template — match any version pair.
		// Matches v0.1-to-v0.2, v0.2-to-v0.3, etc. The original literal
		// "v0.x-to-" never matched anything; the per-pair migration
		// skills (v0.1-to-v0.2 etc.) describe historical Tier-1 file
		// shapes (app_gen.go, wire_gen.go) by design.
		"forge/migration/v0.",
		"forge/migration/upgrade/",
		"forge/migration/service/",
		"forge/migration/cli/",
	}
	isMigration := func(p string) bool {
		for _, prefix := range migrationAllowlist {
			if strings.Contains(p, prefix) {
				return true
			}
		}
		return false
	}

	// Match `<word>_gen.go` filenames inside skill bodies. We match on the
	// underscore-prefixed `_gen.go` suffix to skip over directory references
	// like `gen/` and Go imports.
	re := regexp.MustCompile(`([A-Za-z0-9_<>]+)_gen\.go`)

	skillsRoot := path.Join("project", "skills", "forge")
	violations := map[string][]string{}

	walkErr := fs.WalkDir(templateFS, skillsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(p, "SKILL.md") {
			return nil
		}
		if isMigration(p) {
			return nil
		}

		body, err := fs.ReadFile(templateFS, p)
		if err != nil {
			return err
		}

		paragraphs := strings.Split(string(body), "\n\n")
		seen := map[string]struct{}{}
		for _, para := range paragraphs {
			lower := strings.ToLower(para)
			isHistorical := false
			for _, kw := range historicalKeywords {
				if strings.Contains(lower, kw) {
					isHistorical = true
					break
				}
			}
			for _, m := range re.FindAllStringSubmatch(para, -1) {
				fname := m[1] + "_gen.go"
				if _, ok := allowedGenFiles[fname]; ok {
					continue
				}
				// Tolerate family-suffix wildcards (`*_gen.go`, `<x>_gen.go`).
				if strings.HasPrefix(fname, "*_") || strings.HasPrefix(m[1], "*") {
					continue
				}
				if _, ok := historicalOnly[fname]; ok && isHistorical {
					continue
				}
				seen[fname] = struct{}{}
			}
		}
		if len(seen) > 0 {
			names := make([]string, 0, len(seen))
			for n := range seen {
				names = append(names, n)
			}
			sort.Strings(names)
			violations[p] = names
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk skills tree: %v", walkErr)
	}

	if len(violations) == 0 {
		return
	}

	files := make([]string, 0, len(violations))
	for f := range violations {
		files = append(files, f)
	}
	sort.Strings(files)
	t.Errorf("skill catalog references *_gen.go filenames forge no longer emits.\n" +
		"Either add the filename to allowedGenFiles in this test (if forge emits it now) " +
		"or update the skill to drop the stale reference.")
	for _, f := range files {
		t.Errorf("  %s: %s", f, strings.Join(violations[f], ", "))
	}
}
