package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/authhooks"
)

func TestAuthHooksSQLPrintsTheShippedFile(t *testing.T) {
	cmd := newAuthCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"hooks", "sql", "supabase"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("forge auth hooks sql supabase: %v", err)
	}
	if out.String() != authhooks.SupabaseSQL() {
		t.Error("printed SQL differs from authhooks.SupabaseSQL()")
	}
	cmd = newAuthCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"hooks", "sql", "firebase"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "supabase") {
		t.Errorf("firebase: err = %v, want a refusal naming the providers forge has", err)
	}
}
