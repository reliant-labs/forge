package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// covDRunPreview runs `env preview <args>` from dir and returns its stdout.
func covDRunPreview(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	cmd := newEnvPreviewCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

func covDProjectWithForgeYAML(t *testing.T) string {
	t.Helper()
	dir := previewProject(t)
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte("name: demo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCovDPreviewNameCmd(t *testing.T) {
	cases := []struct {
		args    []string
		want    string
		wantErr string
	}{
		{[]string{"name", "--pr", "123"}, "preview-pr-123\n", ""},
		{[]string{"name", "--branch", "feat/Login Page"}, "", ""},
		{[]string{"name", "--pr", "7", "--from", "staging"}, "staging-pr-7\n", ""},
		{[]string{"name", "--pr", "7", "--name-template", "{env}-x-{id}"}, "preview-x-pr-7\n", ""},
		{[]string{"name"}, "", "--pr <number> or --branch <name>"},
	}
	for _, c := range cases {
		out, err := covDRunPreview(t, t.TempDir(), c.args...)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%v: %v", c.args, err)
			continue
		}
		if c.want != "" && out != c.want {
			t.Errorf("%v: out = %q, want %q", c.args, out, c.want)
		}
		if c.want == "" && (!strings.HasPrefix(out, "preview-") || strings.ContainsAny(strings.TrimSpace(out), " /")) {
			t.Errorf("%v: a branch name must become a DNS-safe instance name, got %q", c.args, out)
		}
	}
}

func TestCovDPreviewCreateCmd_TextAndJSON(t *testing.T) {
	dir := covDProjectWithForgeYAML(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	installPreviewWorld(t, &fakePreviewWorld{}, now)

	out, err := covDRunPreview(t, dir, "create", "--pr", "5", "--ttl", "48h")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Created preview preview-pr-5 (from preview, pr-5), expires " + now.Add(48*time.Hour).Format(time.RFC3339),
		"namespace k3d-demo/demo-preview-pr-5",
		"Next: ", "env deploy preview-pr-5 --yes",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, err = covDRunPreview(t, dir, "create", "--pr", "5", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc previewCreateDoc
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("--json must print one document: %v\n%s", err, out)
	}
	if !doc.Refreshed || doc.Name != "preview-pr-5" || doc.Of != "preview" || doc.ExpiresAt != now.Add(72*time.Hour).Format(time.RFC3339) {
		t.Errorf("doc = %+v (a re-run refreshes and defaults to a 72h ttl)", doc)
	}

	out, err = covDRunPreview(t, dir, "create", "--pr", "5")
	if err != nil || !strings.Contains(out, "Refreshed preview preview-pr-5") {
		t.Errorf("a re-run reports Refreshed: %q %v", out, err)
	}
}

func TestCovDPreviewCreateCmd_Refusals(t *testing.T) {
	dir := covDProjectWithForgeYAML(t)
	installPreviewWorld(t, &fakePreviewWorld{}, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))

	if _, err := covDRunPreview(t, dir, "create", "--pr", "1", "--ttl", "forever"); err == nil || !strings.Contains(err.Error(), "--ttl 72h or --ttl 7d") {
		t.Errorf("bad ttl: %v", err)
	}
	if _, err := covDRunPreview(t, dir, "create", "--pr", "1", "--ttl", "9000h"); err == nil {
		t.Error("a ttl over 30d must be refused")
	}
	if _, err := covDRunPreview(t, dir, "create", "--ttl", "1h"); err == nil || !strings.Contains(err.Error(), "--pr <number> or --branch <name>") {
		t.Errorf("no key: %v", err)
	}
	if _, err := covDRunPreview(t, dir, "create", "--pr", "1", "--from", "nowhere"); err == nil || !strings.Contains(err.Error(), `template env "nowhere" has no deploy/kcl/nowhere/main.k`) {
		t.Errorf("missing template: %v", err)
	}
	if _, err := covDRunPreview(t, t.TempDir(), "create", "--pr", "1"); err == nil || !strings.Contains(err.Error(), "forge.yaml not found") {
		t.Errorf("outside a project: %v", err)
	}
}

func TestCovDPreviewCreateRendersFailuresAreWrapped(t *testing.T) {
	dir := previewProject(t)
	installPreviewWorld(t, &fakePreviewWorld{}, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	boom := errors.New("kcl exploded")
	previewRender = func(_ context.Context, _ string, env string) (*KCLEntities, error) {
		if env == "preview" {
			return nil, boom
		}
		return &KCLEntities{}, nil
	}
	_, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 3, from: "preview"}, time.Hour)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "render template env preview") {
		t.Fatalf("template render failure: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "deploy", "kcl", "preview-pr-3")); !os.IsNotExist(statErr) {
		t.Errorf("a failed create must roll the instance back, stat err = %v", statErr)
	}

	previewRender = func(_ context.Context, _ string, env string) (*KCLEntities, error) {
		return nil, boom
	}
	_, err = createPreview(context.Background(), dir, previewKeyFlags{pr: 3, from: "preview"}, time.Hour)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "render preview preview-pr-3") {
		t.Fatalf("instance render failure: %v", err)
	}
}

