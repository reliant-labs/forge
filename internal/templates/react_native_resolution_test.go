package templates

import (
	"encoding/json"
	"testing"
)

// Expo SDK 58's expo/tsconfig.base sets moduleResolution "bundler" and the
// "react-native" custom condition, so generated hooks resolve the runtime's
// export-map subpaths through the base. The owned tsconfig must not shadow it,
// and must not use baseUrl, which TypeScript 6 (SDK 58's pinned compiler) rejects
// with TS5101. The executable consumer fixture is the compile/bundle proof.
func TestReactNativeTsconfigInheritsSDK58Resolution(t *testing.T) {
	body, err := FrontendTemplates().Render("react-native/tsconfig.json", FrontendTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Extends         string `json:"extends"`
		CompilerOptions map[string]any
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Extends != "expo/tsconfig.base" {
		t.Fatalf("extends = %q, want expo/tsconfig.base", cfg.Extends)
	}
	for _, key := range []string{"baseUrl", "moduleResolution"} {
		if _, ok := cfg.CompilerOptions[key]; ok {
			t.Errorf("compilerOptions.%s is set; SDK 58's base supplies resolution and TS 6 rejects baseUrl", key)
		}
	}
	if _, ok := cfg.CompilerOptions["paths"]; !ok {
		t.Error("the @/* path alias is missing")
	}
}

// The scaffold does not enable experiments.typedRoutes. With it off, Expo CLI
// 58 (startTypescriptTypeGeneration -> forceRemovalTSConfig) strips
// ".expo/types/**/*.ts" and "expo-env.d.ts" from tsconfig "include" on every
// `expo start`, so listing them made the owned file churn on first run. Both
// halves must stay in step: enabling typedRoutes means adding them back.
func TestReactNativeTsconfigMatchesTypedRoutesSetting(t *testing.T) {
	ts, err := FrontendTemplates().Render("react-native/tsconfig.json", FrontendTemplateData{})
	if err != nil {
		t.Fatal(err)
	}
	app, err := FrontendTemplates().Render("react-native/app.json.tmpl", FrontendTemplateData{FrontendName: "mobile", ProjectName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct{ Include []string }
	var appCfg struct {
		Expo struct {
			Experiments struct {
				TypedRoutes bool `json:"typedRoutes"`
			} `json:"experiments"`
		} `json:"expo"`
	}
	if err := json.Unmarshal(ts, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(app, &appCfg); err != nil {
		t.Fatal(err)
	}
	has := map[string]bool{}
	for _, inc := range cfg.Include {
		has[inc] = true
	}
	typed := has[".expo/types/**/*.ts"] || has["expo-env.d.ts"]
	if typed != appCfg.Expo.Experiments.TypedRoutes {
		t.Errorf("tsconfig typed-route includes = %v but app.json experiments.typedRoutes = %v; Expo rewrites tsconfig on start when they disagree", typed, appCfg.Expo.Experiments.TypedRoutes)
	}
}

// newArchEnabled was removed from the Expo config schema in SDK 55 (the New
// Architecture is mandatory) and expo-doctor fails an app.json that sets it.
func TestReactNativeAppJSONHasNoRemovedFields(t *testing.T) {
	body, err := FrontendTemplates().Render("react-native/app.json.tmpl", FrontendTemplateData{FrontendName: "mobile", ProjectName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	var app struct{ Expo map[string]any }
	if err := json.Unmarshal(body, &app); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"newArchEnabled", "edgeToEdgeEnabled", "jsEngine"} {
		if _, ok := app.Expo[field]; ok {
			t.Errorf("app.json sets %q, which Expo SDK 58 no longer accepts", field)
		}
	}
}
