package credentials

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestOrganizationsRotationLogoutAndLegacyIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	ep := "https://cp.example"
	// Version 1 must remain readable and survive an upgrade, without assigning
	// an invented org to its ordinary or anonymous credentials.
	old := `{"version":1,"credentials":{"https://cp.example":{"forge-cli":{"token":"legacy"},"reliant-cli":{"token":"reliant"},"forge-cli-claimable":{"token":"anonymous","claimable":{"claim_url":"secret","org_id":"anonymous"}}}}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err := Lookup(path, ep, "forge-cli"); err != nil || c.Token != "legacy" {
		t.Fatalf("legacy: %v", err)
	}
	for _, c := range []Credential{{Token: "A1", OrgID: "A"}, {Token: "B1", OrgID: "B"}, {Token: "A2", OrgID: "A"}} {
		if err := Store(path, ep, "forge-cli", c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Lookup(path, ep, "forge-cli"); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("unscoped lookup guessed: %v", err)
	}
	for org, token := range map[string]string{"A": "A2", "B": "B1"} {
		c, err := Lookup(path, "https://CP.example/", "forge-cli", org)
		if err != nil || c.Token != token || c.OrgID != org {
			t.Fatalf("%s: wrong credential or error %v", org, err)
		}
	}
	if _, err := Lookup(path, ep, "forge-cli", "unknown"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown org fell back: %v", err)
	}
	if _, err := Remove(path, ep, "forge-cli"); !errors.Is(err, ErrOrgRequired) {
		t.Fatalf("unscoped logout: %v", err)
	}
	if ok, err := Remove(path, ep, "forge-cli", "A"); err != nil || !ok {
		t.Fatal(err)
	}
	if c, err := Lookup(path, ep, "forge-cli", "B"); err != nil || c.Token != "B1" {
		t.Fatalf("B lost on logout: %v", err)
	}
	for client, token := range map[string]string{"reliant-cli": "reliant", "forge-cli-claimable": "anonymous"} {
		if c, err := Lookup(path, ep, client); err != nil || c.Token != token {
			t.Fatalf("%s lost: %v", client, err)
		}
	}
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Credentials[ep]["forge-cli"].Token != "legacy" {
		t.Fatal("legacy was lost")
	}
	f.Organizations[ep]["B"]["forge-cli"] = Credential{Token: "bad", OrgID: "A"}
	if _, err := f.Get(ep, "forge-cli", "B"); err == nil {
		t.Fatal("mismatched org metadata accepted")
	}
}

func TestOrganizationsConcurrentStoresPreserveBoth(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	var wg sync.WaitGroup
	for _, org := range []string{"A", "B"} {
		wg.Add(1)
		go func(org string) {
			defer wg.Done()
			if err := Store(path, "https://cp.example", "forge-cli", Credential{Token: org, OrgID: org}); err != nil {
				t.Error(err)
			}
		}(org)
	}
	wg.Wait()
	for _, org := range []string{"A", "B"} {
		if c, err := Lookup(path, "https://cp.example", "forge-cli", org); err != nil || c.Token != org {
			t.Fatalf("concurrent write lost %s: %v", org, err)
		}
	}
}
