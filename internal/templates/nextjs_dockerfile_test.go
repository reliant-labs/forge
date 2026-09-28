package templates

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func renderNextjsDockerfile(t *testing.T, data FrontendTemplateData) []byte {
	t.Helper()
	out, err := FrontendTemplates().Render("nextjs/Dockerfile.tmpl", data)
	if err != nil {
		t.Fatalf("render nextjs/Dockerfile.tmpl: %v", err)
	}
	return out
}

// dockerInstructions returns the Dockerfile's instruction lines — comments
// and blank lines dropped — so an assertion about what the image DOES cannot
// be satisfied by a comment that merely mentions it.
func dockerInstructions(body []byte) []string {
	var out []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		out = append(out, trimmed)
	}
	return out
}

func hasInstruction(lines []string, substr string) bool {
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// TestNextjsDockerfileFollowsOutputMode is the reproduction for "the Next.js
// Dockerfile ignores the frontend's `output` mode".
//
// The template used to be one fixed file that copied .next-prod/standalone —
// the artifact ONLY `output: standalone` produces. A static frontend's build
// writes out/, and a server frontend's writes a full .next-prod for
// `next start`, so both images failed at
//
//	COPY --from=builder /app/.next-prod/standalone ./: not found
//
// and CI's docker-build job builds an image for every frontend, so a static
// frontend's CI could never go green. Each mode must copy what its build
// actually writes.
func TestNextjsDockerfileFollowsOutputMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		output   string
		want     []string // instruction substrings the image needs
		mustNot  []string // instruction substrings that break this mode
		exposure string
	}{
		{
			output: "standalone",
			want: []string{
				"FROM node:22-alpine AS runner",
				"COPY --from=builder --chown=node:node /src/frontends/web/.next-prod/standalone ./",
				`CMD ["node", "server.js"]`,
			},
			mustNot:  []string{"/out", "nginx", "next start"},
			exposure: "EXPOSE 3000",
		},
		{
			output: "static",
			want: []string{
				"FROM nginxinc/nginx-unprivileged:1.30-alpine AS runner",
				"COPY --from=builder /src/frontends/web/out /usr/share/nginx/html",
			},
			mustNot:  []string{".next-prod/standalone", "server.js", "USER node"},
			exposure: "EXPOSE 8080",
		},
		{
			output: "server",
			want: []string{
				"COPY --from=builder --chown=node:node /src/frontends/web/.next-prod ./.next-prod",
				"COPY --from=builder --chown=node:node /src/frontends/web/node_modules ./node_modules",
				`CMD ["node", "node_modules/next/dist/bin/next", "start"]`,
			},
			mustNot:  []string{".next-prod/standalone", "server.js", "nginx"},
			exposure: "EXPOSE 3000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.output, func(t *testing.T) {
			t.Parallel()
			lines := dockerInstructions(renderNextjsDockerfile(t, FrontendTemplateData{FrontendName: "web", Output: tt.output}))
			for _, w := range append(tt.want, tt.exposure) {
				if !hasInstruction(lines, w) {
					t.Errorf("output=%s: no instruction containing %q\n%s", tt.output, w, strings.Join(lines, "\n"))
				}
			}
			for _, bad := range tt.mustNot {
				if hasInstruction(lines, bad) {
					t.Errorf("output=%s: an instruction references %q, which this mode's build does not produce "+
						"or its runner does not use\n%s", tt.output, bad, strings.Join(lines, "\n"))
				}
			}
		})
	}
}

