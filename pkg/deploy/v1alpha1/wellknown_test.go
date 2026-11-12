package v1alpha1

import (
	"strings"
	"testing"
)

func TestValidateWellKnown(t *testing.T) {
	if err := ValidateWellKnown(nil); err != nil {
		t.Fatal(err)
	}
	ok := map[string]string{"apple-app-site-association": "{}", "assetlinks.json": "[]"}
	if err := ValidateWellKnown(ok); err != nil {
		t.Fatalf("legal files refused: %v", err)
	}
	for name, files := range map[string]map[string]string{
		"traversal":      {"../etc/passwd": "x"},
		"nested":         {"a/b": "x"},
		"hidden":         {".htaccess": "x"},
		"empty name":     {"": "x"},
		"dotdot inside":  {"a..b": "x"},
		"too big":        {"assetlinks.json": strings.Repeat("x", MaxWellKnownFileBytes+1)},
		"too many files": {"a": "", "b": "", "c": "", "d": "", "e": "", "f": "", "g": "", "h": "", "i": ""},
	} {
		if err := ValidateWellKnown(files); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (StaticSiteSpec{WellKnown: map[string]string{"../x": "y"}}).Validate(); err == nil {
		t.Error("StaticSiteSpec.Validate does not check wellKnown")
	}
}
