package cli

import (
	"strings"
	"testing"
)

// staleHostingClaims are the guide's sentences about the staging/prod pair
// forge scaffolds onto Reliant hosting. They are true of a project forge
// scaffolded and false of one that declares other envs.
var staleHostingClaims = []string{
	"staging/prod are already bound",
	"Staging and prod are already bound",
	"Staging and prod are both bound",
	"env deploy staging",
	"env build prod",
	"--env prod",
}

// memoryWithEnvs renders the memory of a project whose deploy/kcl declares
// exactly envs, on top of files.
func memoryWithEnvs(t *testing.T, files map[string]string, envs ...string) string {
	t.Helper()
	tree := map[string]string{"deploy/kcl/kcl.mod": "[package]\nname = \"shop\"\n"}
	for _, e := range envs {
		tree["deploy/kcl/"+e+"/main.k"] = "import forge\n"
	}
	for k, v := range files {
		tree[k] = v
	}
	root := t.TempDir()
	writeTree(t, root, tree)
	return renderMemoryAs(t, root)
}

func goServiceFiles() map[string]string {
	return map[string]string{
		"forge.yaml":                          "name: shop\nmodule_path: example.com/shop\n",
		"go.mod":                              "module example.com/shop\n",
		"Taskfile.yml":                        "version: '3'\ntasks:\n  test: {cmds: [go test ./...]}\n",
		"internal/handlers/orders/service.go": "package orders\n",
	}
}

func siteFiles() map[string]string {
	return map[string]string{
		"forge.yaml":     "name: shop\n",
		"package.json":   `{"name":"shop","scripts":{"build":"next build"},"dependencies":{"next":"^16"}}`,
		"next.config.ts": "export default { output: \"export\" }\n",
	}
}

// A project whose envs are not staging and prod (reliant: dev, demo,
// desktop) is not told staging and prod are bound to Reliant hosting: its
// guide names the envs it declares and claims no hosting binding for them.
func TestMemoryNamesTheProjectsRealEnvs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, bind string
		files      map[string]string
	}{
		{"go-service", "<workload>", goServiceFiles()},
		{"static-site", "<frontend>", siteFiles()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			memory := memoryWithEnvs(t, tc.files, "dev", "demo", "desktop")
			for _, banned := range staleHostingClaims {
				if strings.Contains(memory, banned) {
					t.Errorf("memory for envs dev/demo/desktop claims %q", banned)
				}
			}
			for _, want := range []string{
				"This project's envs are `demo`, `desktop`, `dev`",
				"never assume one is hosted",
				"env new <name> --from <env> --bind " + tc.bind + "=hosted",
			} {
				if !strings.Contains(memory, want) {
					t.Errorf("memory for envs dev/demo/desktop lacks %q", want)
				}
			}
		})
	}
}

// A project with no deploy tree is told nothing is bound to hosting yet.
func TestMemoryWithNoEnvsClaimsNoHosting(t *testing.T) {
	t.Parallel()
	memory := memoryWithEnvs(t, goServiceFiles())
	for _, banned := range staleHostingClaims {
		if strings.Contains(memory, banned) {
			t.Errorf("memory with no envs claims %q", banned)
		}
	}
	if !strings.Contains(memory, "declares no env yet") {
		t.Error("memory with no envs does not say so")
	}
}

// The envs forge scaffolds keep the scaffold's wording: staging and prod
// ARE bound to hosting there.
func TestMemoryForScaffoldedEnvsKeepsTheHostingClaim(t *testing.T) {
	t.Parallel()
	goMemory := memoryWithEnvs(t, goServiceFiles(), "dev", "staging", "prod")
	if !strings.Contains(goMemory, "ships to Reliant hosting (staging/prod are already bound to it; `deploy`)") {
		t.Error("a scaffolded Go service's memory lost its hosting line")
	}
	siteMemory := memoryWithEnvs(t, siteFiles(), "dev", "staging", "prod")
	for _, want := range []string{
		"Staging and prod are already bound to Reliant hosted static hosting",
		"Staging and prod are both bound to hosting",
		"env deploy staging v0.1.0",
		"domain add <host> --env prod",
	} {
		if !strings.Contains(siteMemory, want) {
			t.Errorf("a scaffolded site's memory lacks %q", want)
		}
	}
	if strings.Contains(goMemory, "This project's envs are") || strings.Contains(siteMemory, "This project's envs are") {
		t.Error("a scaffolded project's memory describes its envs as non-scaffold")
	}
}
