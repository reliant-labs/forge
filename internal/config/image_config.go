package config

import (
	"fmt"
	"path"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ImageConfig is the project image, declared: the `image:` block of
// forge.yaml.
//
// forge already knows most of what the image is. It compiled the binaries for
// the target platform (each GoBuild workload's output), and it knows where they
// go and which one is the entrypoint. What it cannot derive is the base the
// binaries run on and any non-Go files the process reads at runtime. This block
// states those, and forge assembles the image itself (internal/imagebuild): the
// base pulled by digest for the target platform, one layer of binaries, one of
// files, and this config, pushed straight to the workload's registry. It never
// reads a Dockerfile and needs no Docker daemon to push.
//
//	image:
//	  base: gcr.io/distroless/static-debian12@sha256:…
//	  cmd: [server]
//	  files:
//	    db/migrations: /app/db/migrations
//
// It is project-level, not per env, on purpose: one build is promoted by
// digest from env to env, so the recipe that made it must not vary between
// them.
//
// Absent, forge builds the project image from the root Dockerfile with
// `docker build`, exactly as before. That is the path for an image this model
// cannot express (a RUN step, a package install): forge does not interpret a
// Dockerfile, it hands it to docker.
type ImageConfig struct {
	// Base is the image the binaries are layered onto. Pin it by digest
	// (`ref@sha256:…`); a tag is resolved at build time, so two builds of one
	// commit can differ. "scratch" is an empty base. Required when the block
	// is present.
	Base string `yaml:"base,omitempty"`
	// Workdir is the image's working directory and where the binaries land
	// (`<workdir>/<output_name>`). Default /app.
	Workdir string `yaml:"workdir,omitempty"`
	// User is the image's user. Default 65532:65532, the nonroot user
	// distroless ships.
	User string `yaml:"user,omitempty"`
	// Entrypoint is the image's ENTRYPOINT. Default `[<workdir>/<binary>]`
	// for the project's binary (the one named after the project, or the only
	// one).
	Entrypoint []string `yaml:"entrypoint,omitempty"`
	// Cmd is the image's default CMD: the arguments a workload's `args`
	// replace. Empty means none (a base's CMD is never inherited under a new
	// entrypoint, as with docker).
	Cmd []string `yaml:"cmd,omitempty"`
	// Env is added to the base image's environment, replacing a variable of
	// the same name.
	Env map[string]string `yaml:"env,omitempty"`
	// Files maps a project-relative file or directory to its absolute path in
	// the image: what the process reads at runtime that is not compiled in.
	Files map[string]string `yaml:"files,omitempty"`
}

// Image defaults.
const (
	DefaultImageWorkdir = "/app"
	DefaultImageUser    = "65532:65532"
)

// Declared reports whether forge.yaml declares the project image.
func (c ImageConfig) Declared() bool {
	return c.Base != "" || c.Workdir != "" || c.User != "" || len(c.Entrypoint) > 0 ||
		len(c.Cmd) > 0 || len(c.Env) > 0 || len(c.Files) > 0
}

// EffectiveWorkdir is Workdir, defaulted.
func (c ImageConfig) EffectiveWorkdir() string {
	if c.Workdir == "" {
		return DefaultImageWorkdir
	}
	return c.Workdir
}

// EffectiveUser is User, defaulted.
func (c ImageConfig) EffectiveUser() string {
	if c.User == "" {
		return DefaultImageUser
	}
	return c.User
}

// validateImage checks the `image:` block. It runs only when the block is
// present: absent means the Dockerfile path, which has nothing to validate
// here.
func validateImage(cfg *ProjectConfig, root *yaml.Node) []validationIssue {
	c := cfg.Image
	if !c.Declared() {
		return nil
	}
	var out []validationIssue
	issue := func(keys []string, msg, fix string) {
		line, col := findNodePos(root, keys)
		out = append(out, validationIssue{line: line, column: col, msg: msg, fix: fix})
	}
	if strings.TrimSpace(c.Base) == "" {
		issue([]string{"image"}, "image.base is required when the image block is present",
			"name the base image, pinned by digest: base: gcr.io/distroless/static-debian12@sha256:<digest> (or delete the image block to build the root Dockerfile with docker).")
	}
	if c.Workdir != "" && !path.IsAbs(c.Workdir) {
		issue([]string{"image", "workdir"}, fmt.Sprintf("image.workdir %q is not an absolute path", c.Workdir),
			"use an absolute in-image path such as /app.")
	}
	srcs := make([]string, 0, len(c.Files))
	for src := range c.Files {
		srcs = append(srcs, src)
	}
	sort.Strings(srcs)
	for _, src := range srcs {
		dest := c.Files[src]
		clean := path.Clean(src)
		if src == "" || path.IsAbs(src) || clean == ".." || strings.HasPrefix(clean, "../") {
			issue([]string{"image", "files", src}, fmt.Sprintf("image.files source %q is not a path inside the project", src),
				"use a project-relative path such as db/migrations.")
		}
		if !path.IsAbs(dest) {
			issue([]string{"image", "files", src}, fmt.Sprintf("image.files destination %q (for %s) is not an absolute path", dest, src),
				"use an absolute in-image path such as /app/db/migrations.")
		}
	}
	return out
}
