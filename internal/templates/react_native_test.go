package templates

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestReactNativeTemplatesList(t *testing.T) {
	files, err := FrontendTemplates().List("react-native")
	if err != nil {
		t.Fatalf("List react-native templates: %v", err)
	}

	expected := []string{
		"package.json.tmpl",
		"app.json.tmpl",
		"tsconfig.json",
		"babel.config.js",
		".gitignore",
		".env.local.tmpl",
		"buf.gen.yaml.tmpl",
		"src/lib/connect.ts.tmpl",
		"src/lib/query-client.ts",
		"src/hooks/use-api-query.ts",
		"src/hooks/use-api-mutation.ts",
		"app/_layout.tsx.tmpl",
		"app/index.tsx.tmpl",
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

func TestReactNativeTemplatesRender(t *testing.T) {
	data := FrontendTemplateData{
		FrontendName: "myapp",
		ProjectName:  "testproject",
		ApiUrl:       "http://localhost:8080",
		ApiPort:      "8080",
		Module:       "example.com/testproject",
	}

	files, err := FrontendTemplates().List("react-native")
	if err != nil {
		t.Fatalf("List react-native templates: %v", err)
	}

	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			content, err := FrontendTemplates().Render(filepath.Join("react-native", f), data)
			if err != nil {
				t.Fatalf("render %s: %v", f, err)
			}
			if len(content) == 0 {
				t.Errorf("rendered %s is empty", f)
			}
		})
	}

	// Verify specific template outputs
	t.Run("package.json contains expo", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("react-native/package.json.tmpl", data)
		s := string(content)
		if !strings.Contains(s, `"name": "myapp"`) {
			t.Error("package.json should contain frontend name")
		}
		if !strings.Contains(s, "expo") {
			t.Error("package.json should contain expo dependency")
		}
	})

	t.Run("connect.ts uses EXPO_PUBLIC_API_URL", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("react-native/src/lib/connect.ts.tmpl", data)
		s := string(content)
		if !strings.Contains(s, "EXPO_PUBLIC_API_URL") {
			t.Error("connect.ts should reference EXPO_PUBLIC_API_URL")
		}
		if !strings.Contains(s, "http://localhost:8080") {
			t.Error("connect.ts should contain rendered API URL")
		}
	})

	t.Run("layout contains proper JSX", func(t *testing.T) {
		content, _ := FrontendTemplates().Render("react-native/app/_layout.tsx.tmpl", data)
		s := string(content)
		if !strings.Contains(s, "myapp") {
			t.Error("_layout.tsx should contain rendered frontend name")
		}
		if !strings.Contains(s, "QueryClientProvider") {
			t.Error("_layout.tsx should contain QueryClientProvider")
		}
		// Verify JSX double braces are properly rendered (not Go template artifacts)
		if !strings.Contains(s, `options={{ title:`) {
			t.Errorf("_layout.tsx should have JSX options={{...}}, got:\n%s", s)
		}
	})
}