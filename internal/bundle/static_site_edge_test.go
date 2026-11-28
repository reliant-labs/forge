package bundle

import (
	"reflect"
	"testing"
)

// What Build hands release.BundleReader.CheckMarkdownNegotiation and
// CheckEdgeLogOptOut: every forge StaticSite whose spec asks for it, and
// nothing else (edgeLogs: true and an absent key both mean on).
func TestStaticSitesWhereEdgeFields(t *testing.T) {
	doc := func(name, spec string) string {
		return "---\n# cluster: hosted\napiVersion: forge.dev/v1alpha1\nkind: StaticSite\nmetadata:\n  name: " + name + "\nspec:" + spec + "\n"
	}
	stream := doc("md", "\n  markdownNegotiation: true") +
		doc("off", "\n  edgeLogs: false") +
		doc("on", "\n  edgeLogs: true\n  markdownNegotiation: false") +
		doc("plain", " {}") +
		"---\napiVersion: example.com/v1\nkind: StaticSite\nmetadata:\n  name: other\nspec:\n  markdownNegotiation: true\n  edgeLogs: false\n"
	docs, err := parseStream(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := staticSitesWhere(docs, specMarkdownNegotiation), []string{"md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("markdown sites = %v, want %v", got, want)
	}
	if got, want := staticSitesWhere(docs, specEdgeLogsOff), []string{"off"}; !reflect.DeepEqual(got, want) {
		t.Errorf("edge-off sites = %v, want %v", got, want)
	}
}

// The implicit edge-log opt-out marker never reaches a bundle: Build removes
// it, and writes edgeLogs: false only for a reader that honours it. An
// explicit value is never overridden, and the marker's removal leaves no
// empty annotations map behind (the doc is then byte-identical to a site
// without the opt-out).
func TestResolveImplicitEdgeLogOptOut(t *testing.T) {
	stream := func() string {
		return "---\n# cluster: hosted\napiVersion: forge.dev/v1alpha1\nkind: StaticSite\nmetadata:\n  name: web\n  annotations:\n    " +
			EdgeLogsOffIfHonouredAnnotation + ": \"true\"\nspec:\n  basePath: /x\n"
	}
	for honoured, want := range map[bool]map[string]any{
		false: {"basePath": "/x"},
		true:  {"basePath": "/x", "edgeLogs": false},
	} {
		docs, err := parseStream(stream())
		if err != nil {
			t.Fatal(err)
		}
		resolveImplicitEdgeLogOptOut(docs, honoured)
		body := docs[0].body.(map[string]any)
		if got := mapAt(body, "spec"); !reflect.DeepEqual(got, want) {
			t.Errorf("honoured=%v: spec = %v, want %v", honoured, got, want)
		}
		if _, ok := mapAt(body, "metadata")["annotations"]; ok {
			t.Errorf("honoured=%v: the marker's annotations map survived", honoured)
		}
	}
	docs, _ := parseStream("---\napiVersion: forge.dev/v1alpha1\nkind: StaticSite\nmetadata:\n  name: web\n  annotations:\n    " +
		EdgeLogsOffIfHonouredAnnotation + ": \"true\"\nspec:\n  edgeLogs: true\n")
	resolveImplicitEdgeLogOptOut(docs, true)
	if got := mapAt(docs[0].body.(map[string]any), "spec")["edgeLogs"]; got != true {
		t.Errorf("an explicit edgeLogs was overridden: %v", got)
	}
}