// The build runs at the frontend's REPOSITORY path, not /app. From /app the
// committed tsconfig pin "../../node_modules/<pkg>" named /node_modules, which
// webpack re-rooted onto /app/node_modules: a pin that resolved in the image
// and nowhere else, which is what made a bundle split surface only in Docker.
// And the install is `npm ci` — lockfile-exact — never `npm install`.
func TestNextjsDockerfileBuildsAtTheRepoPath(t *testing.T) {
	t.Parallel()

	for _, output := range []string{"standalone", "static", "server"} {
		lines := dockerInstructions(renderNextjsDockerfile(t, FrontendTemplateData{
			FrontendName: "console", RepoPath: "apps/console", Output: output,
		}))
		if !hasInstruction(lines, "WORKDIR /src/apps/console") {
			t.Errorf("output=%s: build does not run at the frontend's repo path /src/apps/console:\n%s",
				output, strings.Join(lines, "\n"))
		}
		if hasInstruction(lines, "WORKDIR /app") && !hasInstruction(lines, "AS runner") {
			t.Errorf("output=%s: builder still uses WORKDIR /app", output)
		}
		if !hasInstruction(lines, "npm ci") || hasInstruction(lines, "npm install") {
			t.Errorf("output=%s: the image must install with `npm ci`, never `npm install`:\n%s",
				output, strings.Join(lines, "\n"))
		}
	}

	// A payload without RepoPath defaults to where the scaffolder writes.
	lines := dockerInstructions(renderNextjsDockerfile(t, FrontendTemplateData{FrontendName: "web", Output: "standalone"}))
	if !hasInstruction(lines, "WORKDIR /src/frontends/web") {
		t.Errorf("RepoPath did not default to frontends/<name>:\n%s", strings.Join(lines, "\n"))
	}
}

