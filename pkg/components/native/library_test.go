// Copyright (c) 2025 Reliant Labs
package native

import (
	"strings"
	"testing"
)

// TestPrimitivesEmbed asserts every primitive in the registry has a
// non-empty embedded source. Catches the common slip of adding an
// entry to `primitives` but forgetting to commit the .tsx file (the
// embed pattern is silent on missing files until first Read).
func TestPrimitivesEmbed(t *testing.T) {
	lib := NewLibrary()
	if len(lib.Primitives()) != 10 {
		t.Fatalf("expected 10 primitives, got %d — keep the surface deliberately small", len(lib.Primitives()))
	}
	for _, p := range lib.Primitives() {
		src, err := lib.Get(p.Name)
		if err != nil {
			t.Errorf("primitive %q: %v", p.Name, err)
			continue
		}
		if len(src) == 0 {
			t.Errorf("primitive %q: empty source", p.Name)
		}
		if !strings.Contains(src, "react-native") {
			t.Errorf("primitive %q: source does not import from react-native — likely a wrong file embedded", p.Name)
		}
	}
}

// TestTokensEmbed asserts the design-tokens file ships and exposes
// the canonical `colors` / `spacing` / `radius` / `textSizes`
// exports — components rely on those names.
func TestTokensEmbed(t *testing.T) {
	lib := NewLibrary()
	src, err := lib.Tokens()
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	for _, sym := range []string{"colors", "spacing", "radius", "textSizes"} {
		if !strings.Contains(src, "export const "+sym) {
			t.Errorf("tokens.ts missing `export const %s` — components depend on this symbol", sym)
		}
	}
}

// TestIndexBarrelCoversEveryPrimitive asserts the generated barrel
// re-exports every primitive — adding a new one without picking it up
// in the barrel would silently ship a component that consumers can't
// import from the package root.
func TestIndexBarrelCoversEveryPrimitive(t *testing.T) {
	lib := NewLibrary()
	barrel := lib.IndexBarrel()
	for _, p := range lib.Primitives() {
		path := "./components/" + p.Name
		if !strings.Contains(barrel, path) {
			t.Errorf("barrel missing re-export for %q (path %s)", p.Name, path)
		}
	}
	// The Stack file exports HStack/VStack as named exports — they
	// must also appear in the barrel.
	if !strings.Contains(barrel, "HStack") || !strings.Contains(barrel, "VStack") {
		t.Errorf("barrel missing HStack/VStack named re-exports:\n%s", barrel)
	}
	// Tokens are re-exported via `export * from "./tokens"`.
	if !strings.Contains(barrel, `export * from "./tokens"`) {
		t.Errorf("barrel missing tokens re-export:\n%s", barrel)
	}
}

// TestDefaultExportName asserts kebab→PascalCase symbol generation —
// "safe-area-view" needs to become SafeAreaView for the import to
// land on the actual default export.
func TestDefaultExportName(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"button", "Button"},
		{"safe-area-view", "SafeAreaView"},
		{"spinner", "Spinner"},
		{"text", "Text"},
	}
	for _, tt := range tests {
		if got := defaultExportName(tt.in); got != tt.want {
			t.Errorf("defaultExportName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// React Native 0.87 (Expo SDK 58) made the Strict TypeScript API the default,
// whose TextInput ref is TextInputInstance. Older RN has no such export. Derive
// the ref from TextInput so a ui-native package can serve SDK 52 or SDK 58.
// Consumer fixtures and the dual-RN type probe are the compile evidence.
func TestInputForwardsVersionNeutralRef(t *testing.T) {
	src, err := NewLibrary().Get("input")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"export type InputRef = React.ComponentRef<typeof TextInput>", "forwardRef<InputRef, InputProps>", "RefAttributes<InputRef>"} {
		if !strings.Contains(src, want) {
			t.Errorf("input must forward a version-neutral TextInput ref; missing %q", want)
		}
	}
	if strings.Contains(src, "type TextInputInstance") || strings.Contains(src, "<TextInputInstance") {
		t.Error("input must not import RN 0.87+-only TextInputInstance")
	}
}
