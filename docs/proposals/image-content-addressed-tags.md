# Content-addressed image tags

- **Status**: deferred (interim fix landed; see "Interim fix" below)
- **Author**: cp-forge agent (2026-06-08 dirty-tag image-cache-staleness postmortem)
- **Related**: `kcl/lib/services.k` `_pull_policy` helper (interim fix), `internal/cli/build.go` (tag-construction owner)
- **Touches**: `internal/cli/build.go` (compute content hash; push under content-addressed tag), `internal/cli/deploy.go` (resolve the deployed tag to feed KCL), `kcl/lib/services.k` (could simplify `_pull_policy` back to a constant once tags are content-addressed), `forge.yaml` (opt-in toggle during rollout).

## Context

`forge build` derives an image tag from `git rev-parse --short HEAD`, appending `-dirty` when the worktree has uncommitted changes (`internal/cli/build.go`). The tag is the only identity Kubernetes uses to decide whether to pull a new image — `imagePullPolicy: IfNotPresent` (the default for tagged images) lets the kubelet skip the pull when it already has a layer cached under that tag.

The two-rebuild dirty case: `git sha = 1ad9cf1`, worktree dirty → tag = `1ad9cf1-dirty`. Edit code (still uncommitted) → tag = `1ad9cf1-dirty` still. New image, same tag, `docker push` overwrites the registry entry. The kubelet on the k3d node sees the tag locally already and runs the **stale** binary. `forge deploy` reports "rollout successful" while the cluster is on the previous build. cp-forge hit this twice in one session — 20+ minutes of "my RPC change doesn't work" debugging both times.

Two structural answers:

1. **Pragmatic / interim:** force `imagePullPolicy: Always` whenever the tag ends in `-dirty` (and for `:latest`). The kubelet re-pulls on every pod start, so the dev iteration loop is correct again. Cost: an extra pull per pod start for dirty builds (registry is `localhost:5051` in dev → ~free).
2. **Correct / long-term:** make the image tag itself a content hash of the binary, so two distinct binaries cannot collide under the same tag. The kubelet's `IfNotPresent` caching is then *correct* for every case — including dirty builds — because a different binary genuinely has a different tag.

The interim fix landed (see `kcl/lib/services.k::_pull_policy`). This proposal scopes the long-term fix.

## Why content-addressed tags

The kubelet caching model is "tags identify images." That contract is the right one *if the tags are honest*. Git-SHA-with-dirty-suffix tags are dishonest: two different binaries can share the same `<sha>-dirty` tag because the SHA didn't move while the worktree changed.

A content hash is honest: the tag IS the binary's identity. `sha256(binary)` (or a layer-digest derivative) cannot collide across distinct images. Every property we want falls out:

- **No staleness.** Different binary → different tag → kubelet pulls.
- **No "Always" hammer.** Tagged images stay `IfNotPresent`; the kubelet's cache works as designed across rollouts. The `_pull_policy` helper in `kcl/lib/services.k` could collapse back to a constant `IfNotPresent` (with `:latest` still forced to `Always` for the rare manual `latest`-push case).
- **Deterministic deploys.** Two `forge build`s of the same source produce the same tag; `forge deploy` is a no-op when the binary is unchanged. Today a dirty rebuild churns the cluster even when the only edit was to a comment.
- **Easy provenance / rollback.** The tag is a content fingerprint, so `kubectl describe pod` tells you exactly which artifact is running.

The git SHA stays useful as **metadata** (`app.kubernetes.io/git-sha=1ad9cf1` label, image annotation, build manifest) — just not as the identity.

## Why this is deferred (not "do it next")

It's a deeper change than it looks. Real work:

