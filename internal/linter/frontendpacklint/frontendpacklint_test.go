package frontendpacklint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// helper builds a fake pack directory with the given manifest body and
// optional template files. Returns the pack root.
func makeFakePack(t *testing.T, manifest string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write pack.yaml: %v", err)
	}
	tplDir := filepath.Join(dir, "templates")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatalf("mkdir templates: %v", err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(tplDir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func TestLintPackDir_FlagsThirdPartyUI(t *testing.T) {
	pack := makeFakePack(t, `
name: bad-table
kind: frontend
version: 0.1.0
`, map[string]string{
		"BadTable.tsx.tmpl": `import { useReactTable } from "@tanstack/react-table";
import * as Dialog from "@radix-ui/react-dialog";

export function BadTable() { return null; }
`,
	})

	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir: %v", err)
	}
	if len(res.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(res.Findings), res.Findings)
	}
	imports := []string{res.Findings[0].Import, res.Findings[1].Import}
	wantFound := map[string]bool{"@tanstack/react-table": false, "@radix-ui/react-dialog": false}
	for _, im := range imports {
		if _, ok := wantFound[im]; ok {
			wantFound[im] = true
		}
	}
	for k, v := range wantFound {
		if !v {
			t.Errorf("expected finding for import %q, didn't find one", k)
		}
	}
	for _, f := range res.Findings {
		if f.Severity != SeverityWarning {
			t.Errorf("expected severity warn, got %q", f.Severity)
		}
		if f.Rule != "frontendpack-third-party-ui" {
			t.Errorf("expected rule frontendpack-third-party-ui, got %q", f.Rule)
		}
	}
}

func TestLintPackDir_HonoursAllowlist(t *testing.T) {
	// @tanstack/react-table is in allowed_third_party — must not produce a
	// finding. @radix-ui/* is NOT — it should still fire.
	pack := makeFakePack(t, `
name: data-table
kind: frontend
version: 1.1.0
allowed_third_party:
  - "@tanstack/react-table"
`, map[string]string{
		"DataTable.tsx.tmpl": `import { useReactTable } from "@tanstack/react-table";
import * as Dialog from "@radix-ui/react-dialog";
`,
	})

	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding (radix only), got %d: %+v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Import != "@radix-ui/react-dialog" {
		t.Errorf("expected radix finding, got %q", res.Findings[0].Import)
	}
}

func TestLintPackDir_AllowlistScopePrefix(t *testing.T) {
	// Trailing-slash allowlist permits the entire scope.
	pack := makeFakePack(t, `
name: radix-pack
kind: frontend
version: 0.1.0
allowed_third_party:
  - "@radix-ui/"
`, map[string]string{
		"X.tsx.tmpl": `import * as A from "@radix-ui/react-dialog";
import * as B from "@radix-ui/react-select";
import * as C from "antd";
`,
	})

	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding (antd), got %d: %+v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Import != "antd" {
		t.Errorf("expected antd finding, got %q", res.Findings[0].Import)
	}
}

func TestLintPackDir_IgnoresGoPacks(t *testing.T) {
	// kind: go (or empty) — the rule is frontend-pack-only.
	pack := makeFakePack(t, `
name: jwt-auth
kind: go
version: 1.0.0
`, map[string]string{
		// Even with bogus tsx file in templates, nothing fires.
		"jwt.tsx.tmpl": `import { x } from "@radix-ui/react-dialog";`,
	})

	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected zero findings for go pack, got %d: %+v", len(res.Findings), res.Findings)
	}
}

func TestLintPackDir_AllowsBaseLibraryAndFrameworkImports(t *testing.T) {
	pack := makeFakePack(t, `
name: clean-pack
kind: frontend
version: 0.1.0
`, map[string]string{
		"Clean.tsx.tmpl": `import { useState } from "react";
import Link from "next/link";
import AlertBanner from "@/components/ui/alert_banner";
import SearchInput from "@/components/ui/search_input";
import { useGetThing } from "@/hooks";
import { create } from "@bufbuild/protobuf";
`,
	})

	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("expected zero findings for clean pack, got %d: %+v", len(res.Findings), res.Findings)
	}
}

