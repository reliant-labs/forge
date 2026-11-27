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