func TestCovDPreviewDeleteCmd(t *testing.T) {
	dir := covDProjectWithForgeYAML(t)
	w := &fakePreviewWorld{namespaces: map[string]map[string]string{"demo-preview-pr-8": {previewOfLabel: "preview"}}}
	installPreviewWorld(t, w, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	prev := lifecycleInteractive
	lifecycleInteractive = func() bool { return false }
	t.Cleanup(func() { lifecycleInteractive = prev })

	if _, err := covDRunPreview(t, dir, "delete", "--pr", "8"); err == nil || !strings.Contains(err.Error(), "refusing to delete a preview without confirmation") {
		t.Fatalf("non-interactive without --yes: %v", err)
	}
	if len(w.deleted) != 0 {
		t.Fatalf("nothing may be deleted before confirmation: %v", w.deleted)
	}
	out, err := covDRunPreview(t, dir, "delete", "--pr", "8", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "deleting namespace k3d-demo/demo-preview-pr-8") || !strings.Contains(out, "[preview] deleted preview-pr-8") {
		t.Errorf("output:\n%s", out)
	}
	if len(w.deleted) != 1 || w.deleted[0] != "k3d-demo/demo-preview-pr-8" {
		t.Errorf("deleted = %v", w.deleted)
	}
	// --name replaces the --pr/--branch key for an instance that exists.
	if _, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 9, from: "preview"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	if out, err := covDRunPreview(t, dir, "delete", "--name", "preview-pr-9", "--yes"); err != nil || !strings.Contains(out, "[preview] deleted preview-pr-9") {
		t.Errorf("--name: %q %v", out, err)
	}
	// A gone instance can only be re-created from its key, so --name alone is refused.
	if _, err := covDRunPreview(t, dir, "delete", "--name", "preview-pr-10", "--yes"); err == nil || !strings.Contains(err.Error(), "re-creating preview preview-pr-10 to delete it") {
		t.Errorf("--name with no instance and no key: %v", err)
	}
	if _, err := covDRunPreview(t, dir, "delete", "--yes"); err == nil || !strings.Contains(err.Error(), "--pr <number> or --branch <name>") {
		t.Errorf("no key and no name: %v", err)
	}
}

func TestCovDDeletePreviewRenderFailureStopsBeforeDeleting(t *testing.T) {
	dir := previewProject(t)
	w := &fakePreviewWorld{}
	installPreviewWorld(t, w, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	if _, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 4, from: "preview"}, time.Hour); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("no render today")
	previewRender = func(context.Context, string, string) (*KCLEntities, error) { return nil, boom }
	err := deletePreview(context.Background(), dir, previewKeyFlags{pr: 4, from: "preview"}, "preview-pr-4", io.Discard)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "render preview preview-pr-4") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "deploy", "kcl", "preview-pr-4")); statErr != nil {
		t.Errorf("a failed delete must keep the instance: %v", statErr)
	}
}

