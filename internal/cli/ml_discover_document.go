package cli

// The forge.dev/ml-discover/v1 document: what `forge ml discover --json`
// prints, and its JSON Schema (`forge project schema ml-discover`).
//
// The schema is GENERATED from MLDiscoverDocument, the type the command
// encodes, so it cannot describe a field the document lacks. Committed at
// schema/ml-discover.v1.schema.json and embedded in the binary;
// TestMLDiscoverSchemaIsCurrent fails on drift, and
// `FORGE_UPDATE_GOLDEN=1 go test ./internal/cli -run TestMLDiscoverSchemaIsCurrent`
// rewrites it after a field is added (here, or on mldiscover.Inventory or
// mlrecs.Finding, which it embeds). Fields are only ever added.

import (
	"encoding/json"
	"reflect"

	"github.com/reliant-labs/forge/internal/schemagen"
	"github.com/reliant-labs/forge/pkg/mldiscover"
	"github.com/reliant-labs/forge/pkg/mlrecs"
	forgeschema "github.com/reliant-labs/forge/schema"
)

// MLDiscoverDocumentSchema is the document's apiVersion.
const MLDiscoverDocumentSchema = "forge.dev/ml-discover/v1"

// mlDiscoverSchemaID is where each release publishes the schema.
const mlDiscoverSchemaID = "https://raw.githubusercontent.com/reliant-labs/forge/main/schema/ml-discover.v1.schema.json"

// MLDiscoverDocument is what `forge ml discover --json` prints: the cluster's
// inventory, the recommendations worst first, and the proposed ml.k.
type MLDiscoverDocument struct {
	// Schema is forge.dev/ml-discover/v1.
	Schema string `json:"schema"`
	mldiscover.Inventory
	// Findings are the recommendations, worst first; a finding an env's
	// forge.RecommendationDecision dismisses is still listed, marked, last.
	Findings []mlrecs.Finding `json:"findings"`
	// Proposal is the env's ml.k as discovery proposes it.
	Proposal MLDiscoverProposal `json:"proposal"`
	// Snapshot is where this run's read was saved
	// (.forge/state/ml-inventory-<context>.json inside a project, or
	// --save-snapshot), when it was.
	Snapshot string `json:"snapshot,omitempty"`
	// Warnings says what discovery could not see or apply: a refused list, a
	// CRD whose controller is missing, an env whose decisions did not
	// render. An absence is never overstated.
	Warnings []string `json:"warnings"`
}

// MLDiscoverProposal is the proposed ml.k.
type MLDiscoverProposal struct {
	// Path is where the ml.k goes (--out, else deploy/kcl/<env>/ml.k);
	// empty when neither --env nor --out placed it.
	Path string `json:"path,omitempty"`
	// Written is true when --write wrote it this run. False with --write
	// means the file already held exactly this.
	Written bool `json:"written"`
	// Exists is true when a file was already at Path.
	Exists bool `json:"exists,omitempty"`
	// MLK is the proposed file, valid KCL that `import forge` resolves;
	// empty when there is nothing to propose.
	MLK string `json:"ml_k"`
	// Diff is the unified diff from the file at Path to MLK, when one
	// existed and differs.
	Diff string `json:"diff,omitempty"`
}

func generateMLDiscoverSchema(describe func(reflect.Type, string) string) ([]byte, error) {
	s, err := schemagen.Generate(reflect.TypeOf(MLDiscoverDocument{}), schemagen.Options{
		ID:    mlDiscoverSchemaID,
		Title: "forge ml discover document (" + MLDiscoverDocumentSchema + ")",
		Description: "What a cluster runs for ML, the recommendations, and the proposed ml.k: " +
			"the output of `forge ml discover --json`. Generated from the Go type forge encodes it with.",
		Required: map[reflect.Type][]string{
			reflect.TypeOf(MLDiscoverDocument{}):          {"schema", "collected_at", "cloud", "accelerators", "machines", "components", "findings", "proposal", "warnings"},
			reflect.TypeOf(MLDiscoverProposal{}):          {"written", "ml_k"},
			reflect.TypeOf(mldiscover.AcceleratorGroup{}): {"vendor", "resource", "nodes", "capacity", "allocatable", "allocated", "detected_by"},
			reflect.TypeOf(mldiscover.Machine{}):          {"name", "vendor", "resource", "capacity", "allocatable", "allocated", "ready"},
			reflect.TypeOf(mldiscover.Component{}):        {"kind", "category", "status", "owner", "matched_by"},
			reflect.TypeOf(mlrecs.Finding{}):              {"id", "severity", "confidence", "title", "evidence", "fix"},
			reflect.TypeOf(mlrecs.Fix{}):                  {"kind", "summary"},
			reflect.TypeOf(mlrecs.Dismissal{}):            {"reason", "expires"},
		},
		Describe: describe,
	})
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// mlDiscoverSchemaJSON is the published schema, as embedded in this binary.
func mlDiscoverSchemaJSON() []byte { return forgeschema.MLDiscoverV1() }
