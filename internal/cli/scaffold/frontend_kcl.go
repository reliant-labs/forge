package scaffold

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/naming"
)

// declareFrontendInKCL adds a freshly-scaffolded frontend to every
// environment's deploy/kcl/<env>/main.k, the same declaration
// `forge project new --frontend` renders from the env templates.
//
// It exists because the declaration is load-bearing, not decorative. In the
// dev env the frontend's port is a KCL fact (`plugin.resolve_port`), and
// `forge env up` reads the frontend set — its path, its dev_runner, and above
// all its port — from the render. A frontend present only in forge.yaml was
// bridged in with forge.yaml's literal `port:` (3000 by default), so the port
// preflight probed 3000, found it taken, and refused to start: the frontend
// the user had just added could not be run. In staging/prod the declaration
// is where its runtime is bound, and a frontend missing from it is one
// that ships nowhere without anyone having decided that.
//
// The edit is two insertions per env and never a rewrite:
//
//   - dev only, unless --port pinned a literal: `_<ident>_frontend_port = plugin.resolve_port(...)`, placed
//     before `_database_url` with the other port declarations. resolve_port
//     (not allocate_port) because nothing outside this project has been told
//     the number: it takes 3000 when free, steps when not, and remembers.
//   - every env: `frontends += [forge.Frontend {...}]` just before the
//     bundle's `secret_provider = ` line. `+=` rather than `=` so the edit composes with
//     a list the file already declares — an earlier frontend, or one the user
//     wrote by hand — instead of replacing it.
//
// FAILURE IS ADVISORY, like declareWorkloadInKCL: the frontend's code is
// already written. An env file that is missing, already names the frontend,
// or has been restructured past the point where the anchors are unambiguous
// is left untouched and the stanza is printed for the user to place.
func declareFrontendInKCL(root, projectName, frontendName string, pinnedPort int) {
	envs, err := os.ReadDir(filepath.Join(root, "deploy", "kcl"))
	if err != nil {
		return // no deploy/ tree (e.g. --disable deploy): nothing to declare into
	}
	for _, env := range envs {
		if !env.IsDir() {
			continue
		}
		rel := filepath.Join("deploy", "kcl", env.Name(), "main.k")
		path := filepath.Join(root, rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue // not an env directory
		}
		dev := env.Name() == "dev"
		updated, status := spliceFrontendIntoEnvKCL(string(raw), projectName, env.Name(), frontendName, dev, pinnedPort)
		switch status {
		case frontendKCLApplied:
			if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
				fmt.Printf("\n⚠️  could not update %s: %v\n\n%s\n", rel, err,
					frontendKCLStanzaHint(projectName, env.Name(), frontendName, dev, pinnedPort))
				continue
			}
			fmt.Printf("   - %s (frontend '%s' declared)\n", rel, frontendName)
		case frontendKCLAlreadyDeclared:
			// Quiet: an earlier run, or the user, already declared it.
		case frontendKCLNoAnchor:
			fmt.Printf("\n📝 %s\n", frontendKCLStanzaHint(projectName, env.Name(), frontendName, dev, pinnedPort))
		}
	}
}

type frontendKCLStatus int

const (
	frontendKCLApplied frontendKCLStatus = iota
	frontendKCLAlreadyDeclared
	frontendKCLNoAnchor
)

// bundleSecretProviderLine is the `secret_provider = ` entry of the env's
// bundle literal — the one field every scaffolded env declares, at
// four-space indentation, directly inside `_bundle = forge.Bundle {`. It
// anchors the frontends insertion.
var bundleSecretProviderLine = regexp.MustCompile(`(?m)^    secret_provider = `)

// databaseURLLine anchors the dev port declaration: the scaffolded dev env
// declares every port first and then composes `_database_url` from them.
var databaseURLLine = regexp.MustCompile(`(?m)^_database_url = `)

// frontendDeclaredIn reports whether content already declares a
// forge.Frontend named name. A frontend name is unique within an env, so a
// `name = "<name>"` line inside any forge.Frontend literal is enough — the
// same looseness workloadDeclaredIn accepts, for the same reason: a false
// "already declared" costs a printed hint, a false "not declared" costs a
// duplicate that KCL would reject.
func frontendDeclaredIn(content, name string) bool {
	re := regexp.MustCompile(`(?s)forge\.Frontend\s*\{[^}]*?name\s*=\s*"` + regexp.QuoteMeta(name) + `"`)
	return re.MatchString(content)
}

