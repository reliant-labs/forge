package v1alpha1

import (
	"encoding/json"
	"testing"
)

// MarkdownNegotiation and EdgeLogs are additive: a spec written before them
// decodes unchanged, a spec that sets neither encodes exactly as before, and
// EdgeLogs keeps an explicit false distinct from absent (absent = on).
func TestStaticSiteSpec_EdgeFieldsWire(t *testing.T) {
	old := `{"basePath":"/x","assetDir":"assets","keepReleases":10,"apis":{"billing":{"name":"billing-api"}}}`
	var spec StaticSiteSpec
	if err := json.Unmarshal([]byte(old), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.MarkdownNegotiation || spec.EdgeLogs != nil {
		t.Errorf("an older spec decoded new fields: markdown=%v edgeLogs=%v", spec.MarkdownNegotiation, spec.EdgeLogs)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != old {
		t.Errorf("an older spec re-encodes as\n%s\nwant\n%s", b, old)
	}

	off := false
	in := StaticSiteSpec{MarkdownNegotiation: true, EdgeLogs: &off}
	b, _ = json.Marshal(in)
	if want := `{"markdownNegotiation":true,"edgeLogs":false}`; string(b) != want {
		t.Errorf("wire = %s, want %s", b, want)
	}
	out := in.DeepCopy()
	*out.EdgeLogs = true
	if *in.EdgeLogs {
		t.Error("DeepCopy shares edgeLogs")
	}
}
