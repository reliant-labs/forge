package seedplan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/schemadef"
)

func TestJSONDocumentExhaustedRefusesPlan(t *testing.T) {
	for _, docs := range [][]string{
		{`{}`, `{"key":"value"}`, `{"key":"other"}`},
		{`[]`, `["value"]`, `["other"]`},
	} {
		t.Run(docs[0], func(t *testing.T) {
			col := schemadef.Column{Name: "payload", Type: schemadef.TypeJSON, NotNull: true, Default: sqlString(docs[0]) + "::jsonb"}
			var terms []string
			for _, doc := range docs {
				terms = append(terms, "payload <> "+sqlString(doc)+"::jsonb")
			}
			// Decode into the schema type rather than duplicating its CHECK type.
			fixture, err := json.Marshal(map[string]any{
				"Name": "documents",
				"Columns": []schemadef.Column{col},
				"Checks": []map[string]any{{"Name": "payload_exclusions", "Columns": []string{"payload"}, "Def": "CHECK (" + strings.Join(terms, " AND ") + ")"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var table schemadef.Table
			if err := json.Unmarshal(fixture, &table); err != nil {
				t.Fatal(err)
			}
			if got := JSONDocumentFor(table, col); got != "" {
				t.Fatalf("exhausted candidates returned %q", got)
			}
			p, err := BuildPlan([]schemadef.Table{table}, nil, Config{})
			if err != nil {
				t.Fatal(err)
			}
			err = p.Validate()
			for _, want := range []string{"documents.payload", "payload_exclusions", "CHECK"} {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("validation = %v; want %s", err, want)
				}
			}
			if got := p.Statements(); len(got) != 0 {
				t.Fatalf("unplaceable plan emitted statements: %v", got)
			}
		})
	}
}
