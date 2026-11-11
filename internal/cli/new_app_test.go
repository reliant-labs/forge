package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/deploy"
	"github.com/reliant-labs/forge/pkg/testkit"
)

// writeTree lays files down under a fresh directory and returns it.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// nodeExpressApp is an Express API with its own Dockerfile and no compose
// file, no database and no Go.
func nodeExpressApp(t *testing.T) string {
	return writeTree(t, map[string]string{
		"package.json":  `{"name":"todo-api","scripts":{"dev":"node --watch src/server.js","start":"node src/server.js"},"dependencies":{"express":"^4.19.0"}}`,
		"src/server.js": "const express = require('express');\nconst app = express();\napp.get('/health', (req, res) => res.json({ok: true}));\napp.listen(process.env.PORT || 3000);\n",
		"Dockerfile":    "FROM node:20-slim\nWORKDIR /app\nCOPY package.json ./\nRUN npm install --omit=dev\nCOPY src ./src\nEXPOSE 3000\nCMD [\"node\", \"src/server.js\"]\n",
		".gitignore":    "node_modules/\n",
	})
}

// pythonComposeApp is a FastAPI app with its own Dockerfile and a compose
// file running it beside Postgres and Redis, migrating with alembic.
func pythonComposeApp(t *testing.T) string {
	return writeTree(t, map[string]string{
		"requirements.txt": "fastapi==0.115.0\nuvicorn[standard]==0.30.0\npsycopg[binary]==3.2.1\nalembic==1.13.2\n",
		"app/main.py":      "from fastapi import FastAPI\napp = FastAPI()\n\n@app.get(\"/health\")\ndef health():\n    return {\"ok\": True}\n",
		"alembic.ini":      "[alembic]\nscript_location = migrations\n",
		"Dockerfile":       "FROM python:3.12-slim\nWORKDIR /srv\nCOPY requirements.txt .\nRUN pip install --no-cache-dir -r requirements.txt\nCOPY . .\nCMD [\"uvicorn\", \"app.main:app\", \"--host\", \"0.0.0.0\", \"--port\", \"8000\"]\n",
		"docker-compose.yml": `services:
  web:
    build: .
    ports:
      - "8000:8000"
    environment:
      DATABASE_URL: postgresql://app:app@db:5432/notes
      LOG_LEVEL: info
      SESSION_SECRET: dev-secret
    depends_on:
      - db
  db:
    image: postgres:16
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: app
      POSTGRES_DB: notes
  redis:
    image: redis:7
  adminer:
    image: adminer
`,
	})
}

