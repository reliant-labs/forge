package cli

import (
	"reflect"
	"testing"
)

// A variant's declared env lands after forge's defaults, so os/exec (which
// keeps the last value of a duplicated key) lets it override CGO_ENABLED=0,
// and it is appended in a stable order.
func TestVariantBuildEnv_DeclaredEnvWinsInStableOrder(t *testing.T) {
	base := []string{"PATH=/bin", "CGO_ENABLED=0"}
	v := BuildVariant{
		Name: "darwin", GOOS: "darwin", GOARCH: "arm64",
		EnvAtBuild: map[string]string{"GOFLAGS": "-trimpath", "CGO_ENABLED": "1"},
	}
	got := variantBuildEnv(base, v)
	want := []string{"PATH=/bin", "CGO_ENABLED=0", "GOOS=darwin", "GOARCH=arm64", "CGO_ENABLED=1", "GOFLAGS=-trimpath"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("env = %q\nwant %q", got, want)
	}
	if base[1] != "CGO_ENABLED=0" || len(base) != 2 {
		t.Errorf("base mutated: %q", base)
	}
}

func TestVariantOutputName(t *testing.T) {
	if got := variantOutputName("cli", BuildVariant{Name: "linux"}); got != "cli-linux" {
		t.Errorf("default = %q, want cli-linux", got)
	}
	if got := variantOutputName("cli", BuildVariant{Name: "linux", OutputName: "acme"}); got != "acme" {
		t.Errorf("override = %q, want acme", got)
	}
}
