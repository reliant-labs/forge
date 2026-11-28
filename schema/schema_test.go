package schema

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Each published schema is the embedded copy of the file in this directory,
// parses as JSON, declares draft 2020-12, and its $id names the file it is
// published as: a release publishes each file at its tag, so an $id pointing
// elsewhere would make the schema reference a document that is not it.
func TestEmbeddedSchemas(t *testing.T) {
	for _, tc := range []struct {
		name  string
		file  string
		get   func() []byte
		title string
	}{
		{"env", "env.v1.schema.json", EnvV1, "forge env document (forge.dev/env/v1)"},
		{"vuln-list", "vuln-list.v1.schema.json", VulnListV1, "forge vuln list document (forge.dev/vuln-list/v1)"},
		{"vuln-patch", "vuln-patch.v1.schema.json", VulnPatchV1, "forge vuln patch document (forge.dev/vuln-patch/v1)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.get()
			onDisk, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(onDisk) {
				t.Fatalf("accessor returns %d bytes, %s holds %d: the embed is stale or the wrong file", len(got), tc.file, len(onDisk))
			}
			var doc struct {
				Schema string `json:"$schema"`
				ID     string `json:"$id"`
				Title  string `json:"title"`
				Type   string `json:"type"`
			}
			if err := json.Unmarshal(got, &doc); err != nil {
				t.Fatalf("not valid JSON: %v", err)
			}
			if doc.Schema != "https://json-schema.org/draft/2020-12/schema" {
				t.Errorf("$schema = %q, want draft 2020-12", doc.Schema)
			}
			if !strings.HasSuffix(doc.ID, "/schema/"+tc.file) {
				t.Errorf("$id = %q, want it to end in /schema/%s", doc.ID, tc.file)
			}
			if doc.Title != tc.title {
				t.Errorf("title = %q, want %q", doc.Title, tc.title)
			}
		})
	}
}

// Each accessor serves its own schema, never another's.
func TestSchemaAccessorsAreDistinct(t *testing.T) {
	seen := map[string]string{}
	for name, b := range map[string][]byte{"env": EnvV1(), "vuln-list": VulnListV1(), "vuln-patch": VulnPatchV1()} {
		if len(b) == 0 {
			t.Fatalf("%s schema is empty", name)
		}
		if prev, dup := seen[string(b)]; dup {
			t.Errorf("%s and %s serve the same bytes", prev, name)
		}
		seen[string(b)] = name
	}
}
