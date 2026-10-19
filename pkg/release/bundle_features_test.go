package release

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// bundleFeatureKeys is the document key paths each optional feature adds.
var bundleFeatureKeys = map[string][]string{
	BundleFeatureCommitIdentity:  {"provenance.commit_subject", "provenance.commit_author"},
	BundleFeatureObjectSummaries: {"objects"},
}

// snapshotKeys are not features: a snapshot bundle is its own schema, gated
// on the server's snapshot capability (see bundle_features.go).
var snapshotKeys = []string{"snapshot", "snapshot_digest"}

// legacyBundleDocKeys is the key-path fixture of the decoder every control
// plane built before forge#850 runs (forge ee3bded4).
func legacyBundleDocKeys(t *testing.T) []string {
	t.Helper()
	f, err := os.Open("testdata/bundle_doc_keys.ee3bded4.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" && !strings.HasPrefix(line, "#") {
			keys = append(keys, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return keys
}

func underAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if path == p || strings.HasPrefix(path, p+".") || strings.HasPrefix(path, p+"[]") || strings.HasPrefix(path, p+"{}") {
			return true
		}
	}
	return false
}

// TestEveryBundleDocAdditionIsAFeature: every key path BundleDoc gained since
// the legacy decoder is owned by a declared feature, so a field added to the
// document cannot reach an older control plane unannounced — the way
// forge#850's commit_subject did.
func TestEveryBundleDocAdditionIsAFeature(t *testing.T) {
	legacy := legacyBundleDocKeys(t)
	var owned []string
	for _, f := range BundleDocFeatures() {
		keys, ok := bundleFeatureKeys[f]
		if !ok {
			t.Errorf("feature %q has no key paths in bundleFeatureKeys", f)
		}
		owned = append(owned, keys...)
	}
	for _, p := range bundleDocKeyPaths(reflect.TypeFor[BundleDoc]()) {
		if slices.Contains(legacy, p) || underAny(p, owned) || underAny(p, snapshotKeys) {
			continue
		}
		t.Errorf("BundleDoc key %q is new since the legacy decoder (forge ee3bded4) and belongs to no feature.\n\n"+
			"Every control plane that predates it refuses the whole bundle with `unknown field`. Add a "+
			"feature for it in bundle_features.go (BundleDocFeatures, BundleReader.Limit, bundleFeatureKeys).", p)
	}
	for _, p := range legacy {
		if !slices.Contains(bundleDocKeyPaths(reflect.TypeFor[BundleDoc]()), p) {
			t.Errorf("BundleDoc no longer has key %q, which the legacy fixture names; a removal is not a feature — regenerate the fixture only for a new baseline", p)
		}
	}
}

func fullBundleDoc() BundleDoc {
	api := obj("Deployment", "api", "a")
	return BundleDoc{
		Schema: BundleSchema, Project: "cp", Env: "prod", Release: "v1",
		ConfigDigest: digest("c"),
		Pins:         BundlePins{Images: map[string]string{"api": digest("a")}},
		Provenance: Provenance{
			Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40), ForgeVersion: "v0.1.46",
			CommitSubject: "fix(boot): shared state converges in admin-server only", CommitAuthor: "Reliant",
		},
		Shape:     Shape{Kind: EnvSelfManaged, Objects: []ShapeObject{api}},
		Objects:   []ObjectSummary{{Key: api.Key(), Summary: "2 replicas · containers api"}},
		CreatedAt: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
	}
}

// TestAnOldReaderGetsADocumentItsDecoderAccepts: a reader that names no
// feature — a control plane that predates GetCapabilities, or the
// capability — gets a document every key of which the legacy strict decoder
// knows. The unlimited document is the control: it must stray, or the
// fixture proves nothing.
func TestAnOldReaderGetsADocumentItsDecoderAccepts(t *testing.T) {
	legacy := legacyBundleDocKeys(t)
	full := fullBundleDoc()

	raw, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	strays, err := unknownKeyPaths(raw, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"objects", "provenance.commit_author", "provenance.commit_subject"}; !slices.Equal(strays, want) {
		t.Fatalf("the full document strays at %v, want %v: the fixture or the doc helper no longer exercises the forge#850 fields", strays, want)
	}

	limited := (&BundleReader{}).Limit(full)
	raw, err = json.Marshal(limited)
	if err != nil {
		t.Fatal(err)
	}
	if strays, err := unknownKeyPaths(raw, legacy); err != nil || len(strays) != 0 {
		t.Fatalf("a document for a reader with no features carries keys the legacy decoder refuses: %v (err %v).\n\n"+
			"That is `RecordBundle: bundle document: json: unknown field` on every control plane built before forge#850.", strays, err)
	}
	var top struct {
		Objects    *json.RawMessage           `json:"objects"`
		Provenance map[string]json.RawMessage `json:"provenance"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	_, subject := top.Provenance["commit_subject"]
	_, author := top.Provenance["commit_author"]
	if top.Objects != nil || subject || author {
		t.Errorf("the limited document still carries a forge#850 key, even empty (objects=%v subject=%v author=%v); an empty key is still an unknown key",
			top.Objects != nil, subject, author)
	}
	// What it keeps is still a valid document for THIS decoder.
	if _, err := DecodeBundleDoc(raw); err != nil {
		t.Fatalf("the limited document no longer decodes: %v", err)
	}
	if limited.Provenance.Commit != full.Provenance.Commit || !reflect.DeepEqual(limited.Shape, full.Shape) {
		t.Fatal("Limit removed more than the optional features")
	}
}

// TestANewReaderGetsTheFullDocument: a reader that names every feature, and
// the nil reader (this forge's own machine ledger), get the document whole.
func TestANewReaderGetsTheFullDocument(t *testing.T) {
	full := fullBundleDoc()
	for name, r := range map[string]*BundleReader{
		"every feature": {Features: BundleDocFeatures()},
		"nil reader":    nil,
	} {
		if got := r.Limit(full); !reflect.DeepEqual(got, full) {
			t.Errorf("%s: Limit changed the document:\n%+v\n%+v", name, got, full)
		}
	}
	partial := (&BundleReader{Features: []string{BundleFeatureObjectSummaries}}).Limit(full)
	if len(partial.Objects) != 1 || partial.Provenance.CommitSubject != "" || partial.Provenance.CommitAuthor != "" {
		t.Errorf("a reader of object summaries only: %+v", partial)
	}
}