// snapshotTree maps every file under root (but forge's own) to its bytes.
func snapshotAppTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		data, err := os.ReadFile(p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func adoptApp(t *testing.T, root, name string, claimable, dryRun bool) error {
	t.Helper()
	return runNewApp(newAppInput{projectName: name, projectPath: root, inPlace: true, claimable: claimable, dryRun: dryRun})
}

// What --kind app reads off an Express app: one service from the root
// Dockerfile on its EXPOSEd port, dev running its `dev` script on the host,
// and no database.
func TestDetectApp_NodeExpress(t *testing.T) {
	root := nodeExpressApp(t)
	det, err := detectApp(root, "todo", false)
	if err != nil {
		t.Fatal(err)
	}
	s := det.spec
	if len(s.Workloads) != 1 || s.ComposeFile != "" || s.Database != nil || s.Migrate != nil {
		t.Fatalf("spec = %+v, want one workload, no compose, no database", s)
	}
	w := s.Workloads[0]
	if w.Name != "api" || w.Kind != "service" || w.Port != 3000 || w.Build == nil || w.Build.Dockerfile != "Dockerfile" {
		t.Errorf("workload = %+v, want service api on 3000 built from Dockerfile", w)
	}
	if strings.Join(w.DevCommand, " ") != "npm run dev" {
		t.Errorf("dev command = %v, want npm run dev", w.DevCommand)
	}
	if strings.Join(det.dockerignoreMissing, ",") != "deploy,.forge,secrets" {
		t.Errorf("dockerignore advice = %v", det.dockerignoreMissing)
	}
}

// What --kind app reads off a compose app: the built service on its
// published port with its literal env, DATABASE_URL from the managed
// Postgres the compose `db` becomes, the credential as a secret, alembic as
// the migration job, Redis as a hosted third-party image and adminer dev-only.
func TestDetectApp_PythonCompose(t *testing.T) {
	root := pythonComposeApp(t)
	det, err := detectApp(root, "notes", false)
	if err != nil {
		t.Fatal(err)
	}
	s := det.spec
	if s.ComposeFile != "docker-compose.yml" {
		t.Errorf("compose file = %q", s.ComposeFile)
	}
	if s.Database == nil || s.Database.Name != "notes" || s.Database.ComposeService != "db" {
		t.Fatalf("database = %+v, want notes from compose service db", s.Database)
	}
	var names []string
	byName := map[string]int{}
	for i, w := range s.Workloads {
		names = append(names, w.Name)
		byName[w.Name] = i
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "redis,web" {
		t.Fatalf("workloads = %v, want redis and web", names)
	}
	web := s.Workloads[byName["web"]]
	if web.Port != 8000 || !web.UsesDatabase || web.Env["LOG_LEVEL"] != "info" || web.Env["DATABASE_URL"] != "" || web.Env["SESSION_SECRET"] != "" {
		t.Errorf("web = %+v, want port 8000, DATABASE_URL from the database, LOG_LEVEL literal, SESSION_SECRET not literal", web)
	}
	if strings.Join(web.Secrets, ",") != "SESSION_SECRET" || strings.Join(det.secrets, ",") != "SESSION_SECRET" {
		t.Errorf("secrets = %v / %v, want SESSION_SECRET", web.Secrets, det.secrets)
	}
	if redis := s.Workloads[byName["redis"]]; redis.Build != nil || redis.Image != "docker.io/library/redis:7" || redis.Expose {
		t.Errorf("redis = %+v, want the third-party image, internal only", redis)
	}
	if s.Migrate == nil || strings.Join(s.Migrate.Command, " ") != "alembic upgrade head" || s.Migrate.Workload != "web" {
		t.Errorf("migrate = %+v, want alembic upgrade head from web's image", s.Migrate)
	}
	var devOnly []string
	for _, d := range s.DevOnly {
		devOnly = append(devOnly, d.ComposeService)
	}
	sort.Strings(devOnly)
	if strings.Join(devOnly, ",") != "adminer,db" {
		t.Errorf("dev-only services = %v, want adminer and db", devOnly)
	}
}

// --dry-run prints and writes nothing; a real run adds only forge.yaml and
// deploy/kcl (and forge's block in .gitignore), and never a Go scaffold.
func TestNewApp_WritesOnlyTheDeclarations(t *testing.T) {
	testkit.Slow(t, "the .gitignore merge execs git")
	root := nodeExpressApp(t)
	before := snapshotAppTree(t, root)
	if err := adoptApp(t, root, "todo", false, true); err != nil {
		t.Fatal(err)
	}
	if after := snapshotAppTree(t, root); len(after) != len(before) {
		t.Fatalf("--dry-run wrote files: %d before, %d after", len(before), len(after))
	}

	if err := adoptApp(t, root, "todo", false, false); err != nil {
		t.Fatal(err)
	}
	after := snapshotAppTree(t, root)
	for rel, body := range before {
		if rel == ".gitignore" {
			if !strings.HasPrefix(after[rel], body) || !strings.Contains(after[rel], "secrets/") {
				t.Errorf(".gitignore was not merged additively:\n%s", after[rel])
			}
			continue
		}
		if after[rel] != body {
			t.Errorf("the app's %s changed", rel)
		}
	}
	var added []string
	for rel := range after {
		if _, ok := before[rel]; !ok {
			added = append(added, rel)
		}
	}
	sort.Strings(added)
	want := "deploy/kcl/dev/main.k,deploy/kcl/kcl.mod,deploy/kcl/prod/main.k,deploy/kcl/staging/main.k,deploy/kcl/workloads.k,forge.yaml"
	if strings.Join(added, ",") != want {
		t.Errorf("added %v, want exactly %s", added, want)
	}
	if yaml := after["forge.yaml"]; !strings.Contains(yaml, "name: todo") || strings.Contains(yaml, "\nmodule_path:") || strings.Contains(yaml, "\nharness:") {
		t.Errorf("forge.yaml:\n%s", yaml)
	}
	// A second run refuses: the app is a forge project now.
	if err := adoptApp(t, root, "todo", false, false); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("second adoption = %v, want a refusal naming the existing forge.yaml", err)
	}
}

func TestNewApp_Refusals(t *testing.T) {
	root := nodeExpressApp(t)
	if err := runNewApp(newAppInput{projectName: "todo", projectPath: root}); err == nil || !strings.Contains(err.Error(), "--in-place") {
		t.Errorf("without --in-place: %v, want a refusal naming --in-place", err)
	}
	empty := writeTree(t, map[string]string{"main.py": "print('hi')\n"})
	if err := adoptApp(t, empty, "x", false, false); err == nil || !strings.Contains(err.Error(), "no Dockerfile") {
		t.Errorf("no Dockerfile: %v, want a refusal naming the Dockerfile", err)
	}
	if entries, _ := os.ReadDir(empty); len(entries) != 1 {
		t.Errorf("a refusal wrote files: %d entries", len(entries))
	}
}

// The bare --in-place refusal over a Dockerfile app names --kind app.
func TestProjectNewInPlaceOverADockerfileAppPointsAtKindApp(t *testing.T) {
	root := nodeExpressApp(t)
	err := runNew(t.Context(), "todo", root, "", "", nil, nil, "", true, false, nil, "reliant", true, "local", "per-service", false, false, false, false, newFrontendOptions{})
	if err == nil || !strings.Contains(err.Error(), "--kind app") {
		t.Fatalf("want a refusal naming --kind app, got %v", err)
	}
}

func TestAppNextStepsArePasteable(t *testing.T) {
	det, err := detectApp(pythonComposeApp(t), "notes", false)
	if err != nil {
		t.Fatal(err)
	}
	lines := appNextSteps(det, false)
	want := map[string]bool{"env render": false, "env up": false, "env deploy": false, "secret set": false, "obs check": false, "skill load": false, "project new": false}
	for _, line := range lines {
		for _, args := range pasteableInvocations(line, Name()) {
			if path := resolvePasteable(t, args); path != "" {
				if _, ok := want[path]; ok {
					want[path] = true
				}
			}
		}
	}
	for cmd, seen := range want {
		if !seen {
			t.Errorf("app next steps never name `%s`:\n%s", cmd, strings.Join(lines, "\n"))
		}
	}
}

// The polyglot path, end to end: an Express app and a FastAPI app with a
// compose file and Postgres are adopted by `--kind app`, and every env
// renders and passes `forge env new --check`: dev on the host / through
// compose, staging and prod on hosting with the managed database, the
// migration job and the secret. The claimable variant fits the claimable
// plan.
func TestNewApp_PolyglotAppsAdoptAndRender(t *testing.T) {
	testkit.Slow(t, "renders six envs through native KCL")

	node := nodeExpressApp(t)
	if err := adoptApp(t, node, "todo", false, false); err != nil {
		t.Fatal(err)
	}
	dev, err := RenderKCL(t.Context(), node, "dev")
	if err != nil {
		t.Fatalf("render node dev: %v", err)
	}
	api := dev.FindWorkload("api")
	if api == nil || !api.OnRuntime(RuntimeHost) || strings.Join(api.Spec.Command, " ") != "npm run dev" || api.HostEnv()["PORT"] == "" {
		t.Fatalf("node dev api = %+v, want npm run dev on the host with PORT", api)
	}
	for _, env := range []string{"staging", "prod"} {
		e, err := RenderKCL(t.Context(), node, env)
		if err != nil {
			t.Fatalf("render node %s: %v", env, err)
		}
		api := e.FindWorkload("api")
		if api == nil || !api.OnRuntime(RuntimeHosted) || api.Build.Type != "docker" || api.Spec.Probes == nil || !api.Spec.Probes.TCP {
			t.Fatalf("node %s api = %+v, want the hosted DockerBuild workload with a tcp probe", env, api)
		}
	}

	py := pythonComposeApp(t)
	if err := adoptApp(t, py, "notes", false, false); err != nil {
		t.Fatal(err)
	}
	dev, err = RenderKCL(t.Context(), py, "dev")
	if err != nil {
		t.Fatalf("render python dev: %v", err)
	}
	for _, name := range []string{"web", "db", "redis", "adminer"} {
		if w := dev.FindWorkload(name); w == nil || !w.OnRuntime(RuntimeCompose) {
			t.Errorf("python dev %s = %+v, want the compose service", name, w)
		}
	}
	for _, env := range []string{"staging", "prod"} {
		e, err := RenderKCL(t.Context(), py, env)
		if err != nil {
			t.Fatalf("render python %s: %v", env, err)
		}
		web := e.FindWorkload("web")
		if web == nil || !web.OnRuntime(RuntimeHosted) {
			t.Fatalf("python %s web = %+v, want hosted", env, web)
		}
		var dbRef, secret bool
		for _, v := range web.Spec.Env {
			dbRef = dbRef || (v.Name == "DATABASE_URL" && v.DatabaseRef != nil && v.DatabaseRef.Name == "notes")
		}
		secret = hasManagedSecret(web, "SESSION_SECRET")
		if !dbRef || !secret {
			t.Errorf("python %s web env %+v, want DATABASE_URL from the managed database and SESSION_SECRET from the store", env, web.Spec.Env)
		}
		if m := e.FindWorkload("migrate"); m == nil || m.Kind != "job" || len(m.Spec.Before) == 0 || strings.Join(m.Spec.Command, " ") != "alembic upgrade head" {
			t.Errorf("python %s migrate = %+v, want the pre-rollout alembic job", env, m)
		}
		if r := e.FindWorkload("redis"); r == nil || !r.OnRuntime(RuntimeHosted) || r.Build.Type != "" && r.Build.Type != "none" {
			t.Errorf("python %s redis = %+v, want the hosted third-party image", env, r)
		}
		if e.FindWorkload("db") != nil || e.FindWorkload("adminer") != nil {
			t.Errorf("python %s hosts a dev-only compose service", env)
		}
		if len(e.Databases) != 1 || e.Databases[0].Name != "notes" {
			t.Errorf("python %s databases = %+v, want notes", env, e.Databases)
		}
	}
	for _, root := range []string{node, py} {
		for _, env := range []string{"dev", "staging", "prod"} {
			if err := checkEnv(t.Context(), root, env, filepath.Join(root, "deploy", "kcl", env)); err != nil {
				t.Errorf("forge env new %s --check in %s: %v", env, filepath.Base(root), err)
			}
		}
	}

	// --claimable: prod fits the anonymous plan, with no secret values.
	claim := writeTree(t, map[string]string{
		"Dockerfile":       "FROM python:3.12-slim\nCOPY . .\nCMD [\"uvicorn\", \"app.main:app\", \"--port\", \"8000\"]\n",
		"requirements.txt": "fastapi\npsycopg2-binary\n",
		"manage.py":        "",
	})
	if err := adoptApp(t, claim, "claimed", true, false); err != nil {
		t.Fatal(err)
	}
	prod, err := RenderKCL(t.Context(), claim, "prod")
	if err != nil {
		t.Fatalf("render claimable prod: %v", err)
	}
	if !declaresClaimable(prod) {
		t.Fatal("claimable prod does not declare claimable = True")
	}
	if reasons := claimableIneligibility(prod, deploy.ClaimableLimitsFallback()); len(reasons) > 0 {
		t.Errorf("claimable prod does not fit the plan: %v", reasons)
	}
}
