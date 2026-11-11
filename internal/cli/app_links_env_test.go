package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

func appLinkEnv(lifecycle string, domains ...string) (*KCLEntities, FrontendEntity) {
	fe := FrontendEntity{Name: "mobile", Type: "rn", Path: "frontends/mobile"}
	return &KCLEntities{
		Lifecycle: lifecycle,
		Identity: &release.ShapeIdentity{Name: "identity", Applications: []release.ShapeIdentityApplication{
			{Name: "dev:mobile", Kind: release.IdentityAppNative, Frontend: "mobile", AssociatedDomains: domains},
		}},
		Frontends: []FrontendEntity{fe},
	}, fe
}

func TestNativeAppLinkConfig(t *testing.T) {
	e, fe := appLinkEnv("local", "app.acme.example", "acme.example")
	got := nativeAppLinkConfig(e, fe)
	if !slices.Equal(got.Domains, []string{"app.acme.example", "acme.example"}) || !got.Developer {
		t.Fatalf("local = %+v", got)
	}
	wantVars := [][2]string{{"FORGE_ASSOCIATED_DOMAINS", "app.acme.example,acme.example"}, {"FORGE_APP_LINK_MODE", "developer"}}
	if !slices.Equal(got.vars(), wantVars) {
		t.Errorf("vars = %v", got.vars())
	}

	// Developer mode is for a local env only: forge never emits it for a
	// deployed env, whose build must read the association from Apple's CDN.
	for _, lifecycle := range []string{"", "ephemeral", "persistent"} {
		e, fe := appLinkEnv(lifecycle, "app.acme.example")
		c := nativeAppLinkConfig(e, fe)
		if c.Developer {
			t.Errorf("lifecycle %q got developer mode", lifecycle)
		}
		for _, kv := range c.vars() {
			if kv[0] == "FORGE_APP_LINK_MODE" {
				t.Errorf("lifecycle %q emitted %v", lifecycle, kv)
			}
		}
	}

	// No domains, no web frontend, no application: nothing, the project's
	// launch env is byte-identical to before.
	e, fe = appLinkEnv("local")
	if c := nativeAppLinkConfig(e, fe); len(c.vars()) != 0 {
		t.Errorf("no domains: %v", c.vars())
	}
	e, _ = appLinkEnv("local", "app.acme.example")
	if c := nativeAppLinkConfig(e, FrontendEntity{Name: "web", Type: "vite-spa"}); len(c.vars()) != 0 {
		t.Errorf("a web frontend got plugin vars: %v", c.vars())
	}
	if c := nativeAppLinkConfig(nil, fe); len(c.vars()) != 0 {
		t.Errorf("nil entities: %v", c.vars())
	}
}

func TestWithAppLinkEnv_NeverReplacesADeveloperValue(t *testing.T) {
	c := appLinkConfig{Domains: []string{"app.acme.example"}, Developer: true}
	env := nativeOriginEnvMap(withAppLinkEnv([]string{"FORGE_ASSOCIATED_DOMAINS=mine.example"}, c))
	if env["FORGE_ASSOCIATED_DOMAINS"] != "mine.example" || env["FORGE_APP_LINK_MODE"] != "developer" {
		t.Errorf("env = %v", env)
	}
	if got := withAppLinkEnv([]string{"A=1"}, appLinkConfig{}); !slices.Equal(got, []string{"A=1"}) {
		t.Errorf("no domains changed the env: %v", got)
	}
}

func TestWriteAppLinkConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mobile")
	path := filepath.Join(dir, ".forge-app-links.json")

	// No domains and no file: nothing is created, not even the directory.
	if err := writeAppLinkConfig(dir, appLinkConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("a frontend with no domains grew a directory")
	}

	c := appLinkConfig{Domains: []string{"app.acme.example"}, Developer: true}
	if err := writeAppLinkConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"FORGE_ASSOCIATED_DOMAINS": "app.acme.example"`, `"FORGE_APP_LINK_MODE": "developer"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("config = %s, missing %s", body, want)
		}
	}
	info, _ := os.Stat(path)
	if err := writeAppLinkConfig(dir, c); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.Stat(path); !again.ModTime().Equal(info.ModTime()) {
		t.Error("an unchanged config was rewritten")
	}

	// Declaring no domain any more removes the stale file.
	if err := writeAppLinkConfig(dir, appLinkConfig{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a stale association config survived")
	}
}
