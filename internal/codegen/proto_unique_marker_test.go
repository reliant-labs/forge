package codegen

import (
	"reflect"
	"strings"
	"testing"
)

// `// forge:unique a,b` is the message-level spelling of a table key. Before
// it, uniqueness had no proto-side declaration at all, and every key was a
// hand edit of the born migration.
func TestUniqueMarkerColumns(t *testing.T) {
	cases := []struct {
		line  string
		cols  []string
		found bool
	}{
		{"// forge:unique namespace,name", []string{"namespace", "name"}, true},
		{"//forge:unique namespace , name", []string{"namespace", "name"}, true},
		{"// forge:unique email", []string{"email"}, true},
		{"// forge:unique namespace,name — one job name per namespace", []string{"namespace", "name"}, true},
		{"// forge:entity forge:unique tenant_id,slug", []string{"tenant_id", "slug"}, true},
		{"  // forge:unique a, b, c  ", []string{"a", "b", "c"}, true},
		// The marker with nothing usable: found, no columns — birth refuses.
		{"// forge:unique", nil, true},
		{"// forge:unique — why", nil, true},
		// Prose after a bare blank would silently shrink the key to its
		// first column; refused instead.
		{"// forge:unique namespace and name", nil, true},
		// Not the marker.
		{"// the forge:unique key is (a, b)", nil, false},
		{"// forge:unique-together a,b", nil, false},
		{"// forge:uniqueness a", nil, false},
		{"string name = 1;", nil, false},
	}
	for _, c := range cases {
		cols, found := UniqueMarkerColumns(c.line)
		if found != c.found || !reflect.DeepEqual(cols, c.cols) {
			t.Errorf("UniqueMarkerColumns(%q) = (%q, %v), want (%q, %v)", c.line, cols, found, c.cols, c.found)
		}
	}
	if !IsKnownProtoMarker(ProtoMarkerUnique) {
		t.Errorf("%s must be in KnownProtoMarkers, or the proto-markers lint calls every use of it unknown", ProtoMarkerUnique)
	}
}

func TestUniqueMarkerNearMiss(t *testing.T) {
	for line, want := range map[string]string{
		"// forge:uniqe namespace,name":       "forge:uniqe",
		"// forge:uniq email":                 "forge:uniq",
		"// forge:unique-index a,b":           "forge:unique-index",
		"// forge:unique a,b":                 "",
		"// forge:entity":                     "",
		"// forge:unmanaged":                  "",
		"// forge:custom":                     "",
		"// see forge:entity docs for unique": "",
	} {
		if got := UniqueMarkerNearMiss(line); got != want {
			t.Errorf("UniqueMarkerNearMiss(%q) = %q, want %q", line, got, want)
		}
	}
}

// The scan binds each key to the top-level message it sits above, in source
// order, and a key alone tablizes the message the way forge:soft-delete
// does.
func TestScanRawProtoDir_UniqueMarker(t *testing.T) {
	dir := writeRawScanFixture(t, map[string]string{"v1/jobs.proto": `syntax = "proto3";
package services.jobs.v1;

// forge:entity
// forge:unique namespace,name — one job name per namespace
// Job is one sharded application.
// forge:unique slug
message Job {
  string namespace = 1;
  string name = 2;
  string slug = 3;
}

// forge:entity forge:unique tenant_id,email
message Member {
  string tenant_id = 1;
  string email = 2;
}

// forge:unique code
message Coupon {
  string code = 1;
}

message Plain {
  string x = 1;
}
`})
	scan, err := ScanRawProtoDir(dir)
	if err != nil {
		t.Fatalf("ScanRawProtoDir: %v", err)
	}
	want := map[string][][]string{
		"Job":    {{"namespace", "name"}, {"slug"}},
		"Member": {{"tenant_id", "email"}},
		"Coupon": {{"code"}},
		"Plain":  nil,
	}
	for msg, keys := range want {
		m, ok := scan.MessageByName(msg)
		if !ok {
			t.Fatalf("%s not scanned", msg)
		}
		var got [][]string
		for _, u := range m.Unique {
			got = append(got, u.Columns)
		}
		if !reflect.DeepEqual(got, keys) {
			t.Errorf("%s: unique keys = %q, want %q", msg, got, keys)
		}
		if m.Marked != (keys != nil) {
			t.Errorf("%s: Marked = %v, want %v (a unique key tablizes the message)", msg, m.Marked, keys != nil)
		}
		if len(m.UnappliedUniqueMarkers)+len(m.UniqueMarkerNearMisses) != 0 {
			t.Errorf("%s: nothing is misplaced or misspelled, got unapplied %v near-misses %v",
				msg, m.UnappliedUniqueMarkers, m.UniqueMarkerNearMisses)
		}
	}
	job, _ := scan.MessageByName("Job")
	if got := job.Unique[0].Site.String(); !strings.Contains(got, "jobs.proto:5: // forge:unique namespace,name") {
		t.Errorf("Job's first key site = %q, want jobs.proto:5", got)
	}
}

