package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/observe"
)

// ManifestSource reads a channel manifest: one JSON document naming each
// channel's current release and where its files are (docs/proposals/
// self-upgrade.md §6.3). Its format, schema 1, is the one reliant publishes as
// daemon-channels.json (reliant dev-docs/cli-release-artifacts.md):
//
//	{
//	  "schema": 1,
//	  "channels": {
//	    "<channel>": {
//	      "version": "1.9.0", "tag": "v1.9.0", "commit": "<sha>",
//	      "commit_time_unix": 1763500000,
//	      "checksums": {"url": "https://…/checksums.txt", "sha256": "<hex>"},
//	      "bundle":    {"url": "https://…/<name>.sigstore.json"},
//	      "platforms": {"linux/amd64": {"url": "https://…/x_linux_amd64.tar.gz", "sha256": "<hex>"}, …}
//	    }
//	  }
//	}
//
// The manifest names the target; it is not what makes an install safe. Each
// asset's name is the last element of its URL, and the [Verifier] checks the
// files themselves against the release's bundle, so a manifest that points at
// another release's files fails verification. A manifest held back to an older
// release is not an install either: [Updater.Check] never moves backwards.
type ManifestSource struct {
	// URL is the manifest's https URL.
	URL string
	// Client fetches it; default an observe.NewHTTPClient with a 30s timeout.
	Client *http.Client
}

// ManifestSchema is the manifest schema ManifestSource reads.
const ManifestSchema = 1

// maxManifestBytes bounds the manifest read.
const maxManifestBytes = 1 << 20

type manifestFile struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
}

type manifestChannel struct {
	Version        string                  `json:"version"`
	Tag            string                  `json:"tag"`
	Commit         string                  `json:"commit"`
	CommitTimeUnix int64                   `json:"commit_time_unix"`
	Checksums      manifestFile            `json:"checksums"`
	Bundle         manifestFile            `json:"bundle"`
	Platforms      map[string]manifestFile `json:"platforms"`
}

type manifestDoc struct {
	Schema   int                        `json:"schema"`
	Channels map[string]manifestChannel `json:"channels"`
}

// Latest returns the release the manifest names for channel.
func (m ManifestSource) Latest(ctx context.Context, channel string) (Release, error) {
	doc, err := m.fetch(ctx)
	if err != nil {
		return Release{}, err
	}
	return doc.release(channel)
}

func (m ManifestSource) fetch(ctx context.Context) (manifestDoc, error) {
	u, err := url.Parse(m.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return manifestDoc{}, fmt.Errorf("selfupdate: ManifestSource.URL must be an https URL with no userinfo, got %q", m.URL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return manifestDoc{}, err
	}
	req.Header.Set("Accept", "application/json")
	// The manifest is the one object that changes in place; never take a
	// cached copy from an intermediary.
	req.Header.Set("Cache-Control", "no-cache")
	client := m.Client
	if client == nil {
		client = observe.NewHTTPClient(observe.HTTPClientOptions{Timeout: 30 * time.Second})
	}
	resp, err := httpsOnly(client).Do(req)
	if err != nil {
		return manifestDoc{}, fmt.Errorf("selfupdate: channel manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return manifestDoc{}, fmt.Errorf("selfupdate: channel manifest %s: %s", u.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return manifestDoc{}, fmt.Errorf("selfupdate: channel manifest: %w", err)
	}
	if len(body) > maxManifestBytes {
		return manifestDoc{}, fmt.Errorf("selfupdate: channel manifest is larger than %d bytes", maxManifestBytes)
	}
	return parseManifest(body)
}

func parseManifest(body []byte) (manifestDoc, error) {
	var doc manifestDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return manifestDoc{}, fmt.Errorf("selfupdate: channel manifest: decode: %w", err)
	}
	if doc.Schema != ManifestSchema {
		return manifestDoc{}, fmt.Errorf("selfupdate: channel manifest has schema %d; this reader knows schema %d", doc.Schema, ManifestSchema)
	}
	return doc, nil
}

func (d manifestDoc) release(channel string) (Release, error) {
	ch, ok := d.Channels[channel]
	if !ok {
		return Release{}, fmt.Errorf("selfupdate: the channel manifest has no channel %q", channel)
	}
	if ch.Tag == "" {
		return Release{}, fmt.Errorf("selfupdate: channel %q names no tag", channel)
	}
	rel := Release{Version: ch.Version, Tag: ch.Tag, Channel: channel}
	if ch.CommitTimeUnix > 0 {
		rel.CommitTime = time.Unix(ch.CommitTimeUnix, 0).UTC()
	}
	seen := map[string]bool{}
	asset := func(what string, f manifestFile) (Asset, error) {
		u, err := url.Parse(f.URL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return Asset{}, fmt.Errorf("selfupdate: channel %q: %s URL %q is not https", channel, what, f.URL)
		}
		name := path.Base(u.Path)
		if !safeName(name) {
			return Asset{}, fmt.Errorf("selfupdate: channel %q: %s URL %q names no file", channel, what, f.URL)
		}
		if seen[name] {
			return Asset{}, fmt.Errorf("selfupdate: channel %q: two files are named %s", channel, name)
		}
		seen[name] = true
		return Asset{Name: name, URL: f.URL}, nil
	}
	sums, err := asset("checksums", ch.Checksums)
	if err != nil {
		return Release{}, err
	}
	rel.Assets = append(rel.Assets, sums)
	if ch.Bundle.URL != "" {
		if rel.Bundle, err = asset("bundle", ch.Bundle); err != nil {
			return Release{}, err
		}
	}
	if len(ch.Platforms) == 0 {
		return Release{}, fmt.Errorf("selfupdate: channel %q has no platforms", channel)
	}
	for _, p := range slices.Sorted(maps.Keys(ch.Platforms)) {
		if !strings.Contains(p, "/") {
			return Release{}, errors.New("selfupdate: channel " + channel + ": platform " + p + " is not goos/goarch")
		}
		a, err := asset(p, ch.Platforms[p])
		if err != nil {
			return Release{}, err
		}
		rel.Assets = append(rel.Assets, a)
	}
	return rel, nil
}
