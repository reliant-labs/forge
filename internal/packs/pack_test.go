package packs

import (
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestLoadPack(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack(jwt-auth) error: %v", err)
	}

	if p.Name != "jwt-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "jwt-auth")
	}
	if p.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", p.Version, "1.0.0")
	}
	if p.Description == "" {
		t.Error("Description is empty")
	}
	if p.Config.Section != "auth" {
		t.Errorf("Config.Section = %q, want %q", p.Config.Section, "auth")
	}
	if len(p.Files) != 2 {
		t.Errorf("len(Files) = %d, want 2", len(p.Files))
	}
	if len(p.Dependencies) != 2 {
		t.Errorf("len(Dependencies) = %d, want 2", len(p.Dependencies))
	}
	if len(p.Generate) != 1 {
		t.Errorf("len(Generate) = %d, want 1", len(p.Generate))
	}
}

func TestLoadPackNotFound(t *testing.T) {
	_, err := LoadPack("nonexistent-pack")
	if err == nil {
		t.Fatal("LoadPack(nonexistent-pack) expected error, got nil")
	}
}

func TestListPacks(t *testing.T) {
	packs, err := ListPacks()
	if err != nil {
		t.Fatalf("ListPacks() error: %v", err)
	}

	if len(packs) == 0 {
		t.Fatal("ListPacks() returned no packs")
	}

	found := false
	for _, p := range packs {
		if p.Name == "jwt-auth" {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListPacks() did not include jwt-auth")
	}
}

func TestGetPack(t *testing.T) {
	p, err := GetPack("jwt-auth")
	if err != nil {
		t.Fatalf("GetPack(jwt-auth) error: %v", err)
	}
	if p.Name != "jwt-auth" {
		t.Errorf("Name = %q, want %q", p.Name, "jwt-auth")
	}
}

func TestGetPackInvalidName(t *testing.T) {
	_, err := GetPack("../etc/passwd")
	if err == nil {
		t.Fatal("GetPack(../etc/passwd) expected error, got nil")
	}
}

func TestValidPackName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"jwt-auth", true},
		{"my_pack", true},
		{"auth123", true},
		{"", false},
		{"-leading-hyphen", false},
		{"_leading-underscore", false},
		{"has spaces", false},
		{"has/slash", false},
		{"has.dot", false},
		{"UPPERCASE", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ValidPackName(tt.name)
			if got != tt.want {
				t.Errorf("ValidPackName(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestIsInstalled(t *testing.T) {
	cfg := &config.ProjectConfig{
		Packs: []string{"jwt-auth", "payments"},
	}

	if !IsInstalled("jwt-auth", cfg) {
		t.Error("IsInstalled(jwt-auth) = false, want true")
	}
	if IsInstalled("nonexistent", cfg) {
		t.Error("IsInstalled(nonexistent) = true, want false")
	}
}

func TestPackFileOverwrite(t *testing.T) {
	p, err := LoadPack("jwt-auth")
	if err != nil {
		t.Fatalf("LoadPack error: %v", err)
	}

	for _, f := range p.Files {
		switch f.Overwrite {
		case "always", "once", "never":
			// valid
		default:
			t.Errorf("File %s has invalid overwrite value %q", f.Template, f.Overwrite)
		}
	}
}
