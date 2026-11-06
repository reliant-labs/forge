package secrets

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateSpecEncodings(t *testing.T) {
	h, err := GenerateSpec{Bytes: 16, Encoding: "hex", Prefix: "k_"}.Generate()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(h, "k_"))
	if err != nil || len(raw) != 16 || !strings.HasPrefix(h, "k_") {
		t.Fatalf("bad hex value %q", h)
	}
	for _, bad := range []GenerateSpec{{Bytes: 0, Encoding: "hex"}, {Bytes: 8, Encoding: "rot13"}} {
		if _, err := bad.Generate(); err == nil {
			t.Fatalf("%+v should be rejected", bad)
		}
	}
}

// A scaffolded store lists every sensitive slot as "" — the header says an
// empty slot is not a value — so a generated key sitting in one is minted,
// while the other slots and the non-empty values are left exactly as they are.
func TestEnsureGeneratedFillsEmptyScaffoldedSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.yaml")
	if err := os.WriteFile(path, []byte("CONNECTION_KEYRING: \"\"\nSTRIPE_KEY: \"\"\nVAULT_KEY: mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	specs := map[string]GenerateSpec{
		"CONNECTION_KEYRING": {Bytes: 32, Encoding: "base64"},
		"VAULT_KEY":          {Bytes: 32, Encoding: "hex"},
	}
	wanted := []string{"CONNECTION_KEYRING", "STRIPE_KEY", "VAULT_KEY"}
	present := map[string]string{"CONNECTION_KEYRING": "", "STRIPE_KEY": "", "VAULT_KEY": "mine"}
	got, err := EnsureGenerated(path, specs, wanted, present)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "CONNECTION_KEYRING" {
		t.Fatalf("generated %v, want [CONNECTION_KEYRING]", got)
	}
	vals, err := ReadSecretFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if vals["CONNECTION_KEYRING"] == "" {
		t.Fatal("the empty CONNECTION_KEYRING slot was not minted")
	}
	if vals["VAULT_KEY"] != "mine" {
		t.Fatalf("a non-empty value was overwritten: VAULT_KEY = %q", vals["VAULT_KEY"])
	}
	if v, ok := vals["STRIPE_KEY"]; !ok || v != "" {
		t.Fatalf("a slot with no generate spec was touched: STRIPE_KEY = %q (present %v)", v, ok)
	}

	// A second pass finds a value and leaves it alone.
	first := vals["CONNECTION_KEYRING"]
	got, err = EnsureGenerated(path, specs, wanted, vals)
	if err != nil || len(got) != 0 {
		t.Fatalf("second pass generated %v err %v", got, err)
	}
	if again, _ := ReadSecretFile(path); again["CONNECTION_KEYRING"] != first {
		t.Fatal("a minted key was replaced on the second pass")
	}
}

// An empty slot in the file does not shadow a value the provider resolves
// from another layer (a worktree inheriting the primary checkout's store).
func TestEnsureGeneratedKeepsInheritedValueOverEmptySlot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.yaml")
	if err := os.WriteFile(path, []byte("K: \"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := EnsureGenerated(path, map[string]GenerateSpec{"K": {Bytes: 8, Encoding: "hex"}}, []string{"K"}, map[string]string{"K": "inherited"})
	if err != nil || len(got) != 0 {
		t.Fatalf("generated %v err %v despite an inherited value", got, err)
	}
}

func TestEnsureGeneratedSkipsKeysPresentInProviderView(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dev.yaml")
	got, err := EnsureGenerated(path, map[string]GenerateSpec{"K": {Bytes: 8, Encoding: "hex"}}, []string{"K"}, map[string]string{"K": "inherited"})
	if err != nil || len(got) != 0 {
		t.Fatalf("generated %v err %v despite an inherited value", got, err)
	}
}
