package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// previewProject is a project with a committed template env "preview".
func previewProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tmpl := filepath.Join(dir, "deploy", "kcl", "preview")
	if err := os.MkdirAll(tmpl, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"main.k": "# template main\n", "config.k": "# template config\n"} {
		if err := os.WriteFile(filepath.Join(tmpl, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// fakePreviewWorld stubs the render (namespace "demo-<env>" on k3d-demo, or a
// fixed one), the clock and kubectl, and restores them after the test.
type fakePreviewWorld struct {
	fixedNamespace string
	hosted         bool
	namespaces     map[string]map[string]string // ns -> labels on the live cluster
	annotations    map[string]string            // ns -> expires-at
	deleted        []string
	hostedDeleted  []string
}

func installPreviewWorld(t *testing.T, w *fakePreviewWorld, now time.Time) {
	t.Helper()
	oldRender, oldNow, oldKubectl, oldHosted := previewRender, previewNow, previewKubectl, previewDeleteHosted
	t.Cleanup(func() {
		previewRender, previewNow, previewKubectl, previewDeleteHosted = oldRender, oldNow, oldKubectl, oldHosted
	})
	previewNow = func() time.Time { return now }
	previewRender = func(_ context.Context, _ string, env string) (*KCLEntities, error) {
		ns := "demo-" + env
		if w.fixedNamespace != "" {
			ns = w.fixedNamespace
		}
		e := &KCLEntities{ManifestNamespaces: []RenderedNamespaceEntity{{Cluster: "k3d-demo", Name: ns}}}
		if w.hosted {
			e.ControlPlane = &ControlPlaneEntity{}
		}
		return e, nil
	}
	previewDeleteHosted = func(_ context.Context, name string, _ io.Writer) error {
		w.hostedDeleted = append(w.hostedDeleted, name)
		return nil
	}
	previewKubectl = func(_ context.Context, kctx string, args ...string) ([]byte, error) {
		switch {
		case args[0] == "get" && args[1] == "namespace":
			labels, ok := w.namespaces[args[2]]
			if !ok {
				return nil, nil
			}
			return json.Marshal(map[string]any{"metadata": map[string]any{"name": args[2], "labels": labels}})
		case args[0] == "get" && args[1] == "namespaces":
			var items []any
			for ns, labels := range w.namespaces {
				items = append(items, map[string]any{"metadata": map[string]any{
					"name": ns, "labels": labels, "annotations": map[string]string{previewExpiresAnnot: w.annotations[ns]},
				}})
			}
			return json.Marshal(map[string]any{"items": items})
		case args[0] == "delete":
			w.deleted = append(w.deleted, kctx+"/"+args[2])
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected kubectl %v", args)
	}
}

func TestPreviewCreate(t *testing.T) {
	dir := previewProject(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	installPreviewWorld(t, &fakePreviewWorld{}, now)
	f := previewKeyFlags{pr: 42, from: "preview"}

	doc, err := createPreview(context.Background(), dir, f, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Name != "preview-pr-42" || doc.Refreshed || doc.ExpiresAt != "2026-11-14T12:00:00Z" || doc.Source != "pr-42" ||
		len(doc.Namespaces) != 1 || doc.Namespaces[0] != "k3d-demo/demo-preview-pr-42" {
		t.Fatalf("doc: %+v", doc)
	}
	inst := filepath.Join(dir, "deploy", "kcl", "preview-pr-42")
	for _, f := range []string{"main.k", "config.k", previewHeaderFileName} {
		if _, err := os.Stat(filepath.Join(inst, f)); err != nil {
			t.Errorf("instance is missing %s: %v", f, err)
		}
	}
	m, ok, err := kclrender.ReadPreviewMarker(inst)
	if err != nil || !ok || m.Of != "preview" || m.ExpiresAt != doc.ExpiresAt {
		t.Fatalf("marker: %+v %v %v", m, ok, err)
	}

	// Re-running refreshes the expiry.
	installPreviewWorld(t, &fakePreviewWorld{}, now.Add(24*time.Hour))
	doc, err = createPreview(context.Background(), dir, f, 72*time.Hour)
	if err != nil || !doc.Refreshed || doc.ExpiresAt != "2026-11-15T12:00:00Z" {
		t.Fatalf("refresh: %+v %v", doc, err)
	}

	// A hand-written env of that name is never overwritten.
	hand := filepath.Join(dir, "deploy", "kcl", "preview-pr-7")
	if err := os.MkdirAll(hand, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 7, from: "preview"}, time.Hour); err == nil || !strings.Contains(err.Error(), "not a preview") {
		t.Errorf("hand-written env: %v", err)
	}
	// A template that is itself an instance is refused.
	if _, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 1, from: "preview-pr-42"}, time.Hour); err == nil || !strings.Contains(err.Error(), "itself a preview") {
		t.Errorf("instance as template: %v", err)
	}
}

// An instance that would render into the template env's own namespace is
// refused, and the copy it made is removed.
func TestPreviewCreateRefusesTheTemplatesNamespace(t *testing.T) {
	dir := previewProject(t)
	installPreviewWorld(t, &fakePreviewWorld{fixedNamespace: "demo-staging"}, time.Now())
	_, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 3, from: "preview"}, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "k3d-demo/demo-staging") || !strings.Contains(err.Error(), `option(\"env\")`) && !strings.Contains(err.Error(), `option("env")`) {
		t.Fatalf("want a namespace clash refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "kcl", "preview-pr-3")); !os.IsNotExist(err) {
		t.Errorf("the refused instance was left behind: %v", err)
	}
}

func TestPreviewDelete(t *testing.T) {
	dir := previewProject(t)
	w := &fakePreviewWorld{hosted: true, namespaces: map[string]map[string]string{
		"demo-preview-pr-42": {previewOfLabel: "preview"},
	}}
	installPreviewWorld(t, w, time.Now())
	f := previewKeyFlags{pr: 42, from: "preview"}
	// A fresh checkout: the instance directory does not exist yet.
	if err := deletePreview(context.Background(), dir, f, "preview-pr-42", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(w.hostedDeleted) != 1 || w.hostedDeleted[0] != "preview-pr-42" {
		t.Errorf("hosted deletes: %v", w.hostedDeleted)
	}
	if len(w.deleted) != 1 || w.deleted[0] != "k3d-demo/demo-preview-pr-42" {
		t.Errorf("namespace deletes: %v", w.deleted)
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy", "kcl", "preview-pr-42")); !os.IsNotExist(err) {
		t.Errorf("instance directory left behind: %v", err)
	}

	// A namespace the preview label does not mark is left alone.
	w2 := &fakePreviewWorld{namespaces: map[string]map[string]string{"demo-preview-pr-43": {}}}
	installPreviewWorld(t, w2, time.Now())
	if err := deletePreview(context.Background(), dir, previewKeyFlags{pr: 43, from: "preview"}, "preview-pr-43", io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(w2.deleted) != 0 {
		t.Errorf("an unlabelled namespace was deleted: %v", w2.deleted)
	}

	// An ordinary env is never deleted as a preview.
	if err := deletePreview(context.Background(), dir, f, "preview", io.Discard); err == nil || !strings.Contains(err.Error(), "not a preview instance") {
		t.Errorf("ordinary env: %v", err)
	}
}

func TestFindExpiredPreviews(t *testing.T) {
	dir := previewProject(t)
	created := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	w := &fakePreviewWorld{
		namespaces: map[string]map[string]string{
			"demo-preview-pr-1":  {previewOfLabel: "preview", previewInstanceLabel: "preview-pr-1"},
			"demo-preview-pr-2":  {previewOfLabel: "preview", previewInstanceLabel: "preview-pr-2"},
			"demo-preview-bad":   {previewOfLabel: "preview", previewInstanceLabel: "preview-bad"},
			"demo-other-pr-9":    {previewOfLabel: "other", previewInstanceLabel: "other-pr-9"},
			"demo-preview-pr-10": {previewOfLabel: "preview", previewInstanceLabel: "preview-pr-10"},
		},
		annotations: map[string]string{
			"demo-preview-pr-1":  "2026-11-02T00:00:00Z", // expired
			"demo-preview-pr-2":  "2026-12-01T00:00:00Z", // live
			"demo-preview-bad":   "whenever",             // unreadable: never expired
			"demo-other-pr-9":    "2026-11-02T00:00:00Z", // another template's
			"demo-preview-pr-10": "2026-11-02T00:00:00Z", // a local instance's: collected as the instance
		},
	}
	installPreviewWorld(t, w, created)
	if _, err := createPreview(context.Background(), dir, previewKeyFlags{pr: 10, from: "preview"}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	installPreviewWorld(t, w, created.Add(10*24*time.Hour))
	got, err := findExpiredPreviews(context.Background(), dir, []string{"preview"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range got {
		names = append(names, c.Kind+":"+c.Name)
	}
	if want := "instance:preview-pr-10,namespace:demo-preview-pr-1"; strings.Join(names, ",") != want {
		t.Fatalf("expired: %v, want %s", names, want)
	}
}