func TestCovDDeletePreviewNamespace_Errors(t *testing.T) {
	ctx := context.Background()
	oldKubectl := previewKubectl
	t.Cleanup(func() { previewKubectl = oldKubectl })

	previewKubectl = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("no cluster") }
	if err := deletePreviewNamespace(ctx, "ctx", "ns", "preview", io.Discard); err == nil || !strings.Contains(err.Error(), "no cluster") {
		t.Errorf("kubectl get failure: %v", err)
	}
	previewKubectl = func(context.Context, string, ...string) ([]byte, error) { return []byte("  \n"), nil }
	var out bytes.Buffer
	if err := deletePreviewNamespace(ctx, "ctx", "ns", "preview", &out); err != nil || out.Len() != 0 {
		t.Errorf("an absent namespace is a quiet no-op: %v %q", err, out.String())
	}
	previewKubectl = func(context.Context, string, ...string) ([]byte, error) { return []byte("{nope"), nil }
	if err := deletePreviewNamespace(ctx, "ctx", "ns", "preview", io.Discard); err == nil || !strings.Contains(err.Error(), "reading namespace ns on ctx") {
		t.Errorf("garbled kubectl output: %v", err)
	}
	calls := 0
	previewKubectl = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		if args[0] == "delete" {
			return nil, errors.New("forbidden")
		}
		return []byte(`{"metadata":{"labels":{"` + previewOfLabel + `":"preview"}}}`), nil
	}
	if err := deletePreviewNamespace(ctx, "ctx", "ns", "preview", io.Discard); err == nil || !strings.Contains(err.Error(), "forbidden") || calls != 2 {
		t.Errorf("delete failure: %v after %d calls", err, calls)
	}
	var left bytes.Buffer
	previewKubectl = func(context.Context, string, ...string) ([]byte, error) {
		return []byte(`{"metadata":{"labels":{"` + previewOfLabel + `":"other"}}}`), nil
	}
	if err := deletePreviewNamespace(ctx, "ctx", "ns", "preview", &left); err != nil || !strings.Contains(left.String(), "left alone") {
		t.Errorf("another template's namespace: %v %q", err, left.String())
	}
}

func covDRunGC(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	cmd := newEnvGCCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(args)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	return out.String(), err
}

