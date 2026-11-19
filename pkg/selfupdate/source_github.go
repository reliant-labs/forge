package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/reliant-labs/forge/pkg/observe"
)

// GitHubSource finds releases through the GitHub releases API.
//
// Channels: "" and "latest" are the newest non-prerelease release
// (/releases/latest); "prerelease" is the newest release of any kind, drafts
// excluded. Any other channel is an error.
type GitHubSource struct {
	// Repo is "owner/name".
	Repo string
	// Bundle names the release's signature asset, e.g.
	// "forge-release.sigstore.json"; it becomes [Release.Bundle].
	Bundle string
	// APIBaseURL defaults to https://api.github.com.
	APIBaseURL string
	// Token, optional, raises the API's rate limit. Never required for a
	// public repository.
	Token  string
	Client *http.Client
}

type ghRelease struct {
	TagName     string    `json:"tag_name"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// Latest returns the channel's newest release.
func (g GitHubSource) Latest(ctx context.Context, channel string) (Release, error) {
	if g.Repo == "" || strings.Count(g.Repo, "/") != 1 {
		return Release{}, fmt.Errorf("selfupdate: GitHubSource.Repo must be owner/name, got %q", g.Repo)
	}
	base := strings.TrimSuffix(g.APIBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	var rel ghRelease
	switch channel {
	case "", "latest":
		if err := g.get(ctx, base+"/repos/"+g.Repo+"/releases/latest", &rel); err != nil {
			return Release{}, err
		}
	case "prerelease":
		var list []ghRelease
		if err := g.get(ctx, base+"/repos/"+g.Repo+"/releases?per_page=30", &list); err != nil {
			return Release{}, err
		}
		found := false
		for _, r := range list {
			if r.Draft {
				continue
			}
			if !found || laterTag(r, rel) {
				rel, found = r, true
			}
		}
		if !found {
			return Release{}, fmt.Errorf("selfupdate: %s has no releases", g.Repo)
		}
	default:
		return Release{}, fmt.Errorf("selfupdate: GitHubSource has no channel %q (use latest or prerelease)", channel)
	}
	if rel.TagName == "" {
		return Release{}, errors.New("selfupdate: GitHub returned a release with no tag")
	}
	out := Release{
		Version: strings.TrimPrefix(rel.TagName, "v"),
		Tag:     rel.TagName,
		Channel: channel,
	}
	for _, a := range rel.Assets {
		asset := Asset{Name: a.Name, URL: a.URL, Size: a.Size}
		out.Assets = append(out.Assets, asset)
		if g.Bundle != "" && a.Name == g.Bundle {
			out.Bundle = asset
		}
	}
	return out, nil
}

// laterTag orders two releases by semver tag, falling back to publish time.
func laterTag(a, b ghRelease) bool {
	ca, cb := canonicalVersion(a.TagName), canonicalVersion(b.TagName)
	if ca != "" && cb != "" {
		isNewer, _ := newer(Build{Version: cb}, Release{Version: ca})
		return isNewer
	}
	return a.PublishedAt.After(b.PublishedAt)
}

func (g GitHubSource) get(ctx context.Context, url string, into any) error {
	if !strings.HasPrefix(url, "https://") {
		return fmt.Errorf("selfupdate: refusing non-https API URL %s", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	client := g.Client
	if client == nil {
		client = observe.NewHTTPClient(observe.HTTPClientOptions{Timeout: 30 * time.Second})
	}
	resp, err := httpsOnly(client).Do(req)
	if err != nil {
		return fmt.Errorf("selfupdate: GitHub releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("selfupdate: GitHub releases: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(into); err != nil {
		return fmt.Errorf("selfupdate: GitHub releases: decode: %w", err)
	}
	return nil
}