func TestLintPackDir_TypeOnlyImports(t *testing.T) {
	// `import type ... from ...` should also be detected — types from a
	// disallowed lib still couple the pack to it.
	pack := makeFakePack(t, `
name: bad-types
kind: frontend
version: 0.1.0
`, map[string]string{
		"X.tsx.tmpl": `import type { ColumnDef } from "@tanstack/react-table";`,
	})

	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(res.Findings))
	}
	if res.Findings[0].Line != 1 {
		t.Errorf("expected line 1, got %d", res.Findings[0].Line)
	}
}

func TestLintPacksRoot_AggregatesAcrossPacks(t *testing.T) {
	root := t.TempDir()

	// Pack 1: bad
	bad := filepath.Join(root, "bad-pack")
	if err := os.MkdirAll(filepath.Join(bad, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(bad, "pack.yaml"), []byte("name: bad-pack\nkind: frontend\n"), 0o644)
	_ = os.WriteFile(filepath.Join(bad, "templates", "x.tsx.tmpl"),
		[]byte(`import { x } from "@radix-ui/react-dialog";`), 0o644)

	// Pack 2: clean
	clean := filepath.Join(root, "clean-pack")
	if err := os.MkdirAll(filepath.Join(clean, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(clean, "pack.yaml"), []byte("name: clean-pack\nkind: frontend\n"), 0o644)
	_ = os.WriteFile(filepath.Join(clean, "templates", "y.tsx.tmpl"),
		[]byte(`import X from "@/components/ui/badge";`), 0o644)

	// Pack 3: go pack — irrelevant
	gop := filepath.Join(root, "jwt-auth")
	_ = os.MkdirAll(filepath.Join(gop, "templates"), 0o755)
	_ = os.WriteFile(filepath.Join(gop, "pack.yaml"), []byte("name: jwt-auth\nkind: go\n"), 0o644)

	res, err := LintPacksRoot(root)
	if err != nil {
		t.Fatalf("LintPacksRoot: %v", err)
	}
	if len(res.Findings) != 1 {
		t.Fatalf("expected 1 aggregated finding, got %d: %+v", len(res.Findings), res.Findings)
	}
	if res.Findings[0].Pack != "bad-pack" {
		t.Errorf("expected pack=bad-pack, got %q", res.Findings[0].Pack)
	}
}

func TestResult_FormatText(t *testing.T) {
	r := Result{Findings: []Finding{
		{Pack: "p", File: "p/templates/x.tsx.tmpl", Line: 3,
			Import: "@radix-ui/react-dialog", Severity: SeverityWarning,
			Rule: "frontendpack-third-party-ui", Message: "imports radix"},
	}}
	out := r.FormatText()
	if !strings.Contains(out, "frontendpack-third-party-ui") {
		t.Errorf("expected rule name in output, got %q", out)
	}
	if !strings.Contains(out, "allowed_third_party") {
		t.Errorf("expected remediation hint mentioning allowed_third_party, got %q", out)
	}
}

func TestLintPackDir_ExistingDataTablePackIsClean(t *testing.T) {
	// The data-table pack ships in this repo and is the canonical example
	// of "wraps a headless engine" — it should pass with @tanstack/react-table
	// allowlisted. This test guards against regressions.
	pack := filepath.Join("..", "..", "packs", "data-table")
	if _, err := os.Stat(pack); os.IsNotExist(err) {
		t.Skip("data-table pack not at expected path; skipping")
	}
	res, err := LintPackDir(pack)
	if err != nil {
		t.Fatalf("LintPackDir(data-table): %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("data-table pack should be clean (allowlist covers TanStack); got %d findings:\n%s",
			len(res.Findings), res.FormatText())
	}
}
