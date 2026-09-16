package cluster

import (
	"strings"
	"testing"
)

// realPodJSON is a trimmed but structurally faithful `kubectl get ... -o json`
// List, shaped after what control-plane's prod namespace actually returns. It
// carries the three things a jsonpath-based read gets wrong: a CronJob's
// doubly-nested pod template, an operator pinning a launched pod's image in an
// env var, and ordinary config that must NOT be mistaken for an image.
const realPodJSON = `{"items":[
 {"kind":"Deployment","metadata":{"name":"admin-server"},"spec":{"template":{"spec":{
   "containers":[{"name":"admin-server","image":"reg.example.com/acme/control-plane@sha256:aaa",
     "env":[{"name":"DAEMON_IMAGE","value":"reg.example.com/acme/workspace-base@sha256:bbb"},
            {"name":"LOG_LEVEL","value":"info"},
            {"name":"DATABASE_URL","value":"postgres://user:pw@db:5432/app"}]}]}}}},
 {"kind":"CronJob","metadata":{"name":"nightly"},"spec":{"jobTemplate":{"spec":{"template":{"spec":{
   "containers":[{"name":"worker","image":"reg.example.com/acme/control-plane@sha256:ccc"}]}}}}}},
 {"kind":"Deployment","metadata":{"name":"api"},"spec":{"template":{"spec":{
   "initContainers":[{"name":"migrate","image":"reg.example.com/acme/migrate@sha256:ddd"}],
   "containers":[{"name":"api","image":"reg.example.com/acme/api@sha256:eee"}]}}}}
]}`

// TestParseWorkloadImages_CronJobTemplateIsRead pins the nested-template read.
// A CronJob keeps its pod template at spec.jobTemplate.spec.template while
// every other kind uses spec.template. Missing it would make a drifted cron —
// running real application images — invisible to verification.
func TestParseWorkloadImages_CronJobTemplateIsRead(t *testing.T) {
	images, err := parseWorkloadImages([]byte(realPodJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, img := range images {
		if img.Kind == "CronJob" && img.Name == "nightly" {
			found = true
			if img.Image != "reg.example.com/acme/control-plane@sha256:ccc" {
				t.Errorf("CronJob image = %q", img.Image)
			}
		}
	}
	if !found {
		t.Error("CronJob pod template was not read — a drifted cron would be invisible")
	}
}

// TestParseWorkloadImages_ConfigPinnedImageIsFound is the regression guard for
// the false MISSING found against real prod: workspace-base is deployed only
// as workspace-controller's DAEMON_IMAGE.
func TestParseWorkloadImages_ConfigPinnedImageIsFound(t *testing.T) {
	images, err := parseWorkloadImages([]byte(realPodJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var found bool
	for _, img := range images {
		if strings.Contains(img.Image, "workspace-base") {
			found = true
			if img.EnvVar != "DAEMON_IMAGE" {
				t.Errorf("config-pinned image must record its env var, got %q", img.EnvVar)
			}
		}
	}
	if !found {
		t.Error("an image pinned in an env var was not found — it would be reported MISSING on a healthy env")
	}
}

// TestParseWorkloadImages_OrdinaryConfigIsNotAnImage is the other side of the
// same scan. Sweeping LOG_LEVEL or a DATABASE_URL into the image set would
// report them as missing images and bury the real findings in noise.
func TestParseWorkloadImages_OrdinaryConfigIsNotAnImage(t *testing.T) {
	images, err := parseWorkloadImages([]byte(realPodJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, img := range images {
		if img.EnvVar == "LOG_LEVEL" || img.EnvVar == "DATABASE_URL" {
			t.Errorf("%s must not be treated as an image reference (value %q)", img.EnvVar, img.Image)
		}
	}
}

// TestParseWorkloadImages_InitContainersAreRead — forge's `job` workload
// expansion puts real application images in init containers.
func TestParseWorkloadImages_InitContainersAreRead(t *testing.T) {
	images, err := parseWorkloadImages([]byte(realPodJSON))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, img := range images {
		if img.Container == "migrate" {
			return
		}
	}
	t.Error("init container image was not read")
}

// TestLooksLikeImageRef guards the env-var scan against noise.
//
// The scan exists so an operator's config-pinned pod image (control-plane's
// DAEMON_IMAGE) is not reported MISSING. The risk it introduces is the
// opposite failure: sweeping ordinary configuration into the image set and
// reporting database URLs as missing images. These cases pin the boundary —
// the rejections matter more than the acceptances.
func TestLooksLikeImageRef(t *testing.T) {
	accept := []string{
		"us-central1-docker.pkg.dev/reliant-labs-475814/reliant-prod/workspace-base@sha256:35d417158d5352f893ccff38881d059d45e210cbcec622d91c352a7a3a55d662",
		"ghcr.io/acme/api@sha256:1ea56682243fcb8775a2574ce41b129b28fb5c82cf239b64660e236a458f66fe",
		"ghcr.io/acme/api:v1.5.15",
		"localhost:5000/api:dev",
	}
	for _, v := range accept {
		if !looksLikeImageRef(v) {
			t.Errorf("expected %q to be recognised as an image reference", v)
		}
	}

	reject := []string{
		"",
		"info",                            // log level
		"v1.5.15",                         // bare version
		"postgres",                        // bare name, no registry
		"postgres:16",                     // unqualified host — too weak a signal
		"true",                            // a flag
		"postgres://user:pw@host:5432/db", // a database URL: has a colon and slashes
		"https://api.example.com/v1",      // a URL
		"/var/run/secrets/token",          // a path
		"some value with spaces:1",        // free text
	}
	for _, v := range reject {
		if looksLikeImageRef(v) {
			t.Errorf("expected %q NOT to be treated as an image reference", v)
		}
	}
}
