// Package imagebuild assembles the project image from forge's own build model,
// without a Dockerfile or a container runtime.
//
// This is the ko model. forge has already cross-compiled the Go binaries for
// the target platform, so the image is a pinned base image, one layer holding
// those binaries, one layer holding any declared runtime files, and a config
// (entrypoint, cmd, user, workdir, env). Every input is a value forge holds
// (forge.yaml's `image:` block plus the GoBuild outputs); nothing is parsed
// out of a Dockerfile. The base is pulled from, and the result pushed to, a
// registry with the docker credential store's credentials, so no daemon is
// involved.
//
// The output is reproducible: entries are sorted and root-owned, every
// timestamp is Spec.Created, and the config carries no wall-clock time. The
// same base, binaries and files give the same digest.
package imagebuild

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// Spec is one image to assemble.
type Spec struct {
	// Base is the base image reference ("scratch" for none). BaseImage, when
	// set, replaces the pull (tests).
	Base      string
	BaseImage v1.Image
	// Platform is the image platform; the base is pulled for it.
	Platform v1.Platform

	Workdir    string
	User       string
	Entrypoint []string
	// Cmd is the default CMD. nil clears whatever the base declared, as a
	// Dockerfile ENTRYPOINT does.
	Cmd []string
	// Env is merged over the base's environment, replacing a variable of the
	// same name.
	Env map[string]string

	// Binaries maps an absolute in-image path to the host file forge built.
	Binaries map[string]string
	// Files maps an absolute in-image path to a host file or directory.
	Files map[string]string

	// Created stamps the config, the history and every layer entry. The zero
	// value is the Unix epoch.
	Created time.Time

	// Keychain authenticates the base pull; nil is the docker credential
	// store.
	Keychain authn.Keychain
}

// Result is an assembled image.
type Result struct {
	Image v1.Image
	// Digest is the manifest digest the image has in any registry.
	Digest v1.Hash
	// Base is the base reference and the digest it resolved to ("scratch"
	// for none).
	Base string
}

// Build assembles the image described by s.
func Build(ctx context.Context, s Spec) (*Result, error) {
	created := s.Created.UTC()
	if s.Created.IsZero() {
		created = time.Unix(0, 0).UTC()
	}
	base, baseLabel, err := resolveBase(ctx, s)
	if err != nil {
		return nil, err
	}
	mt := layerMediaType(base)

	var adds []mutate.Addendum
	for _, l := range []struct {
		what    string
		entries map[string]string
	}{{"binaries", s.Binaries}, {"files", s.Files}} {
		if len(l.entries) == 0 {
			continue
		}
		entries, err := layerEntries(l.entries)
		if err != nil {
			return nil, err
		}
		layer, err := tarLayer(entries, created, mt)
		if err != nil {
			return nil, err
		}
		adds = append(adds, mutate.Addendum{
			Layer:   layer,
			History: v1.History{Created: v1.Time{Time: created}, CreatedBy: "forge: " + l.what + " " + strings.Join(sortedKeys(l.entries), " ")},
		})
	}

	img := base
	if len(adds) > 0 {
		if img, err = mutate.Append(img, adds...); err != nil {
			return nil, fmt.Errorf("append layers: %w", err)
		}
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read image config: %w", err)
	}
	cf = cf.DeepCopy()
	cf.Config = imageConfig(cf.Config, s)
	cf.Created = v1.Time{Time: created}
	cf.OS, cf.Architecture, cf.Variant = s.Platform.OS, s.Platform.Architecture, s.Platform.Variant
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		return nil, err
	}
	digest, err := img.Digest()
	if err != nil {
		return nil, err
	}
	return &Result{Image: img, Digest: digest, Base: baseLabel}, nil
}

// imageConfig is the base's config with the spec applied over it.
func imageConfig(c v1.Config, s Spec) v1.Config {
	c.WorkingDir = s.Workdir
	c.User = s.User
	if len(s.Entrypoint) > 0 {
		c.Entrypoint = append([]string(nil), s.Entrypoint...)
		// A new entrypoint never inherits the base's CMD: it was written
		// for the base's own entrypoint.
		c.Cmd = nil
	}
	if s.Cmd != nil {
		c.Cmd = append([]string(nil), s.Cmd...)
	}
	// ArgsEscaped is a Windows-only legacy flag; a base that carries it
	// would otherwise leak it onto an entrypoint it never described.
	c.ArgsEscaped = false
	for _, k := range sortedKeys(s.Env) {
		kv := k + "=" + s.Env[k]
		replaced := false
		for i, e := range c.Env {
			if strings.HasPrefix(e, k+"=") {
				c.Env[i], replaced = kv, true
				break
			}
		}
		if !replaced {
			c.Env = append(c.Env, kv)
		}
	}
	return c
}

func resolveBase(ctx context.Context, s Spec) (v1.Image, string, error) {
	if s.BaseImage != nil {
		if err := checkPlatform(s.BaseImage, s.Platform); err != nil {
			return nil, "", err
		}
		return s.BaseImage, s.Base, nil
	}
	if s.Base == "" {
		return nil, "", errors.New("no base image")
	}
	if s.Base == "scratch" {
		img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
			OS: s.Platform.OS, Architecture: s.Platform.Architecture, RootFS: v1.RootFS{Type: "layers"},
		})
		return img, "scratch", err
	}
	ref, err := name.ParseReference(s.Base)
	if err != nil {
		return nil, "", fmt.Errorf("base image %q: %w", s.Base, err)
	}
	kc := s.Keychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(kc), remote.WithPlatform(s.Platform))
	if err != nil {
		return nil, "", fmt.Errorf("pull base image %s for %s: %w", s.Base, PlatformString(s.Platform), err)
	}
	if err := checkPlatform(img, s.Platform); err != nil {
		return nil, "", fmt.Errorf("base image %s: %w", s.Base, err)
	}
	label := s.Base
	if _, pinned := ref.(name.Digest); !pinned {
		if d, err := img.Digest(); err == nil {
			label += " (resolved to " + d.String() + "; pin it by digest for a reproducible build)"
		}
	}
	return img, label, nil
}