1. **Build-pipeline coordination.** `forge build` has to compute the hash AFTER the binary is built but BEFORE the docker tag is decided. The current shape is "decide tag from git → pass tag to `docker build`." The new shape is "build to a content-derived intermediate → hash → re-tag." That's a re-plumb of `internal/cli/build.go`.
2. **Multi-binary projects.** Each binary in `cmd/` produces its own image; each needs its own content hash. The tag scheme has to scope per-binary (probably `<binary-name>:sha256-<hash[:12]>`).
3. **Frontend / Next.js images.** Frontends ship as containers too. The "content" is the built `.next/` tree, not a Go binary. Same idea, different hash input. Two different code paths to land cleanly.
4. **External-deploy targets.** `forge.Service.deploy = External { deploy_cmd = "flyctl deploy --image ${IMAGE}:${TAG}" }` substitutes `${TAG}` from the resolved tag. Content tags break operator expectations if they were used to seeing the git SHA in the substituted command. Needs a clear migration story + an explicit `git_sha` template variable as a non-identity fallback.
5. **Cache friendliness.** Layer caches (the bottom layers, e.g. base image + go-mod download) stay the same across builds; only the top layer (the new binary) differs. The content hash MUST hash the top layer, not the whole image — otherwise no rebuild ever shares a tag and registry storage explodes. Concretely: tag off `docker inspect <local-image-id> --format '{{.RootFS.Layers}}' | tail -1` (the topmost layer's diff-ID) rather than the full image manifest digest. Same registry behavior, far better cache reuse.
6. **Rollout migration.** Existing clusters running SHA-tagged pods need a deploy that flips tag-scheme without orphaning the old image (`kubectl set image deployment/foo foo=<old-tag>` after a botched roll-forward should still resolve). The pragmatic path: ship both tag schemes during a deprecation window — registry double-tagged, KCL renderer reads a `forge.yaml: build.tag_scheme: content|git-sha` toggle.

Each of these is solvable; together they're a multi-PR landing, not a one-commit fix. The dev loop is unblocked today by the `_pull_policy = "Always" when -dirty` interim. There is no production correctness bug — production builds are on clean SHAs, where `IfNotPresent` is correct under either tag scheme.

## Proposed change (when un-deferred)

Three layers, smallest to largest.

### Layer 1 — opt-in toggle in `forge.yaml`

```yaml
build:
  tag_scheme: content  # default: git-sha
```

Only `content` and `git-sha` are valid. Default stays `git-sha` for one minor release so projects opt in explicitly. After a deprecation window, default flips to `content` and `git-sha` becomes opt-out for projects that need to keep SHA tags for CI integration reasons.

### Layer 2 — `forge build` computes and stamps the tag

`internal/cli/build.go`:

```go
// After docker build completes:
topLayerDigest := dockerInspectTopLayer(localImageID)  // sha256:abc123...
tag := "sha256-" + topLayerDigest[7:7+12]              // sha256-abc123abc123 (12-char prefix)
dockerTag(localImageID, registry + "/" + imageName + ":" + tag)
dockerPush(registry + "/" + imageName + ":" + tag)
```

The git SHA still goes on the image as a *label* (`org.opencontainers.image.revision=1ad9cf1`) and as a Deployment metadata annotation (`app.kubernetes.io/git-sha`), preserving provenance for `kubectl describe` / `forge inspect`.

### Layer 3 — `forge deploy` resolves the deployed tag

`internal/cli/deploy.go` reads the just-pushed tag from `forge build`'s output and passes it via `-D image_tag=<tag>` to `kcl run`. The KCL `_pull_policy` helper (in `kcl/lib/services.k`) collapses to:

```python
_pull_policy = lambda image_ref: str -> str {
    # Once tags are content-addressed, `IfNotPresent` is always correct
    # for them — distinct binaries produce distinct tags by construction.
    # `:latest` keeps its `Always` escape hatch for manual debugging pushes.
    "Always" if image_ref.endswith(":latest") else "IfNotPresent"
}
```

The `-dirty` branch goes away — there is no `-dirty` tag in the content scheme; the binary's content already disambiguates.

## Test plan

- Unit test in `internal/cli/build_test.go`: deterministic content hash for a fixed binary input.
- Integration test in `scripts/e2e-*.sh`: build → modify a single Go file → rebuild → assert the two pushed tags differ; assert `kubectl rollout status` actually pulls and rolls (no `IfNotPresent` short-circuit).
- KCL test (`kcl/tests/positive_image_pull_policy.k` becomes the regression pin): same image-tag inputs, asserts the renderer behavior matches whatever scheme is in effect. After the migration the test's `-dirty` cases become "no such tag exists in content scheme" — drop them; keep the `:latest` → `Always` and content-tag → `IfNotPresent` cases.

## Interim fix (already landed)

`kcl/lib/services.k::_pull_policy`:

```python
"Always" if _tag.endswith("-dirty") or _tag == "latest" else "IfNotPresent"
```

Applied uniformly to Service, Operator, and CronJob container specs. Test: `kcl/tests/positive_image_pull_policy.k`. Verified against cp-forge's `deploy/kcl/dev/main.k` — `-dirty` and `latest` render `Always`, clean SHA renders `IfNotPresent`, tag-containing-but-not-ending-in-dirty (`:dirty-feature-v1`) renders `IfNotPresent`.

This is correct but blunt: an extra pull per pod start during dev iteration. Real cost is negligible at `localhost:5051` registry latency. Real cost in CI/prod is zero — those tags are clean SHAs that stay `IfNotPresent`.

The interim is good enough to defer this proposal until the build pipeline has another reason to be reshaped (likely the Bazel / nix-build remote-cache work in the backlog, which already needs content-addressed inputs).
