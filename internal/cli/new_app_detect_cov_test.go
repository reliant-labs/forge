package cli

import (
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/generator"
)

func covDYAMLNode(t *testing.T, src string) yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Content) == 0 {
		t.Fatalf("no content in %q", src)
	}
	return *doc.Content[0]
}

func TestCovDCleanRel(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "."},
		{"./a/../b", "b"},
		{"api/Dockerfile", "api/Dockerfile"},
	} {
		if got := cleanRel(c.in); got != c.want {
			t.Errorf("cleanRel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCovDComposePorts_ReadsTheContainerSide(t *testing.T) {
	n := covDYAMLNode(t, `
- "3000"
- "8080:3000"
- "127.0.0.1:8081:3001/tcp"
- target: 3002
  published: 9000
- "notaport"
- "70000"
- "0"
`)
	got := composePorts(n)
	want := []int{3000, 3000, 3001, 3002}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ports = %v, want %v", got, want)
	}
}

func TestCovDComposeEnvironment_MapAndList(t *testing.T) {
	m, order := composeEnvironment(covDYAMLNode(t, "A: one\nB: two\n"))
	if m["A"] != "one" || m["B"] != "two" || strings.Join(order, ",") != "A,B" {
		t.Errorf("map form = %v %v", m, order)
	}
	l, order := composeEnvironment(covDYAMLNode(t, "- X=1\n- PASSTHROUGH\n- Y=a=b\n"))
	if l["X"] != "1" || l["PASSTHROUGH"] != "${PASSTHROUGH}" || l["Y"] != "a=b" {
		t.Errorf("list form = %v", l)
	}
	if strings.Join(order, ",") != "X,PASSTHROUGH,Y" {
		t.Errorf("order = %v", order)
	}
	empty, order := composeEnvironment(covDYAMLNode(t, "just a scalar"))
	if len(empty) != 0 || len(order) != 0 {
		t.Errorf("scalar form must read as none, got %v %v", empty, order)
	}
}

func TestCovDComposeDependsOn_ListAndMap(t *testing.T) {
	if got := composeDependsOn(covDYAMLNode(t, "- db\n- cache\n")); !reflect.DeepEqual(got, []string{"db", "cache"}) {
		t.Errorf("list form = %v", got)
	}
	if got := composeDependsOn(covDYAMLNode(t, "db:\n  condition: service_healthy\ncache:\n  condition: service_started\n")); !reflect.DeepEqual(got, []string{"db", "cache"}) {
		t.Errorf("map form = %v", got)
	}
	if got := composeDependsOn(covDYAMLNode(t, "scalar")); got != nil {
		t.Errorf("scalar form = %v, want none", got)
	}
}

func TestCovDComposeCommand_ScalarAndSequence(t *testing.T) {
	argv, raw := composeCommand(covDYAMLNode(t, `"uvicorn app:app --port 8000"`))
	if !reflect.DeepEqual(argv, []string{"uvicorn", "app:app", "--port", "8000"}) || raw != "uvicorn app:app --port 8000" {
		t.Errorf("scalar = %v %q", argv, raw)
	}
	argv, raw = composeCommand(covDYAMLNode(t, "- node\n- server.js\n"))
	if !reflect.DeepEqual(argv, []string{"node", "server.js"}) || raw != "node server.js" {
		t.Errorf("sequence = %v %q", argv, raw)
	}
}

func TestCovDSplitComposeCommandEdgeCases(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`a "b c" 'd e' f\ g`, []string{"a", "b c", "d e", "f g"}},
		{"a\tb\nc", []string{"a", "b", "c"}},
		{`unterminated "quote here`, []string{"unterminated", `"quote`, "here"}},
		{`trailing\`, []string{`trailing\`}},
		{"", nil},
	}
	for _, c := range cases {
		if got := splitComposeCommand(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitComposeCommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCovDImageRepoAndNormalizeImage(t *testing.T) {
	for _, c := range []struct{ in, repo, norm string }{
		{"postgres:16", "postgres", "docker.io/library/postgres:16"},
		{"docker.io/library/postgres:16", "postgres", "docker.io/library/postgres:16"},
		{"Bitnami/Redis@sha256:abc", "bitnami/redis", "docker.io/Bitnami/Redis@sha256:abc"},
		{"ghcr.io/acme/api:1.0", "acme/api", "ghcr.io/acme/api:1.0"},
		{"localhost:5000/team/img:tag", "team/img", "localhost:5000/team/img:tag"},
		{"localhost/img", "img", "localhost/img"},
		{"redis", "redis", "docker.io/library/redis"},
	} {
		if got := imageRepo(c.in); got != c.repo {
			t.Errorf("imageRepo(%q) = %q, want %q", c.in, got, c.repo)
		}
		if got := normalizeImage(c.in); got != c.norm {
			t.Errorf("normalizeImage(%q) = %q, want %q", c.in, got, c.norm)
		}
	}
}

func TestCovDIsPostgresImage(t *testing.T) {
	for in, want := range map[string]bool{
		"postgres:16":                      true,
		"postgis/postgis:15":               true,
		"bitnami/postgresql":               true,
		"acme/postgres":                    true,
		"registry.local/acme/postgresql:1": true,
		"redis:7":                          false,
		"mysql:8":                          false,
	} {
		if got := isPostgresImage(in); got != want {
			t.Errorf("isPostgresImage(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestCovDExpandDockerVars(t *testing.T) {
	vars := map[string]string{"PORT": "8080", "EMPTY": ""}
	for _, c := range []struct{ in, want string }{
		{"$PORT", "8080"},
		{"${PORT}", "8080"},
		{"${MISSING:-3000}", "3000"},
		{"${EMPTY:-9}", "9"},
		{"$NOPE", ""},
		{"plain", "plain"},
	} {
		if got := expandDockerVars(c.in, vars); got != c.want {
			t.Errorf("expandDockerVars(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCovDShellFields(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{`a=1 b="x y" c='z w'`, []string{"a=1", "b=x y", "c=z w"}},
		{`  spaced   out  `, []string{"spaced", "out"}},
		{`""`, []string{""}},
		{"", nil},
	} {
		if got := shellFields(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("shellFields(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCovDDockerCommand_ExecAndShellForms(t *testing.T) {
	argv, raw := dockerCommand(`["node", "server.js"]`)
	if !reflect.DeepEqual(argv, []string{"node", "server.js"}) || raw != "node server.js" {
		t.Errorf("exec form = %v %q", argv, raw)
	}
	argv, raw = dockerCommand("node server.js --port 80")
	if !reflect.DeepEqual(argv, []string{"sh", "-c", "node server.js --port 80"}) || raw != "node server.js --port 80" {
		t.Errorf("shell form = %v %q", argv, raw)
	}
	argv, _ = dockerCommand("[not json")
	if len(argv) != 3 || argv[0] != "sh" {
		t.Errorf("malformed exec form must fall back to the shell form, got %v", argv)
	}
}

func TestCovDDockerKV_ModernAndLegacy(t *testing.T) {
	if got := dockerKV(`A=1 B="two words"`); got["A"] != "1" || got["B"] != "two words" {
		t.Errorf("modern form = %v", got)
	}
	if got := dockerKV(`PORT "8080"`); got["PORT"] != "8080" {
		t.Errorf("legacy form = %v", got)
	}
}

func TestCovDPortInCommand(t *testing.T) {
	for _, c := range []struct {
		cmd  string
		want int
	}{
		{"uvicorn app:app --port 8000", 8000},
		{"uvicorn app:app --port=8001", 8001},
		{"gunicorn -b 0.0.0.0:8002 app", 8002},
		{"flask run -p 5005", 5005},
		{"serve localhost:4000", 4000},
		{"node server.js", 0},
		{"app --port 99999", 0},
	} {
		if got := portInCommand(c.cmd); got != c.want {
			t.Errorf("portInCommand(%q) = %d, want %d", c.cmd, got, c.want)
		}
	}
}

func TestCovDPortToEnv(t *testing.T) {
	if got := portToEnv("serve --port 3000", 3000); got != "serve --port $PORT" {
		t.Errorf("got %q", got)
	}
	if got := portToEnv("serve 13000", 3000); got != "serve 13000" {
		t.Errorf("a port that is only a suffix of another number must not be rewritten: %q", got)
	}
	if got := portToEnv("serve --port 3000", 0); got != "serve --port 3000" {
		t.Errorf("no port means no rewrite: %q", got)
	}
}

func TestCovDWorkloadNameAndIdent(t *testing.T) {
	for in, want := range map[string]string{
		"API_Server":   "api-server",
		"9lives":       "app-9lives",
		"":             "app",
		"--x--":        "x",
		"web.frontend": "web-frontend",
	} {
		if got := workloadName(in); got != want {
			t.Errorf("workloadName(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"my-svc": "my_svc",
		"forge":  "forge_app",
		"map":    "map_app",
		"worker": "worker",
	} {
		if got := workloadIdent(in); got != want {
			t.Errorf("workloadIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCovDIsDBHostVar(t *testing.T) {
	for _, k := range []string{"DB_HOST", "database_host", "PGHOST", "Postgres_Host"} {
		if !isDBHostVar(k) {
			t.Errorf("%s is a database host variable", k)
		}
	}
	for _, k := range []string{"DB_PORT", "HOST", "REDIS_HOST"} {
		if isDBHostVar(k) {
			t.Errorf("%s is not a database host variable", k)
		}
	}
}

func TestCovDLooksLikeWorker(t *testing.T) {
	if !looksLikeWorker("emailer", "celery -A app worker") || !looksLikeWorker("queue-runner", "") {
		t.Error("celery and queue names are workers")
	}
	if looksLikeWorker("api", "node server.js") {
		t.Error("a plain api is not a worker")
	}
}

func TestCovDAppSource_DefaultPortFirstOfAndInstall(t *testing.T) {
	cases := []struct {
		src  appSource
		cmd  string
		port int
		why  string
	}{
		{appSource{}, "uvicorn app:app", 8000, "the usual port of uvicorn"},
		{appSource{}, "python manage.py runserver", 8000, "the usual port of manage.py"},
		{appSource{}, "flask run", 5000, "Flask's default port"},
		{appSource{lang: "node"}, "node x.js", 3000, "the usual node port"},
		{appSource{lang: "ruby"}, "rackup", 3000, "the usual ruby port"},
		{appSource{lang: "python"}, "python app.py", 8000, "the usual Python port"},
		{appSource{lang: "go"}, "./app", 8080, "a common default"},
	}
	for _, c := range cases {
		port, why := c.src.defaultPort(c.cmd)
		if port != c.port || why != c.why {
			t.Errorf("defaultPort(%q, lang %q) = %d, %q; want %d, %q", c.cmd, c.src.lang, port, why, c.port, c.why)
		}
	}
	if got := firstOf("gunicorn app", "uvicorn", "gunicorn"); got != "gunicorn" {
		t.Errorf("firstOf = %q", got)
	}
	if got := firstOf("nothing", "a", "b"); got != "a" {
		t.Errorf("firstOf with no match must name the first word, got %q", got)
	}

	for _, c := range []struct {
		src  appSource
		want string
	}{
		{appSource{lang: "node", pm: "npm"}, "npm ci"},
		{appSource{lang: "node", pm: "pnpm"}, "pnpm install"},
		{appSource{lang: "python", text: "[tool.poetry]"}, "poetry install"},
		{appSource{lang: "python", text: "[project]\nname='x'"}, "pip install -e ."},
		{appSource{lang: "python", text: "flask"}, "pip install -r requirements.txt"},
		{appSource{lang: "ruby"}, "bundle install"},
		{appSource{lang: "go"}, ""},
	} {
		if got := c.src.installCommand(); got != c.want {
			t.Errorf("installCommand(%+v) = %q, want %q", c.src, got, c.want)
		}
	}
}

func TestCovDAppSource_DevCommandPrecedence(t *testing.T) {
	npm := appSource{pm: "npm", scripts: map[string]string{"start": "node x"}}
	if cmd, why := npm.devCommand(nil, "", 3000); strings.Join(cmd, " ") != "npm start" || !strings.Contains(why, `"start"`) {
		t.Errorf("npm start = %v %q", cmd, why)
	}
	yarn := appSource{pm: "yarn", scripts: map[string]string{"start:dev": "x"}}
	if cmd, _ := yarn.devCommand(nil, "", 3000); strings.Join(cmd, " ") != "yarn run start:dev" {
		t.Errorf("yarn = %v", cmd)
	}
	npmDev := appSource{pm: "npm", scripts: map[string]string{"dev": "x"}}
	if cmd, _ := npmDev.devCommand(nil, "", 3000); strings.Join(cmd, " ") != "npm run dev" {
		t.Errorf("npm dev = %v", cmd)
	}
	proc := appSource{procfile: map[string]string{"web": "gunicorn -b :8000 app"}}
	if cmd, why := proc.devCommand([]string{"ignored"}, "ignored", 8000); cmd[2] != "gunicorn -b :$PORT app" || !strings.Contains(why, "Procfile") {
		t.Errorf("procfile = %v %q", cmd, why)
	}
	fb := appSource{}
	if cmd, why := fb.devCommand([]string{"sh", "-c", "serve 3000"}, "serve 3000", 3000); cmd[2] != "serve $PORT" || !strings.Contains(why, "Dockerfile's CMD") {
		t.Errorf("sh -c fallback = %v %q", cmd, why)
	}
	if cmd, _ := fb.devCommand([]string{"node", "s.js"}, "node s.js", 3000); cmd[2] != "node s.js" {
		t.Errorf("exec fallback = %v", cmd)
	}
	if cmd, why := fb.devCommand(nil, "", 0); !strings.HasPrefix(why, "no dev command") || !strings.Contains(cmd[2], "set the dev command") {
		t.Errorf("no command = %v %q", cmd, why)
	}
}

func TestCovDReadAppSource_DetectsLanguageAndPackageManager(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		lang  string
		pm    string
	}{
		{"pnpm node", map[string]string{"package.json": `{"dependencies":{"a":"1"},"devDependencies":{"b":"1"}}`, "pnpm-lock.yaml": ""}, "node", "pnpm"},
		{"yarn node", map[string]string{"package.json": `{}`, "yarn.lock": ""}, "node", "yarn"},
		{"bun node", map[string]string{"package.json": `{}`, "bun.lockb": ""}, "node", "bun"},
		{"bun text lock", map[string]string{"package.json": `{}`, "bun.lock": ""}, "node", "bun"},
		{"python", map[string]string{"requirements.txt": "Flask\n"}, "python", ""},
		{"ruby", map[string]string{"Gemfile": "gem 'pg'\n"}, "ruby", ""},
		{"go", map[string]string{"go.mod": "module x\n"}, "go", ""},
		{"java", map[string]string{"pom.xml": "<project/>"}, "java", ""},
		{"gradle kts", map[string]string{"build.gradle.kts": ""}, "java", ""},
		{"unknown", map[string]string{"README.md": "hi"}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeAppTree(t, c.files)
			s := readAppSource(root, "")
			if s.lang != c.lang || s.pm != c.pm {
				t.Errorf("lang/pm = %q/%q, want %q/%q", s.lang, s.pm, c.lang, c.pm)
			}
		})
	}

	root := writeAppTree(t, map[string]string{
		"svc/package.json": `{"scripts":{"dev":"x"},"dependencies":{"express":"4"},"devDependencies":{"jest":"1"}}`,
		"svc/Procfile":     "web: node server.js\nrelease : npm run migrate\nnot a process line\n",
	})
	s := readAppSource(root, "svc")
	if !s.has("express") || !s.has("jest") || s.has("react") || s.scripts["dev"] != "x" {
		t.Errorf("deps/scripts = %v %v", s.deps, s.scripts)
	}
	if s.procfile["web"] != "node server.js" || s.procfile["release"] != "npm run migrate" || len(s.procfile) != 2 {
		t.Errorf("procfile = %v", s.procfile)
	}
}

func TestCovDAppSource_UsesPostgresByStack(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  bool
		why   string
	}{
		{"node pg", map[string]string{"package.json": `{"dependencies":{"pg":"8"}}`}, true, "package.json depends on pg"},
		{"prisma postgres", map[string]string{"package.json": `{"dependencies":{"@prisma/client":"5"}}`, "prisma/schema.prisma": `datasource db { provider = "postgresql" }`}, true, "prisma/schema.prisma"},
		{"prisma sqlite", map[string]string{"package.json": `{"dependencies":{"prisma":"5"}}`, "prisma/schema.prisma": `datasource db { provider = "sqlite" }`}, false, ""},
		{"python psycopg", map[string]string{"requirements.txt": "psycopg2-binary==2.9\n"}, true, "Python dependencies"},
		{"ruby pg", map[string]string{"Gemfile": "gem 'pg'\n"}, true, "Gemfile names pg"},
		{"go pgx", map[string]string{"go.mod": "module x\nrequire github.com/jackc/pgx/v5 v5.0.0\n"}, true, "go.mod names a Postgres driver"},
		{"nothing", map[string]string{"package.json": `{"dependencies":{"express":"4"}}`}, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeAppTree(t, c.files)
			got, why := readAppSource(root, "").usesPostgres(root)
			if got != c.want || !strings.Contains(why, c.why) {
				t.Errorf("usesPostgres = %v, %q; want %v containing %q", got, why, c.want, c.why)
			}
		})
	}
}

func TestCovDAppSource_MigrationCommand(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		want   string
		source string
	}{
		{"npm script", map[string]string{"package.json": `{"scripts":{"migrate":"x"}}`}, "npm run migrate", `script "migrate"`},
		{"yarn script wins by order", map[string]string{"package.json": `{"scripts":{"db:migrate":"x","migrate:deploy":"y"}}`, "yarn.lock": ""}, "yarn run migrate:deploy", `"migrate:deploy"`},
		{"prisma", map[string]string{"package.json": `{"dependencies":{"prisma":"5"}}`}, "npx prisma migrate deploy", "prisma"},
		{"knex", map[string]string{"package.json": `{"dependencies":{"knex":"3"}}`}, "npx knex migrate:latest", "knex"},
		{"sequelize", map[string]string{"package.json": `{"dependencies":{"sequelize-cli":"6"}}`}, "npx sequelize-cli db:migrate", "sequelize-cli"},
		{"drizzle", map[string]string{"package.json": `{"devDependencies":{"drizzle-kit":"1"}}`}, "npx drizzle-kit migrate", "drizzle-kit"},
		{"alembic", map[string]string{"requirements.txt": "alembic\n", "alembic.ini": ""}, "alembic upgrade head", "alembic.ini"},
		{"django", map[string]string{"requirements.txt": "django\n", "manage.py": ""}, "python manage.py migrate", "Django"},
		{"rails", map[string]string{"Gemfile": "gem 'rails'", "bin/rails": "", "db/migrate/001_x.rb": ""}, "bin/rails db:migrate", "Rails"},
		{"rails without migrations", map[string]string{"Gemfile": "gem 'rails'", "bin/rails": ""}, "", ""},
		{"none", map[string]string{"package.json": `{}`}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeAppTree(t, c.files)
			cmd, source := readAppSource(root, "").migrationCommand(root)
			if got := strings.Join(cmd, " "); got != c.want || !strings.Contains(source, c.source) {
				t.Errorf("migrationCommand = %q (%q), want %q (%q)", got, source, c.want, c.source)
			}
		})
	}
}

func TestCovDHasAppToAdopt(t *testing.T) {
	if hasAppToAdopt(writeAppTree(t, map[string]string{"README.md": "x"})) {
		t.Error("an empty repo has nothing to adopt")
	}
	if !hasAppToAdopt(writeAppTree(t, map[string]string{"Dockerfile": "FROM scratch\n"})) {
		t.Error("a Dockerfile is adoptable")
	}
	if !hasAppToAdopt(writeAppTree(t, map[string]string{"api/Dockerfile": "FROM scratch\n"})) {
		t.Error("a top-level directory's Dockerfile is adoptable")
	}
	if hasAppToAdopt(writeAppTree(t, map[string]string{"node_modules/x/Dockerfile": "FROM scratch\n", ".hidden/Dockerfile": "FROM scratch\n"})) {
		t.Error("vendored and hidden directories are not scanned")
	}
	if !hasAppToAdopt(writeAppTree(t, map[string]string{"compose.yaml": "services:\n  web:\n    build: .\n"})) {
		t.Error("a compose service with a build is adoptable")
	}
	if hasAppToAdopt(writeAppTree(t, map[string]string{"compose.yaml": "services:\n  db:\n    image: postgres:16\n"})) {
		t.Error("a compose file that builds nothing is not an app")
	}
	if hasAppToAdopt(writeAppTree(t, map[string]string{"compose.yaml": "services: [unclosed"})) {
		t.Error("an unparseable compose file is not adoptable")
	}
}

func TestCovDAppDetection_DedupeNames(t *testing.T) {
	d := &appDetection{spec: generator.AppSpec{Workloads: []generator.AppWorkload{
		{Name: "api", Ident: "api", Image: "api", Build: &generator.AppBuild{}},
		{Name: "api", Ident: "api", Image: "api", Build: &generator.AppBuild{}},
		{Name: "api", Ident: "api", Image: "api"},
		{Name: "web", Ident: "web"},
	}}}
	d.dedupeNames()
	var names []string
	for _, w := range d.spec.Workloads {
		names = append(names, w.Name)
	}
	if strings.Join(names, ",") != "api,api-2,api-3,web" {
		t.Fatalf("names = %v", names)
	}
	if d.spec.Workloads[1].Image != "api-2" || d.spec.Workloads[1].Ident != "api_2" {
		t.Errorf("a renamed built workload must follow its image and ident: %+v", d.spec.Workloads[1])
	}
	if d.spec.Workloads[2].Image != "api" {
		t.Errorf("a workload with no build keeps its image: %+v", d.spec.Workloads[2])
	}
}

func TestCovDDetectApp_MonorepoDockerfilesBecomeWorkloads(t *testing.T) {
	root := writeAppTree(t, map[string]string{
		"Dockerfile":               "FROM node:20\nEXPOSE 8080/tcp\nCMD [\"node\", \"s.js\"]\n",
		"package.json":             `{"dependencies":{"pg":"8"}}`,
		"emailer/Dockerfile":       "FROM python:3.12\nCMD celery -A app worker\n",
		"emailer/requirements.txt": "celery\n",
		"web/Dockerfile":           "FROM node:20\nENV PORT=4000\nCMD node server.js\n",
		"cron/Dockerfile":          "FROM alpine\nCMD [\"sh\", \"-c\", \"serve --port 7000\"]\n",
		"docker-compose.yml":       "services:\n  db:\n    image: postgres:16\n",
	})
	det, err := detectApp(root, "mono", false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]generator.AppWorkload{}
	for _, w := range det.spec.Workloads {
		byName[w.Name] = w
	}
	if len(byName) != 4 {
		t.Fatalf("workloads = %+v", det.spec.Workloads)
	}
	if api := byName["api"]; api.Kind != "service" || api.Port != 8080 || !api.UsesDatabase || !api.Expose {
		t.Errorf("root workload = %+v", api)
	}
	if em := byName["emailer"]; em.Kind != "worker" || em.Expose {
		t.Errorf("a celery worker must not be exposed: %+v", em)
	}
	if web := byName["web"]; web.Port != 4000 || !strings.Contains(web.PortSource, "ENV PORT") {
		t.Errorf("web port = %d (%s)", web.Port, web.PortSource)
	}
	if cron := byName["cron"]; cron.Port != 7000 || !strings.Contains(cron.PortSource, "CMD") {
		t.Errorf("cron port = %d (%s)", cron.Port, cron.PortSource)
	}
	assumed := strings.Join(det.assumed, "\n")
	if !strings.Contains(assumed, "api uses Postgres (package.json depends on pg)") {
		t.Errorf("the app's own driver must be reported: %v", det.assumed)
	}
	if !strings.Contains(assumed, "emailer uses the Postgres of docker-compose.yml") {
		t.Errorf("a compose Postgres must be reported for a worker with no driver of its own: %v", det.assumed)
	}
	if !strings.Contains(assumed, "no migration command found") {
		t.Errorf("a missing migration command must be reported: %v", det.assumed)
	}
}

func TestCovDDetectApp_Refusals(t *testing.T) {
	if _, err := detectApp(writeAppTree(t, map[string]string{"README.md": "x"}), "p", false); err == nil || !strings.Contains(err.Error(), "no Dockerfile") {
		t.Errorf("an empty directory: got %v", err)
	}
	root := writeAppTree(t, map[string]string{"Dockerfile": "FROM node:20\nEXPOSE 3000\nCMD [\"node\", \"s.js\", \"--api-token=abcdef123456\"]\n"})
	if _, err := detectApp(root, "p", false); err == nil || !strings.Contains(err.Error(), "carries a credential") {
		t.Errorf("a credential in the CMD must be refused, got %v", err)
	}
	if _, err := detectApp(writeAppTree(t, map[string]string{"compose.yaml": "services: [unclosed"}), "p", false); err == nil {
		t.Error("an unparseable compose file must be refused")
	}
}
