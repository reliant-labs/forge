package deploytarget

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Exercise both real artifact transports with the anonymous deploy's Docker
// config shape. An ambient desktop helper must never shadow its current login.
func TestArtifactPushesUsePinnedAnonymousCredential(t *testing.T) {
	for _, kind := range []string{"site", "source maps"} {
		t.Run(kind, func(t *testing.T) {
			var authenticated atomic.Int32
			var manifests sync.Map
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, pass, ok := r.BasicAuth()
				if !ok || user != "forge" || pass != "current-claim" {
					w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				authenticated.Add(1)
				switch r.Method {
				case http.MethodHead:
					w.WriteHeader(http.StatusNotFound)
				case http.MethodPost:
					w.Header().Set("Location", r.URL.Path+"session")
					w.WriteHeader(http.StatusAccepted)
				case http.MethodPut:
					body, _ := io.ReadAll(r.Body)
					if strings.Contains(r.URL.Path, "/manifests/") {
						manifests.Store(r.URL.Path, body)
					}
					w.WriteHeader(http.StatusCreated)
				case http.MethodGet:
					body, ok := manifests.Load(r.URL.Path)
					if !ok {
						http.NotFound(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
					w.Header().Set("Content-Length", strconv.Itoa(len(body.([]byte))))
					_, _ = w.Write(body.([]byte))
				default:
					http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				}
			}))
			defer srv.Close()
			host := strings.TrimPrefix(srv.URL, "http://")
			cfg := map[string]any{
				"credsStore":  "forge-test-must-not-be-called",
				"credHelpers": map[string]string{host: ""},
				"auths":       map[string]any{host: map[string]string{"auth": base64.StdEncoding.EncodeToString([]byte("forge:current-claim"))}},
			}
			raw, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("DOCKER_CONFIG", dir)
			var digest string
			if kind == "site" {
				digest, err = PushStaticSiteArtifact(context.Background(), host+"/org/project/web/static.v1", []byte("site"))
			} else {
				digest, err = PushSourcemapsArtifact(context.Background(), host+"/org/project/web-sourcemaps", "build1", SourcemapBuild{BuildID: "build1"}, []byte("maps"))
			}
			if err != nil {
				t.Fatalf("push %s with current claim: %v", kind, err)
			}
			if !strings.HasPrefix(digest, "sha256:") || authenticated.Load() == 0 {
				t.Fatalf("push did not publish an authenticated artifact: digest=%q requests=%d", digest, authenticated.Load())
			}
			stored, err := os.ReadFile(filepath.Join(dir, "config.json"))
			if err != nil || string(stored) != string(raw) {
				t.Fatalf("artifact push mutated Docker credentials: %v", err)
			}
		})
	}
}
