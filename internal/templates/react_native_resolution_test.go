package templates

import (
	"encoding/json"
	"testing"
)

// Expo SDK 52 inherits legacy node resolution; generated hooks import package
// export-map subpaths, which must resolve through the same contract as Metro.
// The executable consumer fixture is the compile/bundle proof of this setting.
func TestReactNativeUsesExportMapTypeResolution(t *testing.T) {
	body, err := FrontendTemplates().Render("react-native/tsconfig.json", FrontendTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		CompilerOptions struct {
			ModuleResolution string `json:"moduleResolution"`
		} `json:"compilerOptions"`
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.CompilerOptions.ModuleResolution != "bundler" {
		t.Fatalf("native generated service-hooks require export-map resolution, got %q", cfg.CompilerOptions.ModuleResolution)
	}
}
