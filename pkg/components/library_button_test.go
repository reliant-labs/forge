package components

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Button is the most-used component in every frontend forge scaffolds,
// and it ships verbatim: a project cannot patch it without losing the edit
// on the next install. These guards pin the behaviour its header comment
// promises. Contrast across both scaffold palettes is checked separately in
// internal/templates/frontend_button_contrast_test.go, which can read the
// themes.

func buttonSource(t *testing.T) string {
	t.Helper()
	src, err := NewLibrary().Get("button")
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// TestButtonSizesShareOneScale: every size declares a height, a square for
// icon-only use and an icon size, so a toolbar of mixed buttons lines up.
func TestButtonSizesShareOneScale(t *testing.T) {
	t.Parallel()
	src := buttonSource(t)
	if !strings.Contains(src, `export type ButtonSize = "xs" | "sm" | "md" | "lg" | "xl";`) {
		t.Fatal("ButtonSize must be exactly xs | sm | md | lg | xl")
	}
	for _, want := range []struct{ size, height, square, icon string }{
		{"xs", "h-6", "size-6", "[&_svg]:size-3"},
		{"sm", "h-7", "size-7", "[&_svg]:size-3.5"},
		{"md", "h-8", "size-8", "[&_svg]:size-4"},
		{"lg", "h-9", "size-9", "[&_svg]:size-4"},
		{"xl", "h-10", "size-10", "[&_svg]:size-5"},
	} {
		block := regexp.MustCompile(`(?s)\n  ` + want.size + `: \{(.*?)\n  \},`).FindStringSubmatch(src)
		if block == nil {
			t.Errorf("sizeStyles has no %s entry", want.size)
			continue
		}
		for _, cls := range []string{want.height, want.square, want.icon} {
			if !strings.Contains(block[1], `"`+cls) && !strings.Contains(block[1], " "+cls) {
				t.Errorf("size %s: want %q in %s", want.size, cls, strings.TrimSpace(block[1]))
			}
		}
	}
}

// TestButtonLoadingHoldsItsWidth: a button that changes width when clicked
// moves the thing under the pointer. The label fades in place (it keeps its
// box and stays the accessible name) instead of being swapped for a spinner.
func TestButtonLoadingHoldsItsWidth(t *testing.T) {
	t.Parallel()
	src := buttonSource(t)
	for _, want := range []string{
		"loading?: boolean",
		"aria-busy={loading || undefined}",
		`const fade = spinnerOverlay ? "opacity-0" : "";`,
		"absolute inset-0", // the spinner overlays; it does not take a flex slot
		"disabled={inert}",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("button.tsx no longer contains %q", want)
		}
	}
	if strings.Contains(src, "isLoading") {
		t.Error("button.tsx: the loading prop is `loading`; `isLoading` was renamed (see the v0.1.48 upgrade playbook)")
	}
}

// TestButtonFocusIsAnOutline: forced-colors mode (Windows high contrast)
// drops box-shadow, so a `ring-*` focus indicator disappears for exactly the
// users who need it most. The indicator is a 2px outline offset from the
// edge, keyboard only.
func TestButtonFocusIsAnOutline(t *testing.T) {
	t.Parallel()
	src := buttonSource(t)
	for _, want := range []string{"focus-visible:outline-2", "focus-visible:outline-offset-2", "focus-visible:outline-accent"} {
		if !strings.Contains(src, want) {
			t.Errorf("button.tsx no longer contains %q", want)
		}
	}
	if regexp.MustCompile(`focus(-visible)?:ring-`).MatchString(src) {
		t.Error("button.tsx draws focus with a ring (box-shadow); forced-colors mode removes it — use an outline")
	}
	if strings.Contains(src, `"focus:outline-none`) || strings.Contains(src, " focus:outline-none") {
		t.Error("button.tsx removes the outline on :focus; that hides the indicator in browsers without :focus-visible heuristics")
	}
}

// TestButtonIconOnlyRequiresAName: the type, not a lint rule, makes an
// icon-only button carry an aria-label.
func TestButtonIconOnlyRequiresAName(t *testing.T) {
	t.Parallel()
	src := buttonSource(t)
	block := regexp.MustCompile(`(?s)interface IconOnlyContent \{(.*?)\n\}`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("button.tsx has no IconOnlyContent interface")
	}
	if !strings.Contains(block[1], `"aria-label": string;`) {
		t.Errorf("IconOnlyContent must require `\"aria-label\": string;`, got:\n%s", block[1])
	}
}

// TestButtonTypeContract runs tsc over testdata/typecheck/button_contract.tsx
// with the scaffolded tsconfig's strictness. The fixture's
// `@ts-expect-error` lines are misuses the type must reject; a loosened type
// turns each into an "unused directive" error.
func TestButtonTypeContract(t *testing.T) {
	if testing.Short() {
		t.Skip("-short: installs typescript and @types/react with npm; runs in the full tier")
	}
	npm, err := exec.LookPath("npm")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("npm (node) not on PATH under CI: install node so the Button type contract runs")
		}
		t.Skip("npm not on PATH")
	}

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	lib := NewLibrary()
	for _, name := range []string{"button", "link"} {
		body, err := lib.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		write("src/"+name+".tsx", body)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "typecheck", "button_contract.tsx"))
	if err != nil {
		t.Fatal(err)
	}
	write("src/button_contract.tsx", string(fixture))
	// The compiler options the scaffolded Next.js tsconfig sets that bear on
	// a component (internal/templates/frontend/nextjs/tsconfig.json.tmpl).
	write("tsconfig.json", `{"compilerOptions":{"target":"ES2022","lib":["dom","dom.iterable","esnext"],
"skipLibCheck":true,"strict":true,"noEmit":true,"esModuleInterop":true,"module":"esnext",
"moduleResolution":"bundler","isolatedModules":true,"noUncheckedIndexedAccess":true,"jsx":"react-jsx"},
"include":["src/**/*.tsx"]}`)
	// The scaffold's own ranges (nextjs/package.json.tmpl).
	write("package.json", `{"private":true,"devDependencies":{"typescript":"^5.8.0","@types/react":"^19.1.0"}}`)

	install := exec.Command(npm, "install", "--no-audit", "--no-fund", "--ignore-scripts", "--loglevel=error")
	install.Dir = dir
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("npm install: %v\n%s", err, out)
	}
	tsc := exec.Command(filepath.Join(dir, "node_modules", ".bin", "tsc"), "-p", ".")
	tsc.Dir = dir
	if out, err := tsc.CombinedOutput(); err != nil {
		t.Fatalf("the Button type contract failed:\n%s", out)
	}
}
