// Package imagebuild assembles an OCI image from a Dockerfile's final stage
// without a container runtime.
//
// This is the ko/Jib model: forge has already compiled the Go binaries for the
// target platform, so the image is the pinned base image plus a layer holding
// those binaries, plus the stage's config (entrypoint, user, env, ports,
// healthcheck). No daemon is involved; the base is pulled from, and the result
// pushed to, a registry with the docker credential store's credentials.
//
// It is NOT a Dockerfile executor. It models the instructions a forge
// Dockerfile's final stage is made of and rejects the rest with the line that
// needs a real builder (UnsupportedError), so it can never ship an image that
// silently differs from what `docker build` would have produced.
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
	"github.com/moby/patternmatcher"
)

// Options describes one image to assemble.
type Options struct {
	// Dockerfile is the file's content. Target names the stage to build; empty
	// builds the last one, as `docker build` does.
	Dockerfile []byte
	Target     string
	BuildArgs  map[string]string

	// Platform is the image platform. The base is pulled for it.
	Platform v1.Platform

	// ContextDir is the build context root; COPY sources without --from read
	// from it, minus what its .dockerignore excludes.
	ContextDir string

	// Artifacts maps a build stage's name to the files that stage produces, as
	// absolute in-stage path -> host path. This is how `COPY --from=builder
	// /app/bin/app ./app` reaches the binary forge already compiled. A path
	// that is neither an artifact nor a context copy of that stage is an error.
	Artifacts map[string]map[string]string

	// Created stamps the config and every history entry. The zero value is the
	// Unix epoch, which keeps the digest a function of the inputs.
	Created time.Time

	// Base replaces the FROM image (tests). Keychain authenticates the base
	// pull; nil means the docker credential store.
	Base     v1.Image
	Keychain authn.Keychain
}

// Result is an assembled image and what is worth telling the author about it.
type Result struct {
	Image v1.Image
	// Digest is the manifest digest the image will have in any registry.
	Digest v1.Hash
	// Base is the FROM reference and its resolved digest ("" for scratch).
	Base string
	// Notes records instructions that were satisfied rather than executed.
	Notes []string

	// Cleanup removes the temporary layer files the image's layers are read
	// from. Call it once the image has been pushed; it is safe to call twice.
	Cleanup func()

	cleanup []func()
}

func (r *Result) release() {
	for _, f := range r.cleanup {
		f()
	}
	r.cleanup = nil
}

// Check reports whether Build can assemble df's stage, without touching a
// registry or the filesystem. It is what chooses between this builder and a
// docker daemon. The base image's own ENV is unknown here, so a variable that
// only the base defines is not resolved.
func Check(dockerfile []byte, target string, buildArgs map[string]string, artifacts map[string]map[string]string) error {
	df, err := parseDockerfile(dockerfile)
	if err != nil {
		return err
	}
	st, err := df.selectStage(target)
	if err != nil {
		return err
	}
	ev, err := evaluateStage(df, st, v1.Config{}, buildArgs, df.globalVars(buildArgs))
	if err != nil {
		return err
	}
	for _, op := range ev.ops {
		if op.kind == opCopy && op.from != "" {
			if err := checkStageSources(op, artifacts); err != nil {
				return &UnsupportedError{Line: op.line, Stage: st.name, What: err.Error()}
			}
		}
	}
	return nil
}

func checkStageSources(op layerOp, artifacts map[string]map[string]string) error {
	for _, src := range op.srcs {
		abs := joinAbs("/", src)
		if _, ok := lookupArtifact(artifacts[op.from], strings.TrimSuffix(abs, "/")); ok {
			continue
		}
		return fmt.Errorf("COPY --from=%s %s: %s is not a binary forge's host build produces, so only a Docker daemon can build this stage", op.from, src, abs)
	}
	return nil
}

// Build assembles the image. On error nothing is left on disk; on success the
// caller owns Result.Cleanup.
func Build(ctx context.Context, o Options) (*Result, error) {
	res := &Result{}
	res.Cleanup = res.release
	img, err := build(ctx, o, res)
	if err != nil {
		res.release()
		return nil, err
	}
	res.Image = img
	return res, nil
}

