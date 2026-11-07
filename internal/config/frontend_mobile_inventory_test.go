package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeFrontendKindAliases(t *testing.T) {
	for input, want := range map[string]string{
		"rn": "react-native", "react-native": "react-native", "expo": "react-native",
		" RN ": "react-native", " React-Native ": "react-native", " EXPO ": "react-native",
		"vite": "vite-spa", "vite-spa": "vite-spa", "nextjs": "nextjs",
		"": "", " Custom ": " Custom ",
	} {
		if got := NormalizeFrontendKind(input); got != want {
			t.Errorf("NormalizeFrontendKind(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMobileInventoryDynamicExpoMarkers(t *testing.T) {
	for _, marker := range []string{"app.config.ts", "app.config.js"} {
		for _, tc := range []struct {
			name, pkg string
			want      bool
		}{
			{"dependency", `{"dependencies":{"expo":"~52.0.0"}}`, true},
			{"dev dependency", `{"devDependencies":{"expo":"workspace:*"}}`, true},
			{"missing package", "", false},
			{"malformed package", "{", false},
			{"unrelated", `{"name":"expo","scripts":{"dev":"expo start"}}`, false},
			{"native only", `{"dependencies":{"react-native":"*"}}`, false},
			{"empty dependency", `{"dependencies":{"expo":" "}}`, false},
			{"invalid dependency", `{"dependencies":{"expo":true}}`, false},
		} {
			t.Run(marker+"/"+tc.name, func(t *testing.T) {
				root := t.TempDir()
				writeMobileInventoryFile(t, root, "frontends/mobile/"+marker, "throw new Error('discovery must not execute config');")
				if tc.pkg != "" {
					writeMobileInventoryFile(t, root, "frontends/mobile/package.json", tc.pkg)
				}
				cfg := &ProjectConfig{}
				ResolveInventoryAtLoad(cfg, root)
				if !tc.want {
					if len(cfg.Frontends) != 0 {
						t.Fatalf("unexpected inventory: %+v", cfg.Frontends)
					}
					return
				}
				if len(cfg.Frontends) != 1 {
					t.Fatalf("inventory: %+v", cfg.Frontends)
				}
				fe := cfg.Frontends[0]
				if fe.Name != "mobile" || fe.Type != "react-native" || fe.DeclaredDir() != "frontends/mobile" {
					t.Fatalf("inventory: %+v", fe)
				}
			})
		}
	}
}

func TestMobileInventoryCompatibilityAndBoundaries(t *testing.T) {
	root := t.TempDir()
	// Legacy app.json remains sufficient, including for non-Expo RN apps.
	writeMobileInventoryFile(t, root, "frontends/legacy/app.json", "{}")
	writeMobileInventoryFile(t, root, "frontends/web/vite.config.ts", "")
	writeMobileInventoryFile(t, root, "frontends/package-only/package.json", `{"dependencies":{"expo":"*"}}`)
	writeMobileInventoryFile(t, root, "apps/native/app.config.ts", "")
	writeMobileInventoryFile(t, root, "apps/native/package.json", `{"dependencies":{"expo":"*"}}`)
	for _, marker := range []string{"app.json", "app.config.ts", "app.config.js"} {
		t.Run("root excluded/"+marker, func(t *testing.T) {
			dir := t.TempDir()
			writeMobileInventoryFile(t, dir, marker, "{}")
			writeMobileInventoryFile(t, dir, "package.json", `{"dependencies":{"expo":"*"}}`)
			if fe, ok := DiscoverRootFrontend(dir); ok {
				t.Fatalf("root native admitted: %+v", fe)
			}
		})
	}
	got := DiscoverInRepoFrontends(root)
	if len(got) != 2 || got[0].Name != "legacy" || got[0].Type != "react-native" || got[1].Type != "vite-spa" {
		t.Fatalf("conventional inventory: %+v", got)
	}
	for _, kind := range []string{"rn", "react-native", "expo"} {
		got := DeriveFrontendsFromKCL(root, []KCLFrontend{
			{Name: "native", Type: kind, Path: "apps/native"},
			{Name: "pinned", Type: kind, Path: "apps/native", HasSource: true},
			{Name: "outside", Type: kind, Path: t.TempDir()},
			{Name: "escape", Type: kind, Path: "../other"},
		})
		if len(got) != 1 || got[0].Name != "native" || got[0].Type != "react-native" || got[0].DeclaredDir() != "apps/native" {
			t.Fatalf("KCL %q inventory: %+v", kind, got)
		}
	}
}

func writeMobileInventoryFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