// A key birth does not read is refused, never dropped: inside a body, after
// a field, above a nested message or an enum. So is a near-miss spelling.
// Each lands on the message whose birth it would have shaped.
func TestScanRawProtoDir_MisplacedUniqueMarker(t *testing.T) {
	dir := writeRawScanFixture(t, map[string]string{"v1/jobs.proto": `syntax = "proto3";
package services.jobs.v1;

// forge:entity
message Job {
  // forge:unique namespace,name
  string namespace = 1;
  string name = 2; // forge:unique name
  // forge:unique x
  message Inner {
    string x = 1;
  }
}

// forge:entity
// forge:uniqe code
message Coupon {
  string code = 1; // forge:uniq code
}

// forge:unique status
enum Status {
  STATUS_UNSPECIFIED = 0;
}
`})
	scan, err := ScanRawProtoDir(dir)
	if err != nil {
		t.Fatalf("ScanRawProtoDir: %v", err)
	}
	job, _ := scan.MessageByName("Job")
	wantJob := []string{"jobs.proto:6:", "jobs.proto:8:", "jobs.proto:9:"}
	if len(job.UnappliedUniqueMarkers) != len(wantJob) {
		t.Fatalf("Job: UnappliedUniqueMarkers = %v, want sites %v", job.UnappliedUniqueMarkers, wantJob)
	}
	for i, w := range wantJob {
		if got := job.UnappliedUniqueMarkers[i].String(); !strings.HasPrefix(got, w) {
			t.Errorf("Job: unapplied[%d] = %q, want %s", i, got, w)
		}
	}
	if len(job.Unique) != 0 {
		t.Errorf("Job: misplaced markers must bind no key, got %v", job.Unique)
	}

	coupon, _ := scan.MessageByName("Coupon")
	if len(coupon.Unique) != 0 {
		t.Errorf("Coupon: a misspelled marker binds no key, got %v", coupon.Unique)
	}
	var near []string
	for _, s := range coupon.UniqueMarkerNearMisses {
		near = append(near, s.String())
	}
	if len(near) != 2 || !strings.HasPrefix(near[0], "jobs.proto:16:") || !strings.HasPrefix(near[1], "jobs.proto:18:") {
		t.Errorf("Coupon: near misses = %q, want lines 16 and 18", near)
	}
	// The key above an enum binds nothing and is attributed to the file's
	// last message (there is none after it), so its birth refuses.
	if len(coupon.UnappliedUniqueMarkers) != 1 || !strings.HasPrefix(coupon.UnappliedUniqueMarkers[0].String(), "jobs.proto:21:") {
		t.Errorf("Coupon: UnappliedUniqueMarkers = %v, want the key above enum Status (line 21)", coupon.UnappliedUniqueMarkers)
	}
}
