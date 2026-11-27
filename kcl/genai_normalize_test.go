package kcl

import (
	"encoding/json"
	"os"
	"slices"
	"testing"

	kclgo "kcl-lang.io/kcl-go"

	"github.com/reliant-labs/forge/pkg/observe/genai/conventions"
)

// TestGenAINormalizeStatementsEvaluate: genai_normalize_gen.k is valid KCL,
// and what a config reads from forge.GENAI_NORMALIZE_SPAN_STATEMENTS is
// exactly the table's OTTL statements, quoting included (the file's text is
// pinned against the table by pkg/observe/genai/conventions; this proves the
// KCL evaluates to it).
func TestGenAINormalizeStatementsEvaluate(t *testing.T) {
	// The native runtime reads the file outside the test process's view;
	// reading it here keys Go's test cache on it.
	if _, err := os.ReadFile("genai_normalize_gen.k"); err != nil {
		t.Fatal(err)
	}
	res, err := kclgo.Run("genai_normalize_gen.k")
	if err != nil {
		t.Fatalf("evaluate genai_normalize_gen.k: %v", err)
	}
	var out struct {
		Version    int      `json:"GENAI_NORMALIZE_VERSION"`
		Statements []string `json:"GENAI_NORMALIZE_SPAN_STATEMENTS"`
	}
	if err := json.Unmarshal([]byte(res.GetRawJsonResult()), &out); err != nil {
		t.Fatalf("decode: %v\n%s", err, res.GetRawJsonResult())
	}
	if out.Version != conventions.Version {
		t.Errorf("GENAI_NORMALIZE_VERSION = %d, want %d", out.Version, conventions.Version)
	}
	if !slices.Equal(out.Statements, conventions.OTTL()) {
		t.Errorf("GENAI_NORMALIZE_SPAN_STATEMENTS evaluates to\n%q\nwant conventions.OTTL()\n%q", out.Statements, conventions.OTTL())
	}
}
