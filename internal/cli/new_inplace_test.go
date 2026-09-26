package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestMergeGitignore(t *testing.T) {
	const forge = "# forge header\nbin/\ngo.work\n\n!gen/go.mod\n.env.local\n"
	for _, tc := range []struct {
		name      string
		user      string
		wantAdded int
		wantLines []string // must appear, in the forge block or the user part
	}{
		{name: "empty user file", user: "", wantAdded: 4, wantLines: []string{"bin/", "go.work", "!gen/go.mod", ".env.local"}},
		{name: "user already has some", user: "# mine\n.env.local\nbin/\n", wantAdded: 2, wantLines: []string{"# mine", "go.work", "!gen/go.mod"}},
		{name: "no trailing newline", user: "node_modules", wantAdded: 4, wantLines: []string{"node_modules", "bin/"}},
		{name: "user covers everything but negations", user: "bin/\ngo.work\n.env.local\n!gen/go.mod\n", wantAdded: 1, wantLines: []string{"!gen/go.mod"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			once, added := mergeGitignore(tc.user, forge)
			if added != tc.wantAdded {
				t.Errorf("added = %d, want %d\n%s", added, tc.wantAdded, once)
			}
			userPart := strings.TrimRight(tc.user, "\n")
			if !strings.HasPrefix(once, userPart) {
				t.Errorf("user content not preserved as a prefix:\n%s", once)
			}
			for _, l := range tc.wantLines {
				if !strings.Contains(once, l+"\n") {
					t.Errorf("merged file lacks %q:\n%s", l, once)
				}
			}
			if strings.Count(once, forgeGitignoreBegin) != 1 || strings.Count(once, forgeGitignoreEnd) != 1 {
				t.Errorf("want exactly one forge block:\n%s", once)
			}
			twice, _ := mergeGitignore(once, forge)
			if twice != once {
				t.Errorf("not idempotent.\nonce:\n%s\ntwice:\n%s", once, twice)
			}
		})
	}

	t.Run("nothing missing writes no block", func(t *testing.T) {
		user := "bin/\ngo.work\n.env.local\n"
		got, added := mergeGitignore(user, "bin/\ngo.work\n")
		if got != user || added != 0 {
			t.Errorf("got %q (added %d), want the user file untouched", got, added)
		}
	})
}

func TestMergeScaffoldInto(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		name := "keep"
		if overwrite {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			staging, target := t.TempDir(), t.TempDir()
			writeTestFile(t, filepath.Join(staging, "README.md"), "forge readme\n")
			writeTestFile(t, filepath.Join(staging, "go.mod"), "module forge\n")
			writeTestFile(t, filepath.Join(staging, "docs", "adr", "0001.md"), "forge adr\n")
			writeTestFile(t, filepath.Join(staging, ".gitignore"), "bin/\n")
			writeTestFile(t, filepath.Join(target, "README.md"), "user readme\n")
			writeTestFile(t, filepath.Join(target, "docs", "adr", "0001.md"), "user adr\n")
			writeTestFile(t, filepath.Join(target, ".gitignore"), ".env\n")

			res, err := mergeScaffoldInto(staging, target, overwrite)
			if err != nil {
				t.Fatal(err)
			}
			read := func(rel string) string {
				b, _ := os.ReadFile(filepath.Join(target, rel))
				return string(b)
			}
			if got := read("go.mod"); got != "module forge\n" {
				t.Errorf("new file go.mod = %q, want forge's", got)
			}
			if got := read(".gitignore"); !strings.HasPrefix(got, ".env\n") || !strings.Contains(got, "bin/\n") {
				t.Errorf(".gitignore not merged (force must not replace it either):\n%s", got)
			}
			collisions := []string{"README.md", "docs/adr/0001.md"}
			if overwrite {
				if !reflect.DeepEqual(res.overwritten, collisions) || len(res.kept) != 0 {
					t.Errorf("overwritten=%v kept=%v, want overwritten=%v", res.overwritten, res.kept, collisions)
				}
				if read("README.md") != "forge readme\n" {
					t.Errorf("--force did not replace README.md")
				}
				return
			}
			if !reflect.DeepEqual(res.kept, collisions) || len(res.overwritten) != 0 {
				t.Errorf("kept=%v overwritten=%v, want kept=%v", res.kept, res.overwritten, collisions)
			}
			if read("README.md") != "user readme\n" || read("docs/adr/0001.md") != "user adr\n" {
				t.Errorf("an existing file was overwritten without --force")
			}
			summary := strings.Join(inPlaceMergeSummary(res), "\n")
			if !strings.Contains(summary, "Kept your existing: README.md, docs/adr/0001.md") {
				t.Errorf("summary does not list the kept files:\n%s", summary)
			}
		})
	}
}

func TestDetectExistingFrontendApps(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "web", "package.json"), `{"dependencies":{"next":"16.0.0","react":"19"}}`)
	writeTestFile(t, filepath.Join(root, "frontend", "package.json"), `{"devDependencies":{"vite":"^6"}}`)
	writeTestFile(t, filepath.Join(root, "frontends", "admin", "package.json"), `{"dependencies":{"next":"15"}}`)
	writeTestFile(t, filepath.Join(root, "frontends", "lib", "package.json"), `{"dependencies":{"lodash":"4"}}`)

	apps := detectExistingFrontendApps(root)
	want := []existingFrontendApp{
		{name: "web", dir: "web", typ: "nextjs"},
		{name: "frontend", dir: "frontend", typ: "vite-spa"},
		{name: "admin", dir: "frontends/admin", typ: "nextjs"},
	}
	if !reflect.DeepEqual(apps, want) {
		t.Fatalf("detected %+v, want %+v", apps, want)
	}

	err := checkInPlaceFrontendCollisions(apps, []string{"web"})
	if err == nil {
		t.Fatal("--frontend web beside an existing web/ Next.js app was not refused")
	}
	for _, s := range []string{"web/ already holds a Next.js app", "Nothing was written", "path: web", "type: nextjs"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("refusal lacks %q:\n%s", s, err)
		}
	}
	if err := checkInPlaceFrontendCollisions(apps, []string{"dashboard"}); err != nil {
		t.Errorf("an unrelated frontend name was refused: %v", err)
	}
}
