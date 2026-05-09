package starters

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestListStartersShipped confirms each migrated starter is discoverable
// and carries the expected metadata.
func TestListStartersShipped(t *testing.T) {
	starters, err := ListStarters()
	if err != nil {
		t.Fatalf("ListStarters: %v", err)
	}

	want := map[string]string{
		"stripe":        "Stripe billing",
		"twilio":        "Twilio SMS",
		"clerk-webhook": "Clerk webhook",
	}
	got := map[string]Starter{}
	for _, s := range starters {
		got[s.Name] = s
	}
	for name, descSubstr := range want {
		s, ok := got[name]
		if !ok {
			t.Errorf("starter %q not in ListStarters()", name)
			continue
		}
		if !strings.Contains(s.Description, descSubstr) {
			t.Errorf("starter %q description %q missing %q", name, s.Description, descSubstr)
		}
		if len(s.Files) == 0 {
			t.Errorf("starter %q ships no files", name)
		}
	}
}

// TestStripeStarterShape locks the Stripe starter to the agreed shape:
// handlers + webhook only, no proto entities.
func TestStripeStarterShape(t *testing.T) {
	s, err := LoadStarter("stripe")
	if err != nil {
		t.Fatalf("LoadStarter(stripe): %v", err)
	}
	if len(s.Deps.Go) == 0 {
		t.Error("stripe starter must list at least one Go dep")
	}
	for _, f := range s.Files {
		if strings.HasSuffix(f.Source, ".proto.tmpl") {
			t.Errorf("stripe starter must not ship proto templates (was %q)", f.Source)
		}
		if strings.HasSuffix(f.Destination, ".proto") {
			t.Errorf("stripe starter must not write proto files (dest %q)", f.Destination)
		}
	}
	// Notes should include the ownership banner.
	if !strings.Contains(s.Notes, "you own this code") {
		t.Errorf("stripe notes should include the ownership banner; got: %s", s.Notes)
	}
}

// TestTwilioStarterShape mirrors TestStripeStarterShape for Twilio.
func TestTwilioStarterShape(t *testing.T) {
	s, err := LoadStarter("twilio")
	if err != nil {
		t.Fatalf("LoadStarter(twilio): %v", err)
	}
	if len(s.Deps.Go) == 0 {
		t.Error("twilio starter must list at least one Go dep")
	}
	for _, f := range s.Files {
		if strings.HasSuffix(f.Source, ".proto.tmpl") {
			t.Errorf("twilio starter must not ship proto templates (was %q)", f.Source)
		}
	}
}

// TestClerkWebhookStarterShape confirms the clerk-webhook starter ships
// only the webhook router (the JWT/JWKS auth side stays in the clerk pack).
func TestClerkWebhookStarterShape(t *testing.T) {
	s, err := LoadStarter("clerk-webhook")
	if err != nil {
		t.Fatalf("LoadStarter(clerk-webhook): %v", err)
	}
	if len(s.Files) != 1 {
		t.Errorf("clerk-webhook starter should ship exactly 1 file (the webhook router); got %d", len(s.Files))
	}
	for _, f := range s.Files {
		if !strings.Contains(f.Source, "webhook") {
			t.Errorf("clerk-webhook starter file source %q should mention 'webhook'", f.Source)
		}
		if strings.Contains(f.Source, "validator") || strings.Contains(f.Source, "auth") {
			t.Errorf("clerk-webhook starter must NOT ship JWT/JWKS templates (they belong in the clerk pack); got %q", f.Source)
		}
	}
}

// TestStarterAddRendersFiles smoke-tests the end-to-end add flow against
// a temp project directory.
func TestStarterAddRendersFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStarter("stripe")
	if err != nil {
		t.Fatalf("LoadStarter(stripe): %v", err)
	}

	var out bytes.Buffer
	if err := s.Add(AddOptions{
		ProjectDir:  dir,
		ModulePath:  "github.com/example/myapp",
		ProjectName: "myapp",
		Service:     "billing",
		Stdout:      &out,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Each declared file should now exist under dir.
	for _, f := range s.Files {
		path := filepath.Join(dir, f.Destination)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist after Add: %v", f.Destination, err)
		}
	}
	// First-run output should announce a created file.
	if !strings.Contains(out.String(), "Created:") {
		t.Errorf("Add output should mention Created: lines; got:\n%s", out.String())
	}
}

// TestStarterAddSkipsExistingByDefault confirms the "user owns the code"
// contract — re-running add does not stomp customizations.
func TestStarterAddSkipsExistingByDefault(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadStarter("stripe")
	if err != nil {
		t.Fatalf("LoadStarter(stripe): %v", err)
	}

	// Pre-create a file the starter would write, with custom content.
	first := filepath.Join(dir, s.Files[0].Destination)
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	customContent := []byte("// I own this file now\n")
	if err := os.WriteFile(first, customContent, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	if err := s.Add(AddOptions{
		ProjectDir:  dir,
		ModulePath:  "github.com/example/myapp",
		ProjectName: "myapp",
		Stdout:      &out,
	}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read after Add: %v", err)
	}
	if string(got) != string(customContent) {
		t.Errorf("Add overwrote a pre-existing file (default behavior must skip).\nWant: %q\nGot:  %q", customContent, got)
	}
	if !strings.Contains(out.String(), "Skipping (exists)") {
		t.Errorf("Add output should mention Skipping (exists); got:\n%s", out.String())
	}
}

func TestValidStarterName(t *testing.T) {
	cases := map[string]bool{
		"stripe":        true,
		"clerk-webhook": true,
		"my_starter":    true,
		"":              false,
		"-leading":      false,
		"_leading":      false,
		"UPPER":         false,
		"has space":     false,
		"has/slash":     false,
		"has.dot":       false,
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ValidStarterName(name); got != want {
				t.Errorf("ValidStarterName(%q) = %v, want %v", name, got, want)
			}
		})
	}
}
