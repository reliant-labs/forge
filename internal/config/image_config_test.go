package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestImageConfigRoundTrip(t *testing.T) {
	cfg, err := LoadProject([]byte(`name: test
module_path: github.com/foo/bar
image:
  base: gcr.io/distroless/static-debian12@sha256:abc
  cmd: [public-api]
  env: {MODE: prod}
  files:
    db/migrations: /app/db/migrations
`), "t.yaml")
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	img := cfg.Image
	if !img.Declared() || img.Base != "gcr.io/distroless/static-debian12@sha256:abc" ||
		!reflect.DeepEqual(img.Cmd, []string{"public-api"}) || img.Env["MODE"] != "prod" ||
		img.Files["db/migrations"] != "/app/db/migrations" {
		t.Errorf("image = %+v", img)
	}
	if img.EffectiveWorkdir() != "/app" || img.EffectiveUser() != "65532:65532" {
		t.Errorf("defaults = %q %q", img.EffectiveWorkdir(), img.EffectiveUser())
	}
}

func TestImageConfigAbsentIsNotDeclared(t *testing.T) {
	cfg, err := LoadProject([]byte("name: test\nmodule_path: github.com/foo/bar\n"), "t.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image.Declared() {
		t.Error("no image block must leave the project image to the Dockerfile")
	}
}

func TestImageConfigValidation(t *testing.T) {
	for _, tc := range []struct {
		name, block, want string
	}{
		{"base is required", "  cmd: [server]\n", "image.base is required"},
		{"workdir is absolute", "  base: scratch\n  workdir: app\n", "image.workdir"},
		{"files source stays in the project", "  base: scratch\n  files:\n    ../secrets: /app/secrets\n", "not a path inside the project"},
		{"files destination is absolute", "  base: scratch\n  files:\n    db/migrations: db/migrations\n", "is not an absolute path"},
		{"unknown key", "  base: scratch\n  entrypont: [/app/x]\n", "entrypont"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadProject([]byte("name: test\nmodule_path: github.com/foo/bar\nimage:\n"+tc.block), "t.yaml")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
