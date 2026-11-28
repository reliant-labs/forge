package templates

import (
	"io/fs"
	"path"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestTemplateEngine_RenderTemplate(t *testing.T) {
	engine, err := NewTemplateEngine()
	if err != nil {
		t.Fatalf("NewTemplateEngine: %v", err)
	}
	if len(engine.templates) == 0 {
		t.Fatal("the engine pre-loaded no templates")
	}
	if _, err := engine.RenderTemplate("service/does-not-exist.tmpl", nil); err == nil || !strings.Contains(err.Error(), "template service/does-not-exist.tmpl not found") {
		t.Errorf("an unknown template = %v", err)
	}

	for name := range engine.templates {
		if !strings.HasSuffix(name, ".tmpl") {
			t.Errorf("loaded %q, which is not a template", name)
		}
	}

	// A template that references a field its data lacks fails at execution,
	// and the error names the template.
	var loaded string
	for name := range engine.templates {
		loaded = name
		break
	}
	if _, err := engine.RenderTemplate(loaded, struct{ Unrelated int }{}); err == nil || !strings.Contains(err.Error(), "failed to execute template "+loaded) {
		t.Errorf("rendering %s with unrelated data = %v, want an execution error naming it", loaded, err)
	}
}

func TestRenderFromFS(t *testing.T) {
	fsys := fstest.MapFS{
		"base/plain.txt":        {Data: []byte("static //go:build ignore\n")},
		"base/hello.txt.tmpl":   {Data: []byte("hello {{.Name | upper}}")},
		"base/broken.txt.tmpl":  {Data: []byte("{{.Name")},
		"base/missing.txt.tmpl": {Data: []byte("{{.Nope.Deeper}}")},
		"base/sub/nested.tmpl":  {Data: []byte("{{.Name}}")},
	}
	data := struct{ Name string }{"world"}

	t.Run("a template is executed with the shared func map", func(t *testing.T) {
		got, err := RenderFromFS(fsys, "base", "hello.txt.tmpl", data)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "hello WORLD" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("a non-template file is returned as it is", func(t *testing.T) {
		got, err := RenderFromFS(fsys, "base", "plain.txt", nil)
		if err != nil || !strings.HasPrefix(string(got), "static") {
			t.Errorf("got (%q, %v)", got, err)
		}
	})

	t.Run("a name under a subdirectory", func(t *testing.T) {
		got, err := RenderFromFS(fsys, "base", "sub/nested.tmpl", data)
		if err != nil || string(got) != "world" {
			t.Errorf("got (%q, %v)", got, err)
		}
	})

	for name, want := range map[string]string{
		"absent.tmpl":      "read template absent.tmpl",
		"broken.txt.tmpl":  "parse template broken.txt.tmpl",
		"missing.txt.tmpl": "execute template missing.txt.tmpl",
	} {
		t.Run("error: "+name, func(t *testing.T) {
			_, err := RenderFromFS(fsys, "base", name, data)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		})
	}
}

func TestTemplateCategory_ListAndSource(t *testing.T) {
	flat, err := InternalPkgTemplates().ListFlat("")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := InternalPkgTemplates().List("")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(flat, "service.go.tmpl") {
		t.Errorf("ListFlat lacks the base templates: %v", flat)
	}
	for _, f := range flat {
		if strings.Contains(f, "/") {
			t.Errorf("ListFlat returned %q: a direct child has no slash", f)
		}
	}
	if !slices.Contains(rec, "client/client.go.tmpl") || len(rec) <= len(flat) {
		t.Errorf("List must descend into kinds: flat=%d recursive=%d", len(flat), len(rec))
	}

	if _, err := InternalPkgTemplates().ListFlat("no-such-dir"); err == nil || !strings.Contains(err.Error(), "read template dir") {
		t.Errorf("listing a missing directory = %v", err)
	}

	if got := ProjectTemplates().SourcePath("Taskfile.yml.tmpl"); got != "project/Taskfile.yml.tmpl" {
		t.Errorf("SourcePath = %q", got)
	}
	raw, err := Source(ProjectTemplates().SourcePath("Taskfile.yml.tmpl"))
	if err != nil || !strings.Contains(string(raw), "{{") {
		t.Errorf("Source must return the raw, unrendered template: (%d bytes, %v)", len(raw), err)
	}
	if _, err := Source("project/no-such-file.tmpl"); err == nil {
		t.Error("Source of an absent file must fail")
	}
}

func TestInternalPackageKindHelpers(t *testing.T) {
	names, err := ListInternalPackageKindTemplates("client")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(names, "client.go.tmpl") {
		t.Errorf("client kind templates = %v", names)
	}
	if _, err := ListInternalPackageKindTemplates("no-such-kind"); err == nil {
		t.Error("an unknown kind must fail to list")
	}
	if _, err := RenderInternalPackageKindTemplate("no-such-kind", "x.tmpl", nil); err == nil {
		t.Error("an unknown kind must fail to render")
	}
	if _, err := RenderInternalPackageTemplate("no-such.tmpl", nil); err == nil {
		t.Error("an unknown base template must fail to render")
	}
	if got := InternalPkgKindTemplates("client").SourcePath("client.go.tmpl"); got != "internal-package/client/client.go.tmpl" {
		t.Errorf("kind SourcePath = %q", got)
	}
}

func TestService_DispatchesToTheNamedCategory(t *testing.T) {
	svc := New(Deps{})

	for category, want := range map[string]string{
		"project":          "project",
		"frontend":         "frontend",
		"deploy":           "deploy",
		"test":             "test",
		"internal-package": "internal-package",
		"service":          "service",
		"webhook":          "webhook",
		"worker":           "worker",
		"worker-cron":      "worker-cron",
		"operator":         "operator",
		"ci":               "ci", // unknown to the switch: rooted at the name itself
	} {
		got := categoryFor(category).basePath
		if got != want {
			t.Errorf("categoryFor(%q).basePath = %q, want %q", category, got, want)
		}
	}

	names, err := svc.ListCategory("internal-package", "client")
	if err != nil || !slices.Contains(names, "client.go.tmpl") {
		t.Errorf("ListCategory = (%v, %v)", names, err)
	}
	raw, err := svc.RenderCategory("project", "agent-pointer.md.tmpl", struct{ Name string }{"demo"})
	if err != nil {
		// The data shape differs per template; the dispatch is what is under test.
		if !strings.Contains(err.Error(), "execute template") {
			t.Errorf("RenderCategory = %v", err)
		}
	} else if len(raw) == 0 {
		t.Error("RenderCategory rendered nothing")
	}
	if _, err := svc.RenderCategory("no-such-category", "x.tmpl", nil); err == nil || !strings.Contains(err.Error(), "read template x.tmpl") {
		t.Errorf("an unknown category must say the file was not found, got %v", err)
	}
	if _, err := svc.ListCategory("no-such-category", ""); err == nil {
		t.Error("an unknown category must fail to list")
	}
	if _, err := svc.RenderInternalPackageKind("no-such-kind", "x.tmpl", nil); err == nil {
		t.Error("an unknown kind must fail to render")
	}
}

func TestFuncMapHelpers(t *testing.T) {
	t.Run("hyphenToUnderscore", func(t *testing.T) {
		for in, want := range map[string]string{"": "", "plain": "plain", "my-svc": "my_svc", "a-b-c": "a_b_c"} {
			if got := hyphenToUnderscore(in); got != want {
				t.Errorf("hyphenToUnderscore(%q) = %q, want %q", in, got, want)
			}
		}
	})
	t.Run("toCamelCase", func(t *testing.T) {
		for in, want := range map[string]string{
			"":            "",
			"Pascal":      "pascal",
			"already":     "already",
			"snake_case":  "snakeCase",
			"two_parts_x": "twoPartsX",
			"Mixed_Start": "MixedStart",
			"a":           "a",
			"UserID":      "userID",
		} {
			if got := toCamelCase(in); got != want {
				t.Errorf("toCamelCase(%q) = %q, want %q", in, got, want)
			}
		}
	})
	t.Run("singularize", func(t *testing.T) {
		for in, want := range map[string]string{"": "", "users": "user", "categories": "category", "user": "user", "people": "person"} {
			if got := singularize(in); got != want {
				t.Errorf("singularize(%q) = %q, want %q", in, got, want)
			}
		}
	})
	t.Run("formatComment", func(t *testing.T) {
		if formatComment("") != "" || formatComment("note") != "// note" {
			t.Errorf("formatComment: %q / %q", formatComment(""), formatComment("note"))
		}
	})
	t.Run("add", func(t *testing.T) {
		if add(2, 3) != 5 || add(-1, 1) != 0 {
			t.Error("add is wrong")
		}
	})
	t.Run("lastStringSlice", func(t *testing.T) {
		s := []string{"a", "b", "c"}
		if !lastStringSlice(2, s) || lastStringSlice(1, s) || lastStringSlice(0, s) {
			t.Error("only the final index of a []string is last")
		}
		if lastStringSlice(0, []int{1}) || lastStringSlice(0, nil) || lastStringSlice(0, "str") {
			t.Error("anything but a []string is never last")
		}
		if !lastStringSlice(-1, []string{}) {
			t.Error("the empty slice's -1 index is, by the formula, its last")
		}
	})
	t.Run("tableFromFK and columnFromFK", func(t *testing.T) {
		for fk, want := range map[string][2]string{
			"users.id":      {"users", "id"},
			"orders.number": {"orders", "number"},
			"bare":          {"bare", "id"},
			"a.b.c":         {"a", "b"},
		} {
			if got := tableFromFK(fk); got != want[0] {
				t.Errorf("tableFromFK(%q) = %q, want %q", fk, got, want[0])
			}
			if got := columnFromFK(fk); got != want[1] {
				t.Errorf("columnFromFK(%q) = %q, want %q", fk, got, want[1])
			}
		}
	})
	t.Run("getDefault", func(t *testing.T) {
		if getDefault("d", nil) != "d" || getDefault("d", "") != "d" || getDefault("d", 0) != "d" {
			t.Error("a nil or zero value takes the default")
		}
		if getDefault("d", "v") != "v" || getDefault("d", 7) != 7 {
			t.Error("a set value wins")
		}
	})
}

func TestFrontendCIConfig(t *testing.T) {
	tests := []struct {
		pm       string
		wantPM   string
		lockfile string
		frozen   string
	}{
		{"", "pnpm", "pnpm-lock.yaml", "pnpm install --frozen-lockfile"},
		{"pnpm", "pnpm", "pnpm-lock.yaml", "pnpm install --frozen-lockfile"},
		{"npm", "npm", "package-lock.json", "npm ci"},
		{"yarn", "yarn", "yarn.lock", "yarn install --frozen-lockfile"},
	}
	for _, tt := range tests {
		fe := FrontendCIConfig{Name: "web", Path: "frontends/web", PackageManager: tt.pm}
		if fe.PM() != tt.wantPM || fe.Lockfile() != tt.lockfile || fe.FrozenInstall() != tt.frozen {
			t.Errorf("PackageManager %q: PM=%q Lockfile=%q FrozenInstall=%q", tt.pm, fe.PM(), fe.Lockfile(), fe.FrozenInstall())
		}
	}
}

func TestK3dLinuxInstallHint(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		hint := K3dLinuxInstallHint(arch)
		for _, want := range []string{k3dReleaseURL(arch), k3dLinuxSHA256[arch] + "  k3d", "sha256sum -c -", "/usr/local/bin/k3d"} {
			if !strings.Contains(hint, want) {
				t.Errorf("%s hint lacks %q: %s", arch, want, hint)
			}
		}
		if !strings.Contains(hint, "k3d-linux-"+arch) {
			t.Errorf("%s hint downloads the wrong binary: %s", arch, hint)
		}
	}
	for _, arch := range []string{"", "386", "riscv64"} {
		if got := K3dLinuxInstallHint(arch); got != "" {
			t.Errorf("an architecture with no pinned checksum must get no hint, got %q for %q", got, arch)
		}
	}
}

func TestFrontendTemplateRoots(t *testing.T) {
	tests := map[string][]string{
		"nextjs":       {"shared", "shared-web", "nextjs"},
		"vite-spa":     {"shared", "shared-web", "vite-spa"},
		"react-native": {"shared", "react-native"},
		"something":    {"something"},
		"":             {""},
	}
	for kind, want := range tests {
		if got := FrontendTemplateRoots(kind); !slices.Equal(got, want) {
			t.Errorf("FrontendTemplateRoots(%q) = %v, want %v", kind, got, want)
		}
	}
}

func TestListFrontendTree(t *testing.T) {
	files, err := ListFrontendTree("vite-spa")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no files in the vite-spa tree")
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Rel] {
			t.Errorf("%s appears twice: a shadowed file must appear once", f.Rel)
		}
		seen[f.Rel] = true
		if !strings.HasSuffix(f.Path, f.Rel) {
			t.Errorf("Path %q must end in its destination %q", f.Path, f.Rel)
		}
		root, _, _ := strings.Cut(f.Path, "/")
		if !slices.Contains(FrontendTemplateRoots("vite-spa"), root) {
			t.Errorf("%s resolved to %q, which is not one of the kind's roots", f.Rel, root)
		}
	}
	if !slices.IsSortedFunc(files, func(a, b FrontendTreeFile) int { return strings.Compare(a.Rel, b.Rel) }) {
		t.Error("the tree must be sorted by destination path")
	}
	if _, err := ListFrontendTree("no-such-kind"); err == nil {
		t.Error("an unknown kind has no tree to list")
	}
}

