package deploytarget

// A hosted static site release carries a STAMP, so `forge env status` can
// witness what the live site actually serves (forge#2231).
//
// A static site has no process to ask, and on hosted static hosting forge
// cannot read the bucket: the control plane's origin serves it. What every
// visitor can read is the site itself, through the CDN, so the release says
// who it is in a file of its own, /_forge/site.json, and the manifest of the
// pushed artifact says the same thing in an annotation. Status fetches the
// first over HTTP and the second from the registry, and compares them.
//
// The stamp cannot name the release digest — the digest covers the stamp —
// so it names the TREE: a sha256 over every file of the site except the
// stamp, by path and content. The annotation is written in the same push, so
// the manifest digest (the release) still depends on the content alone, and
// an unchanged tree still pushes to an unchanged digest.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	// StaticSiteStampPath is where a hosted site release carries its
	// stamp, relative to the site root. It has an extension, so a host
	// that serves index.html for unknown paths answers 404 for a site
	// built without one rather than the page.
	StaticSiteStampPath = "_forge/site.json"
	// StaticSiteTreeAnnotation is the manifest annotation holding the same
	// tree digest the stamp holds.
	StaticSiteTreeAnnotation = "io.reliant.forge.static-site.tree"
	// staticSiteStampSchema names the stamp document's shape.
	staticSiteStampSchema = "forge.static-site-stamp/v1"
)

// StaticSiteStamp is the document at StaticSiteStampPath.
type StaticSiteStamp struct {
	Schema string `json:"schema"`
	// Tree is the site's tree digest ("sha256:<hex>").
	Tree string `json:"tree"`
}

// StampStaticSiteTree writes the stamp into an assembled site tree and
// returns its tree digest. A stamp already in the tree (one a project kept
// in public/, or an earlier build's) is not this tree's and is replaced.
func StampStaticSiteTree(dir string) (string, error) {
	stamp := filepath.Join(dir, filepath.FromSlash(StaticSiteStampPath))
	if err := os.Remove(stamp); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("stamp %s: %w", dir, err)
	}
	tree, err := staticSiteTreeDigest(dir)
	if err != nil {
		return "", err
	}
	doc, err := json.MarshalIndent(StaticSiteStamp{Schema: staticSiteStampSchema, Tree: tree}, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(stamp), 0o755); err != nil {
		return "", fmt.Errorf("stamp %s: %w", dir, err)
	}
	if err := os.WriteFile(stamp, append(doc, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("stamp %s: %w", dir, err)
	}
	return tree, nil
}

// staticSiteTreeDigest is sha256 over each regular file's slash path and the
// sha256 of its content, in path order: what the site serves, independent of
// how it is packed or compressed.
func staticSiteTreeDigest(dir string) (string, error) {
	type entry struct{ rel, sum string }
	var files []entry
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		files = append(files, entry{filepath.ToSlash(rel), hex.EncodeToString(sum[:])})
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", dir, err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%s\n", f.rel, f.sum)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// ReadStaticSiteTree reads the tree digest a pushed site release's manifest
// declares. "" with no error is a release pushed before forge stamped sites.
func ReadStaticSiteTree(ctx context.Context, repository, digest string) (string, error) {
	repo, err := newRegistryRepository("static site", repository)
	if err != nil {
		return "", err
	}
	_, rc, err := repo.FetchReference(ctx, digest)
	if err != nil {
		return "", fmt.Errorf("read the manifest of %s@%s: %w", repository, digest, err)
	}
	defer func() { _ = rc.Close() }()
	return staticSiteTreeOfManifest(rc)
}

func staticSiteTreeOfManifest(r io.Reader) (string, error) {
	var m ocispec.Manifest
	if err := json.NewDecoder(io.LimitReader(r, 4<<20)).Decode(&m); err != nil {
		return "", fmt.Errorf("decode static site manifest: %w", err)
	}
	return strings.TrimSpace(m.Annotations[StaticSiteTreeAnnotation]), nil
}

// ParseStaticSiteStamp reads a stamp document as the live site serves it.
func ParseStaticSiteStamp(body []byte) (StaticSiteStamp, error) {
	var s StaticSiteStamp
	if err := json.Unmarshal(body, &s); err != nil {
		return s, fmt.Errorf("the site's %s is not a forge stamp: %w", StaticSiteStampPath, err)
	}
	if s.Schema != staticSiteStampSchema || !strings.HasPrefix(s.Tree, "sha256:") {
		return s, fmt.Errorf("the site's %s is not a %s document", StaticSiteStampPath, staticSiteStampSchema)
	}
	return s, nil
}