// build does Build's work, accumulating temp files on res so the caller can
// remove them if it fails.
func build(ctx context.Context, o Options, res *Result) (v1.Image, error) {
	df, err := parseDockerfile(o.Dockerfile)
	if err != nil {
		return nil, err
	}
	st, err := df.selectStage(o.Target)
	if err != nil {
		return nil, err
	}
	global := df.globalVars(o.BuildArgs)

	base, err := resolveBase(ctx, df, st, global, o, res)
	if err != nil {
		return nil, err
	}
	baseCF, err := base.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("read base image config: %w", err)
	}
	ev, err := evaluateStage(df, st, baseCF.Config, o.BuildArgs, global)
	if err != nil {
		return nil, err
	}
	res.Notes = ev.notes

	created := o.Created.UTC()
	if o.Created.IsZero() {
		created = time.Unix(0, 0).UTC()
	}
	adds, err := applyOps(st, ev, base, o, created, res)
	if err != nil {
		return nil, err
	}

	img := base
	if len(adds) > 0 {
		if img, err = mutate.Append(img, adds...); err != nil {
			return nil, fmt.Errorf("append layers: %w", err)
		}
	}
	cf, err := img.ConfigFile()
	if err != nil {
		return nil, err
	}
	cf = cf.DeepCopy()
	cf.Config = ev.config
	cf.Created = v1.Time{Time: created}
	cf.OS, cf.Architecture = o.Platform.OS, o.Platform.Architecture
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		return nil, err
	}
	if res.Digest, err = img.Digest(); err != nil {
		return nil, err
	}
	return img, nil
}

// resolveBase is the stage's FROM image: the one Options.Base names, else the
// FROM reference pulled for the target platform.
func resolveBase(ctx context.Context, df *dockerfile, st *stage, global map[string]string, o Options, res *Result) (v1.Image, error) {
	base := o.Base
	if base == nil {
		ref := expandVars(st.base, global)
		if ref == "" {
			return nil, &UnsupportedError{Line: st.line, Stage: st.name, What: "FROM " + st.base + " expands to nothing"}
		}
		if df.stageNamed(ref) != nil {
			return nil, &UnsupportedError{Line: st.line, Stage: st.name, What: "FROM " + ref + " (a stage of this Dockerfile) is not supported for the final stage"}
		}
		var err error
		if base, err = pullBase(ctx, ref, o); err != nil {
			return nil, err
		}
		res.Base = ref
		if d, derr := base.Digest(); derr == nil {
			res.Base = ref + " (" + d.String() + ")"
		}
	}
	if err := checkPlatform(base, o.Platform); err != nil {
		return nil, err
	}
	return base, nil
}

// applyOps turns the stage's ops into layer and history additions over base.
func applyOps(st *stage, ev *evaluated, base v1.Image, o Options, created time.Time, res *Result) ([]mutate.Addendum, error) {
	idx := &fsIndex{img: base}
	ignore, err := loadIgnore(o.ContextDir)
	if err != nil {
		return nil, err
	}
	mt := layerMediaType(base)
	var adds []mutate.Addendum
	hist := func(by string, empty bool) v1.History {
		return v1.History{Created: v1.Time{Time: created}, CreatedBy: by, EmptyLayer: empty}
	}
	emptyHist := func(by string) { adds = append(adds, mutate.Addendum{History: hist(by, true)}) }

	pendingDirs := map[string]bool{}
	for _, op := range ev.ops {
		switch op.kind {
		case opRequireFile:
			ok, err := idx.has(op.path)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, &UnsupportedError{Line: op.line, Stage: st.name, What: fmt.Sprintf("RUN installs ca-certificates, but the base image has no /%s and installing it needs a Docker daemon", op.path)}
			}
			emptyHist("RUN apk add ca-certificates # not executed: the base image ships the CA bundle")

		case opMeta:
			emptyHist(op.history)

		case opMkdir:
			for _, d := range op.dirs {
				exists, err := idx.isDir(d)
				if err != nil {
					return nil, err
				}
				if !exists {
					pendingDirs[d] = true
				}
			}
			emptyHist(op.history)

		case opCopy:
			entries, err := resolveCopy(st, op, o, ignore, idx, pendingDirs)
			if err != nil {
				return nil, err
			}
			layer, cleanup, err := writeLayer(entries, created, mt)
			if err != nil {
				return nil, err
			}
			res.cleanup = append(res.cleanup, cleanup)
			for _, e := range entries {
				idx.add(e.name, e.dir)
			}
			adds = append(adds, mutate.Addendum{Layer: layer, History: hist(op.history, false)})
		}
	}
	return adds, nil
}