// A static export built with a base_path emits every URL under it, so the
// files must be served from there — the same mount the StaticSite deploy uses.
func TestNextjsDockerfileStaticMountsUnderBasePath(t *testing.T) {
	t.Parallel()

	lines := dockerInstructions(renderNextjsDockerfile(t, FrontendTemplateData{
		FrontendName: "web", Output: "static", BasePath: "/admin",
	}))
	for _, want := range []string{
		"COPY --from=builder /src/frontends/web/out /usr/share/nginx/html/admin",
		"'    error_page 404 /admin/404.html;'",
		"http://127.0.0.1:8080/admin/",
	} {
		if !hasInstruction(lines, want) {
			t.Errorf("static+base_path image lacks %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}
}

// The node runners execute `node` and nothing else, and every HIGH CVE a scan
// of node:22-alpine reports sits in the npm/corepack trees the base image
// bundles. CI scans every frontend image with Trivy at HIGH,CRITICAL, so a
// runner that keeps them fails that gate for code it never runs.
func TestNextjsDockerfileNodeRunnerDropsNpm(t *testing.T) {
	t.Parallel()

	for _, output := range []string{"standalone", "server"} {
		body := string(renderNextjsDockerfile(t, FrontendTemplateData{FrontendName: "web", Output: output}))
		runner := body[strings.Index(body, "AS runner"):]
		if !strings.Contains(runner, "rm -rf /usr/local/lib/node_modules/npm") {
			t.Errorf("output=%s: the node runner keeps npm and its vulnerable dependency tree", output)
		}
	}
}

// The frontend's image builds with the frontend directory as its context, and
// a context reads only its OWN .dockerignore — so the scaffold must ship one
// beside the Dockerfile, or a host node_modules (native binaries for the
// developer's OS) is copied over the image's `npm ci`.
func TestNextjsScaffoldShipsDockerignore(t *testing.T) {
	t.Parallel()

	body, err := FrontendTemplates().Render("nextjs/.dockerignore", FrontendTemplateData{FrontendName: "web"})
	if err != nil {
		t.Fatalf("nextjs/.dockerignore is not in the template tree: %v", err)
	}
	files, err := ListFrontendTree("nextjs")
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, f := range files {
		if f.Rel == ".dockerignore" {
			listed = true
		}
	}
	if !listed {
		t.Error(".dockerignore is not part of the rendered nextjs tree, so the scaffold never writes it")
	}
	for _, want := range []string{"node_modules/", ".next-prod/", "out/", "public/config.js"} {
		if !strings.Contains(string(body), "\n"+want+"\n") {
			t.Errorf(".dockerignore does not exclude %q", want)
		}
	}
}

// TestNextjsDockerfileStaticImageBuilds is the real thing: a static export
// served by the rendered Dockerfile, built by docker with the frontend
// directory as its context — exactly CI's `Build <frontend> image` step.
//
// The frontend here is a minimal Next app rather than a full scaffold: what is
// under test is the Dockerfile's contract with an `output: export` build (npm
// ci, `npm run build` writing out/, the nginx runner serving it), and a
// minimal app exercises all of it in a fraction of the time. Before the fix
// this build failed at `COPY … .next-prod/standalone: not found`.
//
// Needs docker and network; skipped under -short and when no daemon answers.
func TestNextjsDockerfileStaticImageBuilds(t *testing.T) {
	if testing.Short() {
		t.Skip("real docker build (~1-2 min); skipped in -short")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if out, err := exec.Command("docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		t.Skipf("no docker daemon reachable: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not on PATH (needed to write the lockfile `npm ci` requires)")
	}
	t.Parallel()

	feDir := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(feDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dockerfile := renderNextjsDockerfile(t, FrontendTemplateData{FrontendName: "web", Output: "static"})
	dockerignore, err := FrontendTemplates().Render("nextjs/.dockerignore", FrontendTemplateData{FrontendName: "web"})
	if err != nil {
		t.Fatal(err)
	}
	write("Dockerfile", string(dockerfile))
	write(".dockerignore", string(dockerignore))
	write("package.json", `{"name":"web","private":true,
  "scripts":{"build":"NODE_ENV=production next build --webpack"},
  "dependencies":{"next":"^16.3.5","react":"^19.1.0","react-dom":"^19.1.0"}}
`)
	// The same production gate the scaffold's static next.config.ts uses.
	write("next.config.mjs", "export default process.env.NODE_ENV === \"production\" ? { output: \"export\" } : {};\n")
	write("app/layout.jsx", "export default function RootLayout({ children }) {\n  return <html lang=\"en\"><body>{children}</body></html>;\n}\n")
	write("app/page.jsx", "export default function Page() {\n  return <p>static image ok</p>;\n}\n")
	write("app/about/page.jsx", "export default function About() {\n  return <p>about page</p>;\n}\n")
	write("public/robots.txt", "User-agent: *\n")
	// A host node_modules the .dockerignore must keep out of the context.
	write("node_modules/.poison", "a host install must not reach the image\n")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	lock := exec.CommandContext(ctx, "npm", "install", "--package-lock-only", "--no-audit", "--no-fund")
	lock.Dir = feDir
	if out, err := lock.CombinedOutput(); err != nil {
		t.Fatalf("npm install --package-lock-only: %v\n%s", err, out)
	}

	tag := "forge-test-nextjs-static:" + strings.ToLower(strings.ReplaceAll(filepath.Base(feDir), "_", "-"))
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	buildCtx, buildCancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "docker", "build", "-t", tag, "-f", filepath.Join(feDir, "Dockerfile"), feDir)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build of a static Next.js frontend failed: %v\n%s", err, tailLines(string(out), 60))
	}

	// The image serves the export: / and /about (an exported about.html
	// reached through try_files), as the unprivileged nginx user.
	name := "forge-test-nextjs-static-" + strings.ToLower(strings.ReplaceAll(filepath.Base(feDir), "_", "-"))
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name, "-p", "127.0.0.1::8080", tag)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	port, err := exec.Command("docker", "port", name, "8080/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	addr := strings.TrimSpace(strings.Split(string(port), "\n")[0])
	for _, probe := range []struct{ path, want string }{{"/", "static image ok"}, {"/about", "about page"}} {
		body := fetchWithRetry(t, "http://"+addr+probe.path)
		if !strings.Contains(body, probe.want) {
			t.Errorf("GET %s did not serve the export (want %q):\n%s", probe.path, probe.want, tailLines(body, 5))
		}
	}
	if uid, err := exec.Command("docker", "exec", name, "id", "-u").Output(); err != nil || strings.TrimSpace(string(uid)) == "0" {
		t.Errorf("the static runner runs as root (uid=%q, err=%v)", strings.TrimSpace(string(uid)), err)
	}
	if out, err := exec.Command("docker", "exec", name, "ls", "/usr/share/nginx/html/robots.txt").CombinedOutput(); err != nil {
		t.Errorf("public/ assets are missing from the export: %v\n%s", err, out)
	}
}

func fetchWithRetry(t *testing.T, url string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(url) //nolint:gosec,noctx // a test fetching its own container on loopback
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				return string(body)
			}
			err = readErr
			if err == nil {
				err = &statusError{code: resp.StatusCode}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never answered 200: %v", url, err)
		}
		time.Sleep(time.Second)
	}
}

type statusError struct{ code int }

func (e *statusError) Error() string { return http.StatusText(e.code) }

func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
