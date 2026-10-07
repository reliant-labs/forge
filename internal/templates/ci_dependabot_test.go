package templates

import (
	"testing"

	"gopkg.in/yaml.v3"
)

type dependabotConfig struct {
	Updates []struct {
		Ecosystem   string                    `yaml:"package-ecosystem"`
		Directory   string                    `yaml:"directory"`
		Directories []string                  `yaml:"directories"`
		Groups      map[string]map[string]any `yaml:"groups"`
	} `yaml:"updates"`
}

// Each Dependabot PR runs the whole CI; ungrouped entries mean one PR per dependency.
func TestDependabotTemplateGroupsEveryEcosystem(t *testing.T) {
	for _, frontend := range []string{"", "web"} {
		body, err := CITemplates("github").Render("dependabot.yml.tmpl", struct{ FrontendName string }{frontend})
		if err != nil {
			t.Fatalf("render (frontend %q): %v", frontend, err)
		}
		var cfg dependabotConfig
		if err := yaml.Unmarshal(body, &cfg); err != nil {
			t.Fatalf("invalid YAML (frontend %q): %v\n%s", frontend, err, body)
		}
		gomodEntries := 0
		for _, u := range cfg.Updates {
			if len(u.Groups) == 0 {
				t.Errorf("frontend %q: %s entry has no groups: — would open one PR per dependency", frontend, u.Ecosystem)
			}
			if u.Ecosystem == "gomod" {
				gomodEntries++
				if len(u.Directories) != 2 || u.Directories[0] != "/" || u.Directories[1] != "/gen" {
					t.Errorf("frontend %q: gomod directories = %v, want [/ /gen] in one entry", frontend, u.Directories)
				}
			}
		}
		if gomodEntries != 1 {
			t.Errorf("frontend %q: %d gomod entries, want exactly 1", frontend, gomodEntries)
		}
	}
}