// frontendPortIdent is the KCL variable holding a frontend's dev port.
func frontendPortIdent(frontendName string) string {
	return "_" + naming.KCLIdentifier(frontendName) + "_frontend_port"
}

func frontendKCLEntry(frontendName string, dev bool, pinnedPort int) string {
	var b strings.Builder
	b.WriteString("    # Added by `forge scaffold frontend " + frontendName + "`. `+=` composes with\n")
	b.WriteString("    # any frontends declared above; edit freely (its runtime, dev_runner, ...).\n")
	b.WriteString("    frontends += [forge.Frontend {\n")
	fmt.Fprintf(&b, "        name = %q\n", frontendName)
	fmt.Fprintf(&b, "        path = %q\n", "frontends/"+frontendName)
	switch {
	case dev && pinnedPort > 0:
		fmt.Fprintf(&b, "        port = %d\n", pinnedPort)
	case dev:
		fmt.Fprintf(&b, "        port = %s\n", frontendPortIdent(frontendName))
	}
	// Every frontend binds a runtime (ADR 0002 §6): the dev server in dev;
	// elsewhere the author's own bucket, whose name forge does not guess —
	// bucket names are global. `forge env new --check` refuses the
	// placeholder until it is filled.
	if dev {
		b.WriteString("        runtime = forge.OnHost {}\n")
	} else {
		b.WriteString("        public_dir = \"out\"\n")
		b.WriteString("        runtime = forge.OnBucket {bucket = \"REPLACE_ME_BUCKET\"}\n")
	}
	b.WriteString("    }]\n")
	return b.String()
}

func frontendKCLPortDecl(projectName, env, frontendName string) string {
	return fmt.Sprintf("# The %s frontend's dev-server port. resolve_port takes 3000 when free,\n"+
		"# steps to the next free port when not, and remembers the answer — so a\n"+
		"# frontend added next to anything already on 3000 still starts.\n"+
		"%s = plugin.resolve_port(%q, 3000)\n\n",
		frontendName, frontendPortIdent(frontendName), projectName+"-"+env+"-"+frontendName)
}

// spliceFrontendIntoEnvKCL is the pure core of declareFrontendInKCL.
func spliceFrontendIntoEnvKCL(content, projectName, env, frontendName string, dev bool, pinnedPort int) (string, frontendKCLStatus) {
	if frontendDeclaredIn(content, frontendName) {
		return content, frontendKCLAlreadyDeclared
	}
	jobs := bundleSecretProviderLine.FindAllStringIndex(content, -1)
	if len(jobs) != 1 {
		return content, frontendKCLNoAnchor
	}
	var portAt []int
	if dev && pinnedPort <= 0 {
		dbs := databaseURLLine.FindAllStringIndex(content, -1)
		if len(dbs) != 1 || dbs[0][0] > jobs[0][0] {
			return content, frontendKCLNoAnchor
		}
		portAt = dbs[0]
	}
	// Insert from the end backwards so the earlier offset stays valid.
	out := content[:jobs[0][0]] + frontendKCLEntry(frontendName, dev, pinnedPort) + content[jobs[0][0]:]
	if portAt != nil {
		out = out[:portAt[0]] + frontendKCLPortDecl(projectName, env, frontendName) + out[portAt[0]:]
	}
	return out, frontendKCLApplied
}

func frontendKCLStanzaHint(projectName, env, frontendName string, dev bool, pinnedPort int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Declare frontend '%s' in deploy/kcl/%s/main.k by hand (forge could not place it unambiguously):\n\n", frontendName, env)
	if dev && pinnedPort <= 0 {
		b.WriteString(frontendKCLPortDecl(projectName, env, frontendName))
	}
	b.WriteString("    # inside the bundle, alongside workloads:\n")
	b.WriteString(frontendKCLEntry(frontendName, dev, pinnedPort))
	return b.String()
}
