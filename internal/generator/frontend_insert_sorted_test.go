package generator

import (
	"encoding/json"
	"testing"
)

// TestInsertIntoObjectKeepsNpmOrder: an inserted dependency lands where npm
// and pnpm would write it, and nothing else moves. It used to go first
// whatever its name, so `forge generate` put rrweb above @bufbuild/protobuf
// (obs 016c599b).
func TestInsertIntoObjectKeepsNpmOrder(t *testing.T) {
	cases := []struct{ name, in, dep, want string }{
		{"middle",
			"{\n  \"dependencies\": {\n    \"@bufbuild/protobuf\": \"^2.0.0\",\n    \"react\": \"^19.0.0\"\n  }\n}\n", "rrweb",
			"{\n  \"dependencies\": {\n    \"@bufbuild/protobuf\": \"^2.0.0\",\n    \"react\": \"^19.0.0\",\n    \"rrweb\": \"1.0.0\"\n  }\n}\n"},
		{"before a later name",
			"{\n  \"dependencies\": {\n    \"@bufbuild/protobuf\": \"^2.0.0\",\n    \"zod\": \"^3.0.0\"\n  }\n}\n", "rrweb",
			"{\n  \"dependencies\": {\n    \"@bufbuild/protobuf\": \"^2.0.0\",\n    \"rrweb\": \"1.0.0\",\n    \"zod\": \"^3.0.0\"\n  }\n}\n"},
		{"first",
			"{\n  \"dependencies\": {\n    \"react\": \"^19.0.0\"\n  }\n}\n", "@hyperdx/otel-web",
			"{\n  \"dependencies\": {\n    \"@hyperdx/otel-web\": \"1.0.0\",\n    \"react\": \"^19.0.0\"\n  }\n}\n"},
		{"empty object",
			"{\n  \"dependencies\": {}\n}\n", "rrweb",
			"{\n  \"dependencies\": {\n    \"rrweb\": \"1.0.0\"\n  }\n}\n"},
		{"one line",
			"{\"dependencies\": {\"a\": \"1\", \"z\": \"2\"}}", "m",
			"{\"dependencies\": {\"a\": \"1\", \"m\": \"1.0.0\", \"z\": \"2\"}}"},
		{"one line, last",
			"{\"dependencies\": {\"a\": \"1\"}}", "m",
			"{\"dependencies\": {\"a\": \"1\", \"m\": \"1.0.0\"}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := insertDependency(tc.in, tc.dep, "1.0.0")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("invalid JSON:\n%s", got)
			}
		})
	}
}
