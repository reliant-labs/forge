package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/sitecrawl/browser"
	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// siteAuditTestdata is a testdata site: good (scores 100, no finding) or bad
// (error findings).
func siteAuditTestdata(name string) string {
	return filepath.Join("testdata", "siteaudit", name)
}

func siteAuditExit(err error) int {
	if err == nil {
		return 0
	}
	var ec exitCodeError
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

func TestSiteAuditGoodBuildExitsZero(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runSiteAudit(context.Background(), &out, siteAuditTestdata("good"), siteAuditOptions{maxPages: 100})
	if err != nil {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
	for _, want := range []string{
		"as https://acme.example/ (from the home page's canonical",
		"Lighthouse-compatible scoring: SEO 100",
		"/about  200     100",
		"Findings: 0 error, 0 warn, 0 info",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSiteAuditJSONIsTheSiteauditReport(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	if err := runSiteAudit(context.Background(), &out, siteAuditTestdata("good"), siteAuditOptions{jsonOut: true, maxPages: 100}); err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	for _, key := range []string{"rules_version", "site", "routes", "findings"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("the v1 document lacks %q: %s", key, out.String())
		}
	}
	var rep siteaudit.Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.RulesVersion != siteaudit.RulesVersion || len(rep.Routes) != 2 || len(rep.Findings) != 0 {
		t.Errorf("report = version %q, %d routes, %d findings; want %q, 2, 0", rep.RulesVersion, len(rep.Routes), len(rep.Findings), siteaudit.RulesVersion)
	}
	if rep.Site.Robots.Status != "ok" || rep.Site.Sitemap.URLs != 2 || rep.Site.NotFound != "not_found" {
		t.Errorf("site summary = %+v; want robots ok, 2 sitemap URLs, a real 404", rep.Site)
	}
}

func TestSiteAuditErrorFindingsExitOne(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runSiteAudit(context.Background(), &out, siteAuditTestdata("bad"), siteAuditOptions{siteURL: "https://bad.example", maxPages: 100})
	if got := siteAuditExit(err); got != exitWrong {
		t.Fatalf("exit = %d (%v), want %d", got, err, exitWrong)
	}
	var ec exitCodeError
	if errors.As(err, &ec) && !ec.IsVerdict() {
		t.Error("error findings are a verdict, not a failure to run")
	}
	for _, want := range []string{
		"error title_missing  /  title",
		"error canonical_foreign_host  /  canonical",
		"error indexing_mismatch  /  meta_robots",
		"fix: Give the page a <title> that names it.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

func TestSiteAuditCannotRunExitsTwo(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"missing directory": filepath.Join(t.TempDir(), "absent"),
		"loopback URL":      "http://127.0.0.1:5173/",
		"localhost URL":     "http://localhost:3000",
		"private URL":       "https://10.0.0.8/",
	}
	for name, target := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runSiteAudit(context.Background(), &out, target, siteAuditOptions{maxPages: 10})
			if got := siteAuditExit(err); got != exitUndetermined {
				t.Fatalf("exit = %d (%v), want %d", got, err, exitUndetermined)
			}
		})
	}
}

func TestSiteAuditBrowserWithoutChromeNamesTheFix(t *testing.T) {
	t.Setenv(browser.ChromePathEnv, filepath.Join(t.TempDir(), "no-chrome"))
	var out bytes.Buffer
	err := runSiteAudit(context.Background(), &out, siteAuditTestdata("good"), siteAuditOptions{browser: true, maxPages: 10})
	if got := siteAuditExit(err); got != exitUndetermined {
		t.Fatalf("exit = %d (%v), want %d", got, err, exitUndetermined)
	}
	if !strings.Contains(err.Error(), browser.ChromePathEnv) {
		t.Errorf("error %q does not name %s", err, browser.ChromePathEnv)
	}
}

func TestSiteAuditDirURL(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<html><head><meta property="og:url" content="https://og.example/x"></head></html>`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, dir, base, flag, env string
		want, from                 string
	}{
		{"flag wins", dir, "", "https://flag.example", "https://env.example", "https://flag.example/", "--site-url"},
		{"env SITE_URL next", dir, "/docs", "", "https://env.example/", "https://env.example/docs/", "from the env's SITE_URL"},
		{"og:url when no canonical", dir, "", "", "", "https://og.example/", "from the home page's og:url"},
		{"localhost when nothing says", t.TempDir(), "", "", "", "http://localhost/", "the home page names no canonical host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, from, err := siteAuditDirURL(tc.dir, tc.base, tc.flag, tc.env)
			if err != nil {
				t.Fatal(err)
			}
			if u.String() != tc.want || from != tc.from {
				t.Errorf("= %s (from %s), want %s (from %s)", u, from, tc.want, tc.from)
			}
		})
	}
	if _, _, err := siteAuditDirURL(dir, "", "not a url", ""); siteAuditExit(err) != exitUndetermined {
		t.Errorf("a bad --site-url: err = %v, want exit 2", err)
	}
}

func TestSiteAuditIsInTheCommandTree(t *testing.T) {
	t.Parallel()
	spec := buildCapabilitiesSpec()
	found := map[string]bool{}
	for _, c := range spec.Commands {
		found[c.Path] = true
	}
	for _, want := range []string{"forge site", "forge site audit"} {
		if !found[want] {
			t.Errorf("forge project capabilities does not list %q", want)
		}
	}
}