func TestEmbeddedTemplatesAreAllReadable(t *testing.T) {
	// Every embedded path is reachable through the same read the renderers use.
	err := fs.WalkDir(templateFS, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path.Ext(p) != ".tmpl" {
			return nil
		}
		if _, rerr := Source(p); rerr != nil {
			t.Errorf("Source(%q): %v", p, rerr)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestTSImportClassification(t *testing.T) {
	types := map[string]string{
		"@/lib/x":     "internal",
		"@/":          "internal",
		"node:fs":     "builtin",
		"fs":          "builtin",
		"fs/promises": "builtin",
		"path":        "builtin",
		"@scope/pkg":  "external",
		"@fs/thing":   "external",
		"react":       "external",
		"next/link":   "external",
		"..":          "parent",
		"../x":        "parent",
		"../../a/b":   "parent",
		".":           "index",
		"./":          "index",
		"./index":     "index",
		"./index.js":  "index",
		"./sibling":   "sibling",
		"./sub/dir":   "sibling",
		"fs-extra":    "external", // a different package from the builtin fs
	}
	for p, want := range types {
		if got := tsImportType(p); got != want {
			t.Errorf("tsImportType(%q) = %q, want %q", p, got, want)
		}
	}
	for p, want := range map[string]bool{"node:test": true, "os": true, "os/x": true, "@os/x": false, "osx": false, "": false} {
		if got := tsIsNodeBuiltin(p); got != want {
			t.Errorf("tsIsNodeBuiltin(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestCompareTSImportPaths(t *testing.T) {
	tests := []struct {
		a, b string
		want int // sign
	}{
		{"a", "b", -1},
		{"B", "a", 1}, // case-insensitive
		{"a", "A", 0},
		{"next/link", "next-auth", -1}, // segment-wise: "next" < "next-auth"
		{"next-auth", "next/link", 1},
		{"a/b", "a/b/c", -1}, // shorter first on a tie
		{"a/b/c", "a/b", 1},
		{"a/b", "a/b", 0},
		{"../a", "./a", 0},   // a leading ".." vs "." is a group discriminator, not a name: the rest ties
		{"../a", "../b", -1}, // both parent: the rest decides
		{"./a", "./b", -1},
		{"../a", "../a", 0},
		{"@/z", "@/a", 1},
	}
	sign := func(n int) int {
		switch {
		case n < 0:
			return -1
		case n > 0:
			return 1
		}
		return 0
	}
	for _, tt := range tests {
		if got := sign(compareTSImportPaths(tt.a, tt.b)); got != tt.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCanonicalTSImportOrder_LeavesWhatItCannotUnderstandAlone(t *testing.T) {
	cases := map[string]string{
		"no import block":              "const x = 1;\nimport b from \"b\";\nimport a from \"a\";\n",
		"code before the first import": "export const x = 1;\nimport b from \"b\";\nimport a from \"a\";\n",
		"a single import":              "import b from \"b\";\n\nconst x = b;\n",
		"a side-effect import":         "import \"./polyfill\";\nimport b from \"b\";\nimport a from \"a\";\n",
		"an unterminated statement":    "import {\n  a,\n  b,\n",
		"empty":                        "",
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			if got := string(CanonicalTSImportOrder([]byte(src))); got != src {
				t.Errorf("source it cannot reorder was rewritten:\n%s", got)
			}
		})
	}
}

func TestCanonicalTSImportOrder_BlockComments(t *testing.T) {
	src := "/* banner\n   spans lines */\n\"use client\";\n\n/** doc for b */\nimport b from \"b\";\n/* one-line */\nimport a from \"a\";\n\nexport {};\n"
	got := string(CanonicalTSImportOrder([]byte(src)))
	// The comment above the FIRST import is the file's banner and stays put;
	// the one between imports annotates the import after it and moves with it.
	want := "/* banner\n   spans lines */\n\"use client\";\n\n/** doc for b */\n/* one-line */\nimport a from \"a\";\nimport b from \"b\";\n\nexport {};\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}

	open := "/* never closed\nimport b from \"b\";\nimport a from \"a\";\n"
	if got := string(CanonicalTSImportOrder([]byte(open))); got != open {
		t.Errorf("an unterminated block comment hides the imports; got:\n%s", got)
	}
}

func TestCanonicalTSImportOrder_MultiLineStatementsMoveWhole(t *testing.T) {
	src := "import {\n  z,\n  y,\n} from \"zeta\";\nimport { a } from \"alpha\";\n"
	want := "import { a } from \"alpha\";\nimport {\n  z,\n  y,\n} from \"zeta\";\n"
	if got := string(CanonicalTSImportOrder([]byte(src))); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
