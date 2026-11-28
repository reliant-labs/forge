package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

func TestCovDPageLayoutForKind_PathPolicies(t *testing.T) {
	cases := []struct {
		feType                   string
		list, detail, create, ed string
	}{
		{"nextjs",
			filepath.Join("src", "app", "widgets", "page.tsx"),
			filepath.Join("src", "app", "widgets", "view", "page.tsx"),
			filepath.Join("src", "app", "widgets", "new", "page.tsx"),
			filepath.Join("src", "app", "widgets", "edit", "page.tsx")},
		{"vite-spa",
			filepath.Join("src", "pages", "widgets", "List.tsx"),
			filepath.Join("src", "pages", "widgets", "Detail.tsx"),
			filepath.Join("src", "pages", "widgets", "Create.tsx"),
			filepath.Join("src", "pages", "widgets", "Edit.tsx")},
	}
	for _, c := range cases {
		t.Run(c.feType, func(t *testing.T) {
			l, err := pageLayoutForKind(c.feType)
			if err != nil {
				t.Fatal(err)
			}
			if l.listTmpl == nil || l.detailTmpl == nil || l.createTmpl == nil || l.editTmpl == nil {
				t.Fatalf("every page template must parse: %+v", l)
			}
			for name, pair := range map[string][2]string{
				"list":   {l.listPath("widgets"), c.list},
				"detail": {l.detailPath("widgets"), c.detail},
				"create": {l.createPath("widgets"), c.create},
				"edit":   {l.editPath("widgets"), c.ed},
			} {
				if pair[0] != pair[1] {
					t.Errorf("%s path = %q, want %q", name, pair[0], pair[1])
				}
			}
			// No Next.js route may need generateStaticParams: the detail is
			// static and the id rides in the query string.
			if strings.Contains(l.detailPath("x"), "[id]") {
				t.Errorf("a static export cannot hold a dynamic detail route: %s", l.detailPath("x"))
			}
		})
	}
	for _, bad := range []string{"", "expo", "react-native", "NextJS"} {
		if _, err := pageLayoutForKind(bad); err == nil || !strings.Contains(err.Error(), "unsupported frontend type for page generation") {
			t.Errorf("pageLayoutForKind(%q) = %v, want an unsupported-type error", bad, err)
		}
	}
}

func TestCovDLoadPageTemplate_MissingTemplateNamesIt(t *testing.T) {
	if _, err := loadPageTemplate("pages", "no-such-page.tsx.tmpl"); err == nil || !strings.Contains(err.Error(), "read page template pages/no-such-page.tsx.tmpl") {
		t.Errorf("err = %v", err)
	}
	if _, err := loadPageTemplate("pages", "list-page.tsx.tmpl"); err != nil {
		t.Errorf("a shipped template must load with its shared partials: %v", err)
	}
}

func TestCovDLooksLikeGeneratedCRUDRouteDir_ViteAndUnknown(t *testing.T) {
	root := t.TempDir()
	both := filepath.Join(root, "widgets")
	mkfile(t, filepath.Join(both, "List.tsx"))
	mkfile(t, filepath.Join(both, "Detail.tsx"))
	if !looksLikeGeneratedCRUDRouteDir("vite-spa", both) {
		t.Error("List.tsx + Detail.tsx is the generated vite pair")
	}
	listOnly := filepath.Join(root, "about")
	mkfile(t, filepath.Join(listOnly, "List.tsx"))
	if looksLikeGeneratedCRUDRouteDir("vite-spa", listOnly) {
		t.Error("a lone List.tsx is a user's page")
	}
	if looksLikeGeneratedCRUDRouteDir("react-native", both) || looksLikeGeneratedCRUDRouteDir("", both) {
		t.Error("only nextjs and vite-spa have a generated CRUD fingerprint")
	}
}

