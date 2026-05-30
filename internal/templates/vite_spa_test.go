package templates

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestViteSPATemplatesList confirms the embedded vite-spa template tree
// carries the files the generator depends on. Missing entries here would
// silently produce a half-scaffolded SPA where buf / vite / tanstack-router
// would fail at first run.
func TestViteSPATemplatesList(t *testing.T) {
	files, err := FrontendTemplates().List("vite-spa")
	if err != nil {
		t.Fatalf("List vite-spa templates: %v", err)
	}

	expected := []string{
		"package.json.tmpl",
		"vite.config.ts",
		"tsconfig.json",
		"tsconfig.node.json",
		"index.html.tmpl",
		".gitignore",
		".env.local.tmpl",
		"buf.gen.yaml.tmpl",
		"eslint.config.mjs",
		"src/main.tsx",
		"src/App.tsx.tmpl",
		"src/routes.tsx.tmpl",
		"src/index.css",
		"src/vite-env.d.ts",
		"src/stores/ui-store.ts",
		"src/lib/connect.ts.tmpl",
		"src/lib/query-client.ts",
		"src/lib/events.ts",
		"src/lib/event-context.tsx",
		"src/lib/search-schemas.ts",
		"src/lib/format-utils.ts",
		"src/lib/auth/provider.ts",
		"src/lib/auth/stub-provider.ts",
		"src/lib/auth/context.tsx",
		"src/hooks/use-api-query.ts",
		"src/hooks/use-api-mutation.ts",
	}

	fileSet := make(map[string]bool)
	for _, f := range files {
		fileSet[f] = true
	}

	for _, e := range expected {
		if !fileSet[e] {
			t.Errorf("expected template %s not found in listing", e)
		}
	}
}

// TestViteSPATemplatesRender exercises the full template tree through the
// shared template engine. Any unparseable Go template token, undefined
// data key, or empty rendered output gets flagged before the user hits it.
func TestViteSPATemplatesRender(t *testing.T) {
	data := FrontendTemplateData{
		FrontendName: "myspa",
		ProjectName:  "testproject",
		ApiUrl:       "http://localhost:8080",
		ApiPort:      "8080",
		Module:       "example.com/testproject",
	}

	files, err := FrontendTemplates().List("vite-spa")
	if err != nil {
		t.Fatalf("List vite-spa templates: %v", err)
	}

	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			content, err := FrontendTemplates().Render(filepath.Join("vite-spa", f), data)
			if err != nil {
				t.Fatalf("render %s: %v", f, err)
			}
			if len(content) == 0 {
				t.Errorf("rendered %s is empty", f)
			}
		})
	}

	t.Run("package.json contains vite + react", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("vite-spa/package.json.tmpl", data)
		s := string(content)
		if !strings.Contains(s, `"name": "myspa"`) {
			t.Error("package.json should contain frontend name")
		}
		if !strings.Contains(s, `"vite"`) {
			t.Error("package.json should contain vite dependency")
		}
		if !strings.Contains(s, "@tanstack/react-router") {
			t.Error("package.json should contain @tanstack/react-router")
		}
		if !strings.Contains(s, "@tanstack/react-query") {
			t.Error("package.json should contain @tanstack/react-query")
		}
		if !strings.Contains(s, "@tailwindcss/vite") {
			t.Error("package.json should contain @tailwindcss/vite")
		}
	})

	t.Run("connect.ts uses VITE_API_URL", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("vite-spa/src/lib/connect.ts.tmpl", data)
		s := string(content)
		if !strings.Contains(s, "VITE_API_URL") {
			t.Error("connect.ts should reference VITE_API_URL")
		}
		if !strings.Contains(s, "http://localhost:8080") {
			t.Error("connect.ts should contain rendered API URL")
		}
	})

	t.Run("routes.tsx wires tanstack-router", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("vite-spa/src/routes.tsx.tmpl", data)
		s := string(content)
		if !strings.Contains(s, "createRouter") {
			t.Error("routes.tsx should reference createRouter")
		}
		if !strings.Contains(s, "createRootRoute") {
			t.Error("routes.tsx should reference createRootRoute")
		}
		if !strings.Contains(s, "FORGE-ROUTES: BEGIN") {
			t.Error("routes.tsx should contain the FORGE-ROUTES marker block for page-generator integration")
		}
	})

	t.Run("main.tsx mounts the router", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("vite-spa/src/main.tsx", data)
		s := string(content)
		if !strings.Contains(s, "RouterProvider") {
			t.Error("main.tsx should mount RouterProvider")
		}
		if !strings.Contains(s, "createRoot") {
			t.Error("main.tsx should call createRoot")
		}
	})
}

// TestViteSPAPageTemplatesRender confirms the vite-spa-pages templates
// render against PageTemplateData (the same shape used for nextjs pages).
// This is the first-line guard that the tanstack-router rewrite of the
// page templates parses cleanly.
func TestViteSPAPageTemplatesRender(t *testing.T) {
	pages := []string{
		"list-page.tsx.tmpl",
		"detail-page.tsx.tmpl",
		"create-page.tsx.tmpl",
		"edit-page.tsx.tmpl",
		"oauth-callback-page.tsx.tmpl",
	}

	// Page templates consume codegen.PageTemplateData, but at the templates
	// package we can't import codegen (cycle). Use the same field surface
	// inline via a map literal — Go template lookups are field-name based
	// for both structs and `map[string]any`.
	data := map[string]any{
		"EntityName":         "Task",
		"EntityNamePlural":   "Tasks",
		"EntitySlug":         "tasks",
		"ServiceName":        "TaskService",
		"ServiceNameCamel":   "taskService",
		"HooksImportPath":    "@/hooks/task-service-hooks",
		"TypesImportPath":    "@/gen/services/tasks/v1/tasks_pb",
		"ListRPC":            "ListTasks",
		"GetRPC":             "GetTask",
		"CreateRPC":          "CreateTask",
		"UpdateRPC":          "UpdateTask",
		"DeleteRPC":          "DeleteTask",
		"HasList":            true,
		"HasGet":             true,
		"HasCreate":          true,
		"HasUpdate":          true,
		"HasDelete":          true,
		"CreateFields":       []any{},
		"UpdateFields":       []any{},
		"ListResponseType":   "ListTasksResponse",
		"GetResponseType":    "GetTaskResponse",
		"CreateRequestType":  "CreateTaskRequest",
		"CreateResponseType": "CreateTaskResponse",
		"UpdateRequestType":  "UpdateTaskRequest",
		"GetRequestType":     "GetTaskRequest",
		"DeleteRequestType":  "DeleteTaskRequest",
	}

	for _, p := range pages {
		p := p
		t.Run(p, func(t *testing.T) {
			content, err := FrontendTemplates().Render(filepath.Join("vite-spa-pages", p), data)
			if err != nil {
				t.Fatalf("render %s: %v", p, err)
			}
			if len(content) == 0 {
				t.Errorf("rendered %s is empty", p)
			}
			// Each page must reach for tanstack-router primitives — that's
			// the whole point of the vite-spa variant. Catch regressions
			// where someone accidentally ports a next/* import in.
			s := string(content)
			if strings.Contains(s, "next/navigation") || strings.Contains(s, "next/link") {
				t.Errorf("vite-spa page %s must not import next/* (got next/* reference)", p)
			}
			if p == "oauth-callback-page.tsx.tmpl" {
				if !strings.Contains(s, "@tanstack/react-router") {
					t.Errorf("%s must import from @tanstack/react-router", p)
				}
			}
		})
	}
}
