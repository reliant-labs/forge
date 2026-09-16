package templates

import (
	"strings"
	"testing"
)

// The CI-to-promote hop, as rendered.
//
// These assert on the WORKFLOW TEXT, which is the only thing a user actually
// gets — the template is the deliverable here, and a data struct that is right
// while the YAML that reads it is wrong ships a broken pipeline. So every
// assertion below is about a string that has to appear (or must NOT appear) in
// the file GitHub will execute.

func renderBuildImages(t *testing.T, data BuildImagesWorkflowData) string {
	t.Helper()
	out, err := CITemplates("github").Render("build-images.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render build-images.yml.tmpl: %v", err)
	}
	return string(out)
}

// OFF BY DEFAULT. The cut-release job talks to a control plane forge did not
// scaffold, so a project that has not opted in must not get a job that fails
// on every push to main against a server it does not have.
func TestBuildImages_CutReleaseIsAbsentUnlessEnabled(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
	})

	for _, absent := range []string{
		"cut-release:",
		"CutRelease",
		"DEPLOY_TOKEN",
		"CONTROL_PLANE_URL",
	} {
		if strings.Contains(out, absent) {
			t.Errorf("an opted-out project's workflow contains %q; "+
				"the cut-release job is gated and must render nothing", absent)
		}
	}

	// And the workflow is still complete — the gate removes a job, it does
	// not leave a summary referencing one that is not there.
	if strings.Contains(out, "needs.cut-release.result") {
		t.Error("the summary references the cut-release job that was not emitted")
	}
}

func TestBuildImages_CutReleaseJobWhenEnabled(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
		CutRelease:  true,
	})

	for _, want := range []string{
		"cut-release:",
		"needs: build-push",
		"/controlplane.v1.DeployService/CutRelease",
		"/controlplane.v1.DeployService/Promote",
		// The summary must account for the job it now depends on.
		"needs.cut-release.result",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the enabled workflow is missing %q", want)
		}
	}
}

// THE DIGEST, NOT THE TAG — and this is the assertion the whole feature rests
// on.
//
// A tag is a mutable pointer. If the release were cut against `sha-abc1234`
// rather than against the digest build-push-action reported, then re-pushing
// that tag would change what the release names, and "the bytes that passed
// staging are the bytes that reach prod" would quietly stop being true while
// every test still passed. The workflow must carry the digest through as a job
// output and send THAT.
func TestBuildImages_TheReleaseIsCutAgainstTheDigest(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
		CutRelease:  true,
	})

	if !strings.Contains(out, "digest: ${{ steps.build.outputs.digest }}") {
		t.Error("build-push does not publish the digest build-push-action reported as a job output; " +
			"without it the cut job has no immutable reference to pin")
	}
	if !strings.Contains(out, "IMAGE_DIGEST: ${{ needs.build-push.outputs.digest }}") {
		t.Error("the cut job does not consume the build job's digest output")
	}
	if !strings.Contains(out, `--arg digest    "$IMAGE_DIGEST"`) {
		t.Error("the CutRelease request body does not send the digest")
	}

	// The negative half, which is the one that catches a regression: the
	// request body must not be built from a tag.
	body := between(t, out, "- name: Cut the release", "- name: Promote")
	for _, forbidden := range []string{
		"steps.meta.outputs.tags",
		"steps.tag.outputs.tag",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the cut step references %q; a release pinned to a mutable tag "+
				"can be re-pointed underneath a promotion", forbidden)
		}
	}
}

// THE PROMOTE CALL CANNOT NAME AN IMAGE.
//
// PromoteReleaseRequest has no image field precisely so that a promotion
// cannot express a rebuild. That guarantee is only as good as the client that
// calls it, so this pins the CI side of it: the promote body carries a
// VERSION, and nothing that could be read as bytes to produce.
func TestBuildImages_ThePromoteCallNamesAVersionAndNoImage(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
		CutRelease:  true,
	})

	promote := between(t, out, "- name: Promote", "  summary:")
	if !strings.Contains(promote, `--arg version       "${{ steps.cut.outputs.version }}"`) {
		t.Error("the promote body does not name the version that was just cut")
	}

	// Scoped to the REQUEST BODY, not the whole step. The step's comments
	// necessarily discuss digests — explaining that the promotion does not
	// carry one is the point of them — and an assertion over the prose would
	// fail on a correct implementation while proving nothing about the wire.
	// What must be free of image references is the JSON that is actually sent.
	body := between(t, promote, "BODY=$(jq -n", "')")
	for _, forbidden := range []string{
		"digest", "IMAGE_DIGEST", "REGISTRY", "image",
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the promote REQUEST BODY carries %q; a promotion re-points at a "+
				"release and must carry nothing that could name bytes to build\nbody was: %s",
				forbidden, body)
		}
	}
}

// THE VERSION IS DERIVED FROM THE COMMIT, WHICH IS WHAT MAKES THE RETRY WORK.
//
// Idempotency on the server is keyed on (org, version). If CI generated a
// version from the clock, or from the run number, every re-run would ask for a
// version that had never been cut — the server would happily cut it, and the
// unique index would never be consulted. The client-side half of idempotency
// is that the same commit always produces the same label.
func TestBuildImages_TheVersionIsDerivedFromTheCommit(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
		CutRelease:  true,
	})

	if !strings.Contains(out, `VERSION="sha-$(echo "${{ github.sha }}" | cut -c1-7)"`) {
		t.Error("the release version is not derived from github.sha; " +
			"a version that varies between runs of the same commit defeats the ledger's idempotency")
	}
	for _, forbidden := range []string{
		"github.run_number",
		"github.run_id",
		"github.run_attempt",
		"date +",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the workflow derives something from %q; a run-varying value in the "+
				"version would cut a new release on every retry", forbidden)
		}
	}
}

// A retry must not be reported as a failure. `created: false` is the server
// saying "this was already recorded", which is exactly what a re-run should
// produce, and a workflow that treated it as an error would go red for having
// been re-run.
func TestBuildImages_ARetryIsNotTreatedAsAFailure(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
		CutRelease:  true,
	})

	cut := between(t, out, "- name: Cut the release", "- name: Promote")
	if !strings.Contains(cut, "was already cut") {
		t.Error("the cut step does not handle the already-cut case; " +
			"`created: false` is a successful retry, not an error")
	}
	// The only exit 1 in the cut step is the HTTP failure, not the
	// already-exists case.
	if strings.Contains(cut, `"created" != "true"`) || strings.Contains(cut, "created == false") {
		t.Error("the cut step appears to fail when created is false; a retry must succeed")
	}
}

// A promote failure must tell the operator the release survived and the
// recovery is a retry. Without that sentence the natural instinct is to go
// looking for something to clean up, and there is nothing to clean up.
func TestBuildImages_APromoteFailureExplainsTheRecovery(t *testing.T) {
	out := renderBuildImages(t, BuildImagesWorkflowData{
		ProjectName: "myapp",
		Registry:    "ghcr",
		CutRelease:  true,
	})

	promote := between(t, out, "- name: Promote", "  summary:")
	for _, want := range []string{
		"The release IS cut",
		"Re-run this workflow to retry the promotion",
		"nothing needs cleaning up",
	} {
		if !strings.Contains(promote, want) {
			t.Errorf("a promote failure does not explain the recovery; missing %q", want)
		}
	}
}

// between returns the text between two markers, failing the test if either is
// absent — so a renamed step surfaces as a clear failure here rather than as a
// vacuous pass in whatever assertion consumes the (empty) slice.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found in the rendered workflow", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("marker %q not found after %q", end, start)
	}
	return rest[:j]
}