func TestCovDReportStaleFrontendRouteDirs_PrintsOrphansOnly(t *testing.T) {
	root := t.TempDir()
	pages := filepath.Join(root, "src", "pages")
	mkfile(t, filepath.Join(pages, "gadgets", "List.tsx"))
	mkfile(t, filepath.Join(pages, "gadgets", "Detail.tsx"))
	mkfile(t, filepath.Join(pages, "widgets", "List.tsx"))
	mkfile(t, filepath.Join(pages, "widgets", "Detail.tsx"))
	mkfile(t, filepath.Join(pages, "home", "Home.tsx"))

	_, stderr := captureStdoutAndStderr(t, func() {
		reportStaleFrontendRouteDirs("vite-spa", root, "web", map[string]bool{"gadgets": true})
	})
	if !strings.Contains(stderr, "frontend web: 1 generated CRUD route dir(s) no longer match a live entity") {
		t.Errorf("stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, filepath.Join(pages, "widgets")+"/") {
		t.Errorf("the orphan must be named:\n%s", stderr)
	}
	for _, kept := range []string{"gadgets", "home"} {
		if strings.Contains(stderr, filepath.Join(pages, kept)) {
			t.Errorf("%s is live or hand-written and must not be reported:\n%s", kept, stderr)
		}
	}

	// Nothing stale, an unknown type and a missing routes dir are all silent.
	for name, run := range map[string]func(){
		"all live": func() {
			reportStaleFrontendRouteDirs("vite-spa", root, "web", map[string]bool{"gadgets": true, "widgets": true})
		},
		"unknown type": func() { reportStaleFrontendRouteDirs("expo", root, "web", nil) },
		"no routes":    func() { reportStaleFrontendRouteDirs("nextjs", t.TempDir(), "web", nil) },
	} {
		if _, stderr := captureStdoutAndStderr(t, run); stderr != "" {
			t.Errorf("%s: expected silence, got %q", name, stderr)
		}
	}
}

func TestCovDInputFields(t *testing.T) {
	a, b := map[string]bool{"LogLevel": true}, map[string]bool{"Port": true}
	got := inputFields([]codegen.ServeConfigInput{{Name: "A", Fields: a}, {Name: "B", Fields: b}, {Name: "C"}})
	if len(got) != 3 || !got[0]["LogLevel"] || !got[1]["Port"] || got[2] != nil {
		t.Errorf("inputFields = %v", got)
	}
	if got := inputFields(nil); len(got) != 0 {
		t.Errorf("no inputs, no sets: %v", got)
	}
}

func TestCovDConfigFieldsExcludingBinaries(t *testing.T) {
	field := func(name string) codegen.ConfigField { return codegen.ConfigField{Name: name, GoName: name} }
	blockField := codegen.ConfigField{Name: "base", GoName: "Base", MessageType: "BaseConfig"}
	messages := []codegen.ConfigMessage{
		{Name: "AppConfig", Fields: []codegen.ConfigField{field("log_level"), blockField}},
		{Name: "BaseConfig", Fields: []codegen.ConfigField{field("base_domain")}},
		{Name: "WorkerConfig", Binary: "worker", Fields: []codegen.ConfigField{field("queue")}},
		{Name: "WebConfig", Frontend: "web", Fields: []codegen.ConfigField{field("api_url")}},
	}
	got := configFieldsExcludingBinaries(messages)
	names := map[string]bool{}
	for _, f := range got {
		names[f.Name] = true
	}
	if !names["log_level"] {
		t.Errorf("the project-global config's own fields stay: %v", names)
	}
	for _, dropped := range []string{"queue", "api_url"} {
		if names[dropped] {
			t.Errorf("%s belongs to a binary or frontend, not the shared root: %v", dropped, names)
		}
	}
	// BaseConfig is composed by AppConfig, so it is reached only through the
	// composing field, never as a top-level entry of its own.
	count := 0
	for _, f := range got {
		if f.Name == "base_domain" {
			count++
		}
	}
	if count > 1 {
		t.Errorf("a composed block's leaf must appear once, got %d", count)
	}
}

func TestCovDGraphRootsOrNil(t *testing.T) {
	if graphRootsOrNil(nil) != nil {
		t.Error("no provider, no roots")
	}
	want := []codegen.GraphRoot{{}, {}}
	if got := graphRootsOrNil(func() []codegen.GraphRoot { return want }); len(got) != 2 {
		t.Errorf("got %v", got)
	}
}