func pullBase(ctx context.Context, ref string, o Options) (v1.Image, error) {
	if ref == "scratch" {
		img := empty.Image
		return mutate.ConfigFile(img, &v1.ConfigFile{OS: o.Platform.OS, Architecture: o.Platform.Architecture, RootFS: v1.RootFS{Type: "layers"}})
	}
	r, err := name.ParseReference(ref)
	if err != nil {
		return nil, fmt.Errorf("base image %q: %w", ref, err)
	}
	kc := o.Keychain
	if kc == nil {
		kc = authn.DefaultKeychain
	}
	p := o.Platform
	img, err := remote.Image(r, remote.WithContext(ctx), remote.WithAuthFromKeychain(kc), remote.WithPlatform(p))
	if err != nil {
		return nil, fmt.Errorf("pull base image %s for %s: %w", ref, platformString(p), err)
	}
	return img, nil
}

func platformString(p v1.Platform) string { return p.OS + "/" + p.Architecture }

func checkPlatform(base v1.Image, want v1.Platform) error {
	cf, err := base.ConfigFile()
	if err != nil {
		return err
	}
	if cf.OS != "" && cf.Architecture != "" && (cf.OS != want.OS || cf.Architecture != want.Architecture) {
		return fmt.Errorf("base image is %s/%s, not %s", cf.OS, cf.Architecture, platformString(want))
	}
	return nil
}

// layerMediaType follows the base image's manifest family so the result is one
// coherent manifest, never docker-schema2 config with OCI layers.
func layerMediaType(base v1.Image) types.MediaType {
	if mt, err := base.MediaType(); err == nil && mt == types.DockerManifestSchema2 {
		return types.DockerLayer
	}
	return types.OCILayer
}

// ---- layer entries ----------------------------------------------------------

type entry struct {
	name   string // path in the image, no leading slash, no trailing slash
	dir    bool
	mode   int64
	src    string // host path for a regular file
	link   string // symlink target
	isLink bool
	size   int64
}