func TestCovDEnvGCCmd(t *testing.T) {
	dir := covDProjectWithForgeYAML(t)
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	w := &fakePreviewWorld{
		namespaces:  map[string]map[string]string{"demo-orphan": {previewOfLabel: "preview"}},
		annotations: map[string]string{"demo-orphan": "2026-11-02T00:00:00Z"},
	}
	installPreviewWorld(t, w, created)
	if _, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 1, from: "preview"}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	installPreviewWorld(t, w, created.Add(10*24*time.Hour))
	prev := lifecycleInteractive
	lifecycleInteractive = func() bool { return false }
	t.Cleanup(func() { lifecycleInteractive = prev })

	if _, err := covDRunGC(t, dir); err == nil || !strings.Contains(err.Error(), "refusing to delete without confirmation") {
		t.Fatalf("non-interactive without --yes: %v", err)
	}

	out, err := covDRunGC(t, dir, "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"would delete instance preview-pr-1 (preview of preview, expired 2026-11-02T00:00:00Z)", "would delete namespace k3d-demo/demo-orphan"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if len(w.deleted) != 0 {
		t.Fatalf("--dry-run deleted %v", w.deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "kcl", "preview-pr-1")); err != nil {
		t.Fatalf("--dry-run removed the instance: %v", err)
	}

	out, err = covDRunGC(t, dir, "--yes", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Expired []gcCandidate `json:"expired"`
		Deleted bool          `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[len(strings.SplitN(out, "\n", 2))-1]), &doc); err != nil {
		// The deletion log lines precede the document; take the last line.
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil {
			t.Fatalf("no JSON document in %q: %v", out, err)
		}
	}
	if !doc.Deleted || len(doc.Expired) != 2 {
		t.Errorf("doc = %+v", doc)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "kcl", "preview-pr-1")); !os.IsNotExist(err) {
		t.Errorf("the expired instance must be removed: %v", err)
	}
	if len(w.deleted) == 0 || !strings.Contains(strings.Join(w.deleted, ","), "demo-orphan") {
		t.Errorf("the orphaned namespace must be deleted: %v", w.deleted)
	}

	delete(w.namespaces, "demo-orphan") // the fake kubectl does not drop what it deletes
	out, err = covDRunGC(t, dir, "--yes")
	if err != nil || !strings.Contains(out, "No expired previews.") {
		t.Errorf("nothing left: %q %v", out, err)
	}
}

func TestCovDFindExpiredPreviews_Errors(t *testing.T) {
	dir := previewProject(t)
	installPreviewWorld(t, &fakePreviewWorld{}, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC))
	boom := errors.New("kubectl down")
	previewKubectl = func(context.Context, string, ...string) ([]byte, error) { return nil, boom }
	if _, err := findExpiredPreviews(context.Background(), dir, []string{"preview"}, nil); !errors.Is(err, boom) {
		t.Errorf("kubectl failure: %v", err)
	}
	previewKubectl = func(context.Context, string, ...string) ([]byte, error) { return []byte("{nope"), nil }
	if _, err := findExpiredPreviews(context.Background(), dir, nil, []string{"extra"}); err == nil || !strings.Contains(err.Error(), "reading namespaces on extra") {
		t.Errorf("garbled kubectl output: %v", err)
	}
	renderBoom := errors.New("render down")
	previewRender = func(context.Context, string, string) (*KCLEntities, error) { return nil, renderBoom }
	if _, err := findExpiredPreviews(context.Background(), dir, []string{"preview"}, nil); !errors.Is(err, renderBoom) || !strings.Contains(err.Error(), "render template env preview") {
		t.Errorf("template render failure: %v", err)
	}
	// A template that does not exist is skipped, not rendered.
	if _, err := findExpiredPreviews(context.Background(), dir, []string{"missing"}, nil); err != nil {
		t.Errorf("an undeclared template has nothing to scan: %v", err)
	}
}

func TestCovDStagePreviewCopyAndSwap(t *testing.T) {
	t.Run("stage copies nested trees and skips the marker and header", func(t *testing.T) {
		kcl := t.TempDir()
		src := filepath.Join(kcl, "preview")
		covDWriteTree(t, src, map[string]string{
			"main.k":                    "m",
			"sub/deep/x.k":              "x",
			kclrender.PreviewMarkerFile: "{}",
			previewHeaderFileName:       "hdr",
		})
		stage, err := stagePreviewCopy(src, kcl, "preview-pr-1")
		if err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(stage, "sub", "deep", "x.k")); err != nil || string(b) != "x" {
			t.Errorf("nested file: %q %v", b, err)
		}
		for _, skipped := range []string{kclrender.PreviewMarkerFile, previewHeaderFileName} {
			if _, err := os.Stat(filepath.Join(stage, skipped)); !os.IsNotExist(err) {
				t.Errorf("%s must not be copied: %v", skipped, err)
			}
		}
	})

	t.Run("a missing template is an error and leaves no stage behind", func(t *testing.T) {
		kcl := t.TempDir()
		if _, err := stagePreviewCopy(filepath.Join(kcl, "absent"), kcl, "p"); err == nil {
			t.Fatal("want an error")
		}
		entries, _ := os.ReadDir(kcl)
		if len(entries) != 0 {
			t.Errorf("stage directory left behind: %v", entries)
		}
	})

	t.Run("swap commit and rollback", func(t *testing.T) {
		kcl := t.TempDir()
		dst := filepath.Join(kcl, "inst")
		covDWriteTree(t, kcl, map[string]string{"inst/old.txt": "old", "stage/new.txt": "new"})
		_, rollback, err := swapPreviewDir(filepath.Join(kcl, "stage"), dst)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dst, "new.txt")); err != nil {
			t.Fatalf("the staged copy must be in place: %v", err)
		}
		if err := rollback(); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(dst, "old.txt")); err != nil || string(b) != "old" {
			t.Errorf("rollback must restore the old instance: %q %v", b, err)
		}

		covDWriteTree(t, kcl, map[string]string{"stage2/new.txt": "new2"})
		commit, _, err := swapPreviewDir(filepath.Join(kcl, "stage2"), dst)
		if err != nil {
			t.Fatal(err)
		}
		if err := commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(kcl, "stage2.old")); !os.IsNotExist(err) {
			t.Errorf("commit must remove the old instance: %v", err)
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "new.txt")); string(b) != "new2" {
			t.Errorf("committed content = %q", b)
		}
	})

	t.Run("swap with no prior instance rolls back to nothing", func(t *testing.T) {
		kcl := t.TempDir()
		covDWriteTree(t, kcl, map[string]string{"stage/new.txt": "n"})
		dst := filepath.Join(kcl, "fresh")
		commit, rollback, err := swapPreviewDir(filepath.Join(kcl, "stage"), dst)
		if err != nil {
			t.Fatal(err)
		}
		if err := commit(); err != nil {
			t.Fatal(err)
		}
		if err := rollback(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Errorf("rollback with no prior instance removes the new one: %v", err)
		}
	})

	t.Run("a stage that cannot be renamed restores the old instance", func(t *testing.T) {
		kcl := t.TempDir()
		dst := filepath.Join(kcl, "inst")
		covDWriteTree(t, kcl, map[string]string{"inst/old.txt": "old"})
		if _, _, err := swapPreviewDir(filepath.Join(kcl, "no-such-stage"), dst); err == nil {
			t.Fatal("want an error")
		}
		if b, err := os.ReadFile(filepath.Join(dst, "old.txt")); err != nil || string(b) != "old" {
			t.Errorf("a failed swap must leave the old instance as it was: %q %v", b, err)
		}
	})
}