// PlatformString renders p as os/arch[/variant].
func PlatformString(p v1.Platform) string {
	s := p.OS + "/" + p.Architecture
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

func checkPlatform(base v1.Image, want v1.Platform) error {
	cf, err := base.ConfigFile()
	if err != nil {
		return err
	}
	if cf.OS != "" && cf.Architecture != "" && (cf.OS != want.OS || cf.Architecture != want.Architecture) {
		return fmt.Errorf("base image is %s/%s, not %s", cf.OS, cf.Architecture, PlatformString(want))
	}
	return nil
}

// layerMediaType follows the base image's manifest family, so the result is one
// coherent manifest and never docker-schema2 config with OCI layers.
func layerMediaType(base v1.Image) types.MediaType {
	if mt, err := base.MediaType(); err == nil && mt == types.DockerManifestSchema2 {
		return types.DockerLayer
	}
	return types.OCILayer
}

// ---- layer entries ----------------------------------------------------------

type entry struct {
	name string // in-image path, no leading or trailing slash
	dir  bool
	link string // symlink target; set for a symlink
	src  string // host path of a regular file
	mode int64
	size int64
}

// layerEntries lists every tar entry for files (in-image path -> host path),
// with each parent directory, sorted by name.
func layerEntries(files map[string]string) ([]entry, error) {
	byName := map[string]entry{}
	addParents := func(name string) {
		for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
			if _, ok := byName[d]; !ok {
				byName[d] = entry{name: d, dir: true, mode: 0o755}
			}
		}
	}
	for _, dest := range sortedKeys(files) {
		host := files[dest]
		if !path.IsAbs(dest) {
			return nil, fmt.Errorf("in-image path %q is not absolute", dest)
		}
		root := strings.TrimPrefix(path.Clean(dest), "/")
		if root == "" {
			return nil, fmt.Errorf("%s cannot be copied onto the image root", host)
		}
		err := filepath.WalkDir(host, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(host, p)
			if err != nil {
				return err
			}
			name := root
			if rel != "." {
				name = path.Join(root, filepath.ToSlash(rel))
			}
			fi, err := d.Info()
			if err != nil {
				return err
			}
			e, err := hostEntry(name, p, fi)
			if err != nil {
				return err
			}
			addParents(name)
			byName[name] = e
			return nil
		})
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s (for %s in the image) does not exist", host, dest)
			}
			return nil, err
		}
	}
	out := make([]entry, 0, len(byName))
	for _, e := range byName {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func hostEntry(name, host string, fi fs.FileInfo) (entry, error) {
	switch {
	case fi.IsDir():
		return entry{name: name, dir: true, mode: int64(fi.Mode().Perm())}, nil
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := os.Readlink(host)
		if err != nil {
			return entry{}, err
		}
		return entry{name: name, link: t, mode: 0o777}, nil
	case fi.Mode().IsRegular():
		return entry{name: name, src: host, mode: int64(fi.Mode().Perm()), size: fi.Size()}, nil
	}
	return entry{}, fmt.Errorf("%s is not a regular file, directory or symlink", host)
}

// tarLayer is entries as one deterministic layer: root-owned, every timestamp
// created, no extended attributes. The tar is streamed from the host files on
// each open, and the compressed bytes are cached after the first, so a layer
// is read from disk once for its digest and once for its upload at most.
func tarLayer(entries []entry, created time.Time, mt types.MediaType) (v1.Layer, error) {
	opener := func() (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() { pw.CloseWithError(writeTar(pw, entries, created)) }()
		return pr, nil
	}
	return tarball.LayerFromOpener(opener, tarball.WithMediaType(mt), tarball.WithCompressedCaching)
}

func writeTar(w io.Writer, entries []entry, created time.Time) error {
	tw := tar.NewWriter(w)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, ModTime: created, Mode: e.mode, Format: tar.FormatPAX}
		switch {
		case e.dir:
			h.Typeflag, h.Name = tar.TypeDir, e.name+"/"
		case e.link != "":
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, e.size
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		f, err := os.Open(e.src)
		if err != nil {
			return err
		}
		n, err := io.Copy(tw, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		if n != e.size {
			return fmt.Errorf("%s changed while the layer was being written", e.src)
		}
	}
	return tw.Close()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---- push and load ----------------------------------------------------------

// Push writes img to every reference in refs. Credentials come from keychain
// (nil: the docker credential store, which `docker login` and forge's own
// platform-registry login both write).
func Push(ctx context.Context, img v1.Image, refs []string, keychain authn.Keychain) error {
	if keychain == nil {
		keychain = authn.DefaultKeychain
	}
	for _, s := range refs {
		ref, err := name.ParseReference(s)
		if err != nil {
			return fmt.Errorf("image reference %q: %w", s, err)
		}
		if err := remote.Write(ref, img, remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain)); err != nil {
			return fmt.Errorf("push %s: %w", s, err)
		}
	}
	return nil
}

// WriteTarball writes img, tagged with every reference in tags, as the
// `docker save` tarball `docker load` reads.
func WriteTarball(w io.Writer, img v1.Image, tags []string) error {
	refs := make(map[name.Reference]v1.Image, len(tags))
	for _, t := range tags {
		tag, err := name.NewTag(t)
		if err != nil {
			return fmt.Errorf("image tag %q: %w", t, err)
		}
		refs[tag] = img
	}
	return tarball.MultiRefWrite(refs, w)
}