func resolveCopy(st *stage, op layerOp, o Options, ignore *patternmatcher.PatternMatcher, idx *fsIndex, pendingDirs map[string]bool) ([]entry, error) {
	var out []entry
	seen := map[string]bool{}
	add := func(e entry) {
		if !seen[e.name] {
			seen[e.name] = true
			out = append(out, e)
		}
	}
	destDirExists, err := idx.isDir(strings.TrimSuffix(op.dest, "/"))
	if err != nil {
		return nil, err
	}
	destIsDir := strings.HasSuffix(op.dest, "/") || destDirExists
	destPath := strings.TrimSuffix(op.dest, "/")

	var files []copied
	for _, src := range op.srcs {
		got, err := sourceFiles(op, src, o, ignore)
		if err != nil {
			return nil, &UnsupportedError{Line: op.line, Stage: st.name, What: err.Error()}
		}
		files = append(files, got...)
	}
	if len(files) == 0 {
		return nil, &UnsupportedError{Line: op.line, Stage: st.name, What: "COPY " + strings.Join(op.srcs, " ") + " matches no files"}
	}
	for _, f := range files {
		target := destPath
		switch {
		case f.rel != "":
			target = path.Join(destPath, f.rel)
		case destIsDir:
			target = path.Join(destPath, f.base)
		}
		e := f.entry
		e.name = strings.TrimPrefix(target, "/")
		parents := parentDirs(e.name)
		for _, p := range parents {
			if seen[p] {
				continue
			}
			exists, err := idx.isDir("/" + p)
			if err != nil {
				return nil, err
			}
			if exists && !pendingDirs["/"+p] {
				continue
			}
			add(entry{name: p, dir: true, mode: 0o755})
		}
		add(e)
	}
	for d := range pendingDirs {
		delete(pendingDirs, d)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

func parentDirs(name string) []string {
	var out []string
	for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
		out = append([]string{d}, out...)
	}
	return out
}

type copied struct {
	entry
	rel  string // path relative to a copied directory, "" for a single file
	base string
}

// sourceFiles resolves one COPY source to host files.
func sourceFiles(op layerOp, src string, o Options, ignore *patternmatcher.PatternMatcher) ([]copied, error) {
	if op.from != "" {
		abs := joinAbs("/", src)
		p := strings.TrimSuffix(abs, "/")
		if host, ok := lookupArtifact(o.Artifacts[op.from], p); ok {
			return walkHost(host, "", ignore)
		}
		return nil, fmt.Errorf("COPY --from=%s %s: %s is not a binary forge's host build produces, so only a Docker daemon can build this stage", op.from, src, abs)
	}
	return contextFiles(o.ContextDir, src, ignore)
}

func contextFiles(ctxDir, src string, ignore *patternmatcher.PatternMatcher) ([]copied, error) {
	if ctxDir == "" {
		return nil, errors.New("no build context directory")
	}
	rel := path.Clean(strings.TrimPrefix(filepath.ToSlash(src), "/"))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, fmt.Errorf("COPY source %q is outside the build context", src)
	}
	matches := []string{rel}
	if strings.ContainsAny(rel, "*?[") {
		m, err := filepath.Glob(filepath.Join(ctxDir, filepath.FromSlash(rel)))
		if err != nil {
			return nil, fmt.Errorf("COPY source %q: %v", src, err)
		}
		matches = matches[:0]
		for _, h := range m {
			r, err := filepath.Rel(ctxDir, h)
			if err != nil {
				return nil, err
			}
			matches = append(matches, filepath.ToSlash(r))
		}
		sort.Strings(matches)
	}
	var out []copied
	for _, m := range matches {
		if rel != "." {
			if ignored, err := ignore.MatchesOrParentMatches(m); err != nil {
				return nil, err
			} else if ignored {
				return nil, fmt.Errorf("COPY source %q is excluded by .dockerignore", src)
			}
		}
		files, err := walkHost(filepath.Join(ctxDir, filepath.FromSlash(m)), m, ignore)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("COPY source %q not found in the build context", src)
			}
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

// walkHost lists host (a file or a directory). A directory yields its contents
// with paths relative to it; ctxRel, when set, is host's path inside the build
// context so .dockerignore applies to each file.
func walkHost(host, ctxRel string, ignore *patternmatcher.PatternMatcher) ([]copied, error) {
	fi, err := os.Lstat(host)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		e, err := fileEntry(host, fi)
		if err != nil {
			return nil, err
		}
		return []copied{{entry: e, base: filepath.Base(host)}}, nil
	}
	out := []copied{{entry: entry{dir: true, mode: int64(fi.Mode().Perm())}, rel: "."}}
	err = filepath.WalkDir(host, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(host, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if ctxRel != "" {
			full := rel
			if ctxRel != "." {
				full = path.Join(ctxRel, rel)
			}
			if ig, err := ignore.MatchesOrParentMatches(full); err != nil {
				return err
			} else if ig {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			out = append(out, copied{entry: entry{dir: true, mode: int64(fi.Mode().Perm())}, rel: rel})
			return nil
		}
		e, err := fileEntry(p, fi)
		if err != nil {
			return err
		}
		out = append(out, copied{entry: e, rel: rel})
		return nil
	})
	return out, err
}

func fileEntry(host string, fi fs.FileInfo) (entry, error) {
	if fi.Mode()&fs.ModeSymlink != 0 {
		t, err := os.Readlink(host)
		if err != nil {
			return entry{}, err
		}
		return entry{isLink: true, link: t, mode: 0o777}, nil
	}
	if !fi.Mode().IsRegular() {
		return entry{}, fmt.Errorf("%s is not a regular file", host)
	}
	return entry{src: host, mode: int64(fi.Mode().Perm()), size: fi.Size()}, nil
}

func lookupArtifact(artifacts map[string]string, abs string) (string, bool) {
	if h, ok := artifacts[abs]; ok {
		return h, true
	}
	return "", false
}

func loadIgnore(ctxDir string) (*patternmatcher.PatternMatcher, error) {
	var patterns []string
	if ctxDir != "" {
		b, err := os.ReadFile(filepath.Join(ctxDir, ".dockerignore"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			patterns = append(patterns, line)
		}
	}
	return patternmatcher.New(patterns)
}

// writeLayer writes entries as one deterministic tar layer: sorted, root-owned,
// every timestamp the build epoch, no extended attributes.
func writeLayer(entries []entry, created time.Time, mt types.MediaType) (v1.Layer, func(), error) {
	f, err := os.CreateTemp("", "forge-layer-*.tar")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	fail := func(err error) (v1.Layer, func(), error) {
		_ = f.Close()
		cleanup()
		return nil, nil, err
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, ModTime: created, Mode: e.mode, Format: tar.FormatPAX}
		switch {
		case e.dir:
			h.Typeflag, h.Name = tar.TypeDir, e.name+"/"
		case e.isLink:
			h.Typeflag, h.Linkname = tar.TypeSymlink, e.link
		default:
			h.Typeflag, h.Size = tar.TypeReg, e.size
		}
		if err := tw.WriteHeader(h); err != nil {
			return fail(err)
		}
		if e.dir || e.isLink {
			continue
		}
		src, err := os.Open(e.src)
		if err != nil {
			return fail(err)
		}
		n, err := io.Copy(tw, src)
		_ = src.Close()
		if err != nil {
			return fail(err)
		}
		if n != e.size {
			return fail(fmt.Errorf("%s changed while the layer was being written", e.src))
		}
	}
	if err := tw.Close(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	layer, err := tarball.LayerFromFile(f.Name(), tarball.WithMediaType(mt), tarball.WithCompressedCaching)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return layer, cleanup, nil
}

// ---- base image filesystem index -------------------------------------------

// fsIndex answers "does this path exist in the image so far" from the base
// image's layers plus what this build added. The base is only read when a
// question needs it, because most builds never ask.
type fsIndex struct {
	img    v1.Image
	loaded bool
	paths  map[string]bool // path -> is a directory
}

func (x *fsIndex) load() error {
	if x.loaded {
		return nil
	}
	x.loaded = true
	if x.paths == nil {
		x.paths = map[string]bool{}
	}
	layers, err := x.img.Layers()
	if err != nil {
		return err
	}
	for _, l := range layers {
		rc, err := l.Uncompressed()
		if err != nil {
			return err
		}
		tr := tar.NewReader(rc)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				_ = rc.Close()
				return err
			}
			p := strings.TrimSuffix(path.Clean("/"+h.Name), "/")
			base := path.Base(p)
			switch {
			case base == ".wh..wh..opq":
				dir := path.Dir(p)
				for k := range x.paths {
					if strings.HasPrefix(k, dir+"/") {
						delete(x.paths, k)
					}
				}
			case strings.HasPrefix(base, ".wh."):
				victim := path.Join(path.Dir(p), strings.TrimPrefix(base, ".wh."))
				delete(x.paths, victim)
				for k := range x.paths {
					if strings.HasPrefix(k, victim+"/") {
						delete(x.paths, k)
					}
				}
			default:
				x.paths[p] = h.Typeflag == tar.TypeDir
			}
		}
		_ = rc.Close()
	}
	return nil
}

func (x *fsIndex) has(p string) (bool, error) {
	if err := x.load(); err != nil {
		return false, err
	}
	_, ok := x.paths[path.Clean("/"+p)]
	return ok, nil
}

func (x *fsIndex) isDir(p string) (bool, error) {
	if err := x.load(); err != nil {
		return false, err
	}
	p = path.Clean("/" + p)
	if p == "/" {
		return true, nil
	}
	return x.paths[p], nil
}

func (x *fsIndex) add(p string, dir bool) {
	if x.paths == nil {
		x.paths = map[string]bool{}
	}
	x.paths[path.Clean("/"+p)] = dir
}

// ---- push -------------------------------------------------------------------

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
