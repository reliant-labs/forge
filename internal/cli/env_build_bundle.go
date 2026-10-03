package cli

// Writing the bundle: `forge env build <env>` is the only producer (doc §4.4).
//
// WHAT A BUNDLE IS FOR. A release says "these exact image bytes"; it is
// env-agnostic. A bundle says "these exact MANIFEST bytes, for this env, from
// this source" — and that is the half nothing recorded before. The same commit
// renders differently under a different forge, a different config_gen.k, a
// moved sibling checkout, or a dirty tree, so git is not an answer to "what
// was shipped" and neither is the release. The bundle is the render's OUTPUT,
// and the commit is one of its inputs.
//
// WHERE IT GOES, and why that is two answers rather than a flag (doc §4.3):
//
//   - An env whose ledger is a control plane PUSHES to
//     <image_push_base>/bundle.v1/<env>, beside the images it was built with,
//     and records it with RecordBundle — which carries the BYTES, so the
//     server records a shape it verified rather than one forge described.
//   - Every other env — a file-ledger env, and every LOCAL env even when its
//     control plane is reachable — writes into the machine ledger's own OCI
//     layout. A local env's bundle is not pushed because it is not shipped.
//
// The rule is the ledger's, never a flag's: an env's bundle is recorded
// wherever that env's history already lives, so "where is this env's bundle"
// cannot have two answers for one env.
//
// A BUILD THAT PUSHES NOTHING STILL WRITES ONE, to the local layout. That is
// deliberate: the bundle is how a later deploy applies the same bytes it
// recorded, and a build with no push has still produced a render worth
// pinning. What it has NOT produced is pushed images, which is the release's
// problem and is refused there.
//
// FAILING TO RECORD IS NOT FAILING TO BUILD, with one exception. `forge env
// build` is the primitive a CI build job runs and a developer runs offline,
// so an unreachable control plane warns and carries on — the same asymmetry
// recordEnvBuildDeclaration follows, for the same reason. A control plane that
// ANSWERS and says no is different: that is a fact about the env, and a green
// build over it would leave every reader on a bundle that does not exist.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/pkg/release"
)

// bundleWriteOutcome is what writing one env's bundle did, so the caller can
// report it without re-deriving any of it.
type bundleWriteOutcome struct {
	Env string
	// Digest is the bundle's identity: its OCI manifest digest.
	Digest string
	// Reference is where the bytes are — a registry reference, or
	// `oci-layout:<dir>@<digest>` for the machine ledger.
	Reference string
	// Pushed reports whether the bytes went to a registry, as opposed to
	// the local OCI layout.
	Pushed bool
	// Recorded reports whether the bundle reached a ledger. False with a
	// nil error is the undeliverable case: the bundle exists, nothing
	// recorded it, and the next command that reaches the control plane
	// will.
	Recorded bool
	// Created is false when the ledger already held this digest for this
	// env — an idempotent retry (F-3), which is what makes a re-push after
	// a lost RecordBundle response free.
	Created bool
	// Objects is how many rendered objects the bundle's shape describes,
	// for the one-line report.
	Objects int
	// Skipped reports that no bundle was written at all, because the env
	// could not be projected. Distinct from "written but not recorded":
	// there is nothing to record and nothing to fetch, and a reader must
	// not show a digest for it.
	Skipped bool
}

// writeBundlesFn is the bundle step as the BUILD calls it.
//
// A seam so a test can prove the build REACHES it. The producer tests below
// call writeEnvBundles directly, which proves the bundle is correct but says
// nothing about whether any command writes one — and "forge env build quietly
// stopped writing bundles" is a regression no producer test can catch.
var writeBundlesFn = writeEnvBundles

// writeEnvBundles writes one bundle per env named, from the pins this build
// resolved.
//
// SEVERAL ENVS FROM ONE CUT is what --bundle-envs is for. A release is
// env-agnostic and a bundle is not, so a pipeline that cuts one release and
// ships it to three envs would otherwise have to re-render each one later,
// at deploy time, from a checkout that may have moved. Rendering them all
// here binds every env's manifests to the same release and the same source.
func writeEnvBundles(ctx context.Context, projectDir string, envs []string, in bundleBuildInputs) ([]bundleWriteOutcome, error) {
	out := make([]bundleWriteOutcome, 0, len(envs))
	for _, env := range envs {
		got, err := writeEnvBundle(ctx, projectDir, env, in)
		if err != nil {
			return out, fmt.Errorf("write env %s's bundle: %w", env, err)
		}
		out = append(out, got)
	}
	return out, nil
}

// bundleBuildInputs is what a bundle needs that only the BUILD knows: which
// release it pins (if any), the digests it resolved, and the CI run that
// produced it.
//
// The provenance and the shape are NOT here: both are projected from the
// render, by projectEnvShapeFn, inside writeEnvBundle. That is the §4.2
// invariant — the shape is computed by the same pass that writes the
// manifests, so the two cannot disagree — and accepting a caller's shape here
// would be exactly the "description beside the bytes" the whole design
// removes.
type bundleBuildInputs struct {
	// Release is the version pinned, "" for an unreleased bundle (a dev
	// build, or a Preview deploy) pinned to the build's own digests.
	Release string
	// Pins is what the render resolved: images by artifact key, and source
	// pins for source-built components.
	Pins release.BundlePins
	// Run is the CI run that built it, or the zero Run when a human did.
	Run release.Run
	// Pushed reports whether this build pushed its images. A build that
	// pushed nothing writes its bundle to the local layout only, whatever
	// the env's ledger is: a registry reference to bytes nobody pushed
	// would be a record of a deploy that cannot be performed.
	Pushed bool
	// NoCharts omits the optional charts layer (doc §4.1). --no-charts on
	// the build, and the default for a build that cannot run helm.
	NoCharts bool
	// Now is the bundle's creation time. The caller's, because
	// bundle.Build never reads a clock — its digest must depend on the
	// render and nothing else, and every env in one --bundle-envs pass
	// shares one timestamp.
	Now time.Time
	// errOut is where warnings go. nil means stderr.
	errOut io.Writer
}

// writeEnvBundle renders one env, builds its bundle, puts the bytes where that
// env's ledger says they go, and records it.
func writeEnvBundle(ctx context.Context, projectDir, env string, in bundleBuildInputs) (bundleWriteOutcome, error) {
	if in.Now.IsZero() {
		return bundleWriteOutcome{}, fmt.Errorf("%w: a bundle's creation time is the caller's", release.ErrInvalid)
	}

	// The SAME projection `forge env shape` prints and F-DECL records. One
	// function, one render: a shape a user read, a shape the control plane
	// stored and a shape a bundle sealed describe the same bytes or the
	// bundle is a record of a deploy that never happened.
	//
	// A PROJECTION FAILURE IS LOUD BUT NOT FATAL, the same asymmetry
	// recordEnvDeclaration follows and for a reason that is stronger here.
	// The caller asked to BUILD, and an env with no renderable
	// deploy/kcl/<env>/ is a legitimate `forge env build` target — a test
	// project, an env named only on the command line, a project whose KCL
	// this forge cannot render for an unrelated reason. Refusing the build
	// over a record nobody asked for would break a command that has always
	// worked.
	//
	// It is also RECOVERABLE in a way a declaration is not: `forge env
	// deploy` resolves the bundle for (env, release, tree) and renders one
	// on demand when none exists, so a build that could not write one costs
	// a render later rather than losing the deploy. What is NOT tolerated is
	// a bundle that was written and then mis-recorded — see
	// recordWrittenBundle.
	doc, err := projectEnvShapeFn(ctx, in.errWriter(), env)
	if err != nil {
		fmt.Fprintf(in.errWriter(),
			"[bundle] Warning: env %s's bundle was not written: %v\n"+
				"[bundle]   The build continues; `forge env deploy %s` renders one on demand, and "+
				"`forge env shape %s` reproduces this.\n", env, err, env, env)
		return bundleWriteOutcome{Env: env, Skipped: true}, nil
	}

	charts, err := bundleCharts(ctx, projectDir, env, in)
	if err != nil {
		return bundleWriteOutcome{}, err
	}

	built, err := bundle.Build(ctx, bundle.BuildInput{
		Project:    doc.Project,
		Env:        env,
		Release:    in.Release,
		Pins:       in.Pins,
		Provenance: doc.Provenance,
		Shape:      bundleShapeInputOf(doc),
		Charts:     charts,
		CreatedAt:  in.Now,
	})
	if err != nil {
		return bundleWriteOutcome{}, err
	}

	ledger, err := bundleLedgerFor(ctx, projectDir, env)
	if err != nil {
		// #404's rule, applied to the bundle: a build never needs a
		// reachable control plane. Opening a hosted env's ledger needs a
		// credential and a server, and when neither answers there is
		// nowhere to say where the bytes go — so nothing is written, the
		// same outcome as a projection failure, and `forge env deploy`
		// renders one on demand. A control plane that ANSWERS and refuses
		// is a fact about the env and still fails the build.
		if declarationUndeliverable(err) {
			fmt.Fprintf(in.errWriter(),
				"[bundle] Warning: env %s's bundle was not written: %v\n"+
					"[bundle]   The build continues; the next build or deploy that reaches the control plane writes it.\n",
				env, err)
			return bundleWriteOutcome{Env: env, Skipped: true}, nil
		}
		return bundleWriteOutcome{}, err
	}

	placed, err := placeBundleBytes(ctx, projectDir, env, built, ledger, in)
	if err != nil {
		return bundleWriteOutcome{}, err
	}
	placed.Objects = len(doc.Shape.Objects)

	record := release.BundleRecord{
		Env: env, Release: in.Release,
		Digest: built.Digest, Reference: placed.Reference,
		ConfigDigest: built.Doc.ConfigDigest,
		Shape:        doc.Shape, Provenance: doc.Provenance,
		Run: in.Run, CreatedAt: in.Now,
	}
	blobs := bundleBlobs{
		Repository: placed.repository,
		Manifest:   built.Manifest,
		Config:     built.ConfigBlob(),
	}
	return recordWrittenBundle(ctx, projectDir, env, ledger, record, blobs, placed, in)
}

// bundleShapeInputOf re-states the declaration half of the projection for
// bundle.Build, which runs the OBJECT half itself from the stream it packages.
//
// Build deliberately does not accept a finished shape: it projects the objects
// from the same parse it writes the manifest layer from, which is what makes
// "the shape describes these manifests" true by construction rather than by a
// caller's discipline. So the declaration fields are passed through and the
// objects are not.
func bundleShapeInputOf(doc envShapeDoc) bundle.ShapeInput {
	return bundle.ShapeInput{
		Kind:      doc.Shape.Kind,
		Workloads: doc.Shape.Workloads,
		Secrets:   doc.Shape.Secrets,
		Domains:   doc.Shape.Domains,
		Clusters:  doc.Shape.Clusters,
		Manifests: doc.manifests,
		Images:    doc.images,
	}
}

// bundleCharts is layer 1: `helm template` output for the env's declared
// forge.HelmChart platform deps, keyed by chart name (doc §4.1).
//
// WHY THE BUNDLE CARRIES THEM AT ALL, when the shape deliberately does not.
// A chart's objects are a platform dependency's, not this project's
// declaration — which is why `forge env shape` leaves them out, so a
// declaration stays derivable with no helm and no network. But the bundle is
// what a deploy APPLIES, and a deploy expands those charts into the same
// apply stream as everything else. A bundle without them would describe a
// deploy missing its cert-manager and its gateway.
//
// It uses the deploy path's own renderer (cluster.RenderChartStreams, through
// resolveDeployHelmSpecs), not a second expansion: a bundle whose charts were
// templated differently from the way the deploy templates them would be a
// record of a deploy nobody performs.
//
// NO HELM IS NOT A FAILURE HERE, unlike in `forge env render`. A build runs
// in CI and on a developer's machine, and refusing to write a bundle because
// helm is absent would make the record conditional on a tool that has nothing
// to do with the project's own code. The charts layer is OPTIONAL by design
// (doc §4.1 marks it "absent with --no-charts"), so it is omitted and said so
// — and the shape, which is what every reader indexes, is unaffected either
// way.
func bundleCharts(ctx context.Context, projectDir, env string, in bundleBuildInputs) (map[string]string, error) {
	if in.NoCharts {
		return nil, nil
	}
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil || entities == nil || len(entities.HelmCharts) == 0 {
		// A render failure is not fatal for the same reason
		// recordEnvBuildDeclaration tolerates one: the bundle's own
		// render already succeeded (projectEnvShapeFn above), so this
		// second read failing means something unrelated to the charts.
		return nil, nil //nolint:nilerr // charts are the optional layer; the bundle stands without them
	}
	if _, lookErr := exec.LookPath("helm"); lookErr != nil {
		fmt.Fprintf(in.errWriter(),
			"[bundle] Note: env %s declares %d helm chart(s) and helm is not on PATH, so its bundle carries no charts layer.\n"+
				"[bundle]   The shape and the manifests are unaffected; install helm, or pass --no-charts to state the omission.\n",
			env, len(entities.HelmCharts))
		return nil, nil
	}
	specs, err := resolveDeployHelmSpecs(ctx, entities, nil)
	if err != nil {
		return nil, err
	}
	streams, err := cluster.RenderChartStreams(ctx, specs)
	if err != nil {
		return nil, fmt.Errorf("render env %s's helm charts for its bundle (pass --no-charts to omit them): %w", env, err)
	}
	out := make(map[string]string, len(streams))
	for _, cs := range streams {
		if strings.TrimSpace(cs.Stream) == "" {
			continue
		}
		out[cs.Name] = cs.Stream
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// bundlePushTarget opens the registry a bundle is pushed to.
//
// A var so a test can push into an in-process oras target instead of standing
// up a registry and a credential helper. The thing under test here is the
// ORDER and the PLACEMENT — render, build, push to the env's bundle
// repository, record the reference that push produced — and none of that is a
// property of the network. internal/bundle's own tests cover Push against a
// real oras store, including the blobs-before-manifest rule.
var bundlePushTarget = func(reference string) (bundle.Pusher, error) {
	return bundle.NewRepository(reference)
}

// pushBundleTo opens the repository and pushes, returning the digest.
func pushBundleTo(ctx context.Context, repo string, built bundle.Bundle) (string, error) {
	target, err := bundlePushTarget(repo)
	if err != nil {
		return "", err
	}
	return bundle.Push(ctx, target, built)
}

// placedBundle is where a bundle's bytes ended up.
type placedBundle struct {
	bundleWriteOutcome
	// repository is the push destination, for RecordBundle's own
	// `repository` field. Empty for a local layout, which the hosted path
	// never sees.
	repository string
}

// placeBundleBytes pushes or writes the bundle, by the env's ledger.
//
// THE DECISION IS THE LEDGER'S, with one override: a build that pushed no
// images never pushes its bundle either. Recording a registry reference for
// bytes that are not in that registry is F-4's shape — resolvable,
// recordable, unfetchable — manufactured by the writer rather than by a GC
// bug.
func placeBundleBytes(ctx context.Context, projectDir, env string, built bundle.Bundle, ledger envLedger, in bundleBuildInputs) (placedBundle, error) {
	if ledger.Hosted && in.Pushed {
		base := cachedHostedPushBase(projectDir, env)
		if base == "" {
			// No base to push under. NOT an error: the images were
			// pushed to their own declared references and the build
			// stands. The bundle goes to the local layout, and the next
			// build that learns the base (every ensure caches it)
			// pushes one.
			fmt.Fprintf(in.errWriter(),
				"[bundle] Note: env %s's image push base is not known yet, so its bundle was written locally.\n"+
					"[bundle]   The next build or deploy that reaches the control plane learns the base and pushes one.\n", env)
			return writeBundleLocally(ctx, projectDir, env, built)
		}
		repo := bundle.Repository(base, env)
		digest, err := pushBundleTo(ctx, repo, built)
		if err != nil {
			// A FAILED PUSH DOES NOT FAIL THE BUILD, for the same reason a
			// failed record does not: the images went to their own declared
			// references and the release is cut, so the build's product
			// stands. The bundle is a RECORD, and a missing one is
			// recoverable — `forge env deploy` renders one on demand.
			//
			// Making it fatal would mean a build newly depended on the
			// bundle subtree being reachable and writable, which is a
			// different fact from the image registry being reachable: a
			// registry can admit images and reject an artifact type, and a
			// project can push images somewhere the control plane does not
			// name as its base. Found exactly that way — two tests that
			// push images to a stub and had no route to the declared
			// registry started failing the whole build.
			fmt.Fprintf(in.errWriter(),
				"[bundle] Warning: env %s's bundle could not be pushed to %s (%v).\n"+
					"[bundle]   It was written to this machine's ledger instead; the build continues, and the next\n"+
					"[bundle]   build or deploy that can reach the registry pushes it (a bundle is content-addressed,\n"+
					"[bundle]   so re-pushing identical bytes is free).\n", env, repo, err)
			return writeBundleLocally(ctx, projectDir, env, built)
		}
		return placedBundle{
			bundleWriteOutcome: bundleWriteOutcome{
				Env: env, Digest: digest, Reference: repo + "@" + digest, Pushed: true,
			},
			repository: repo,
		}, nil
	}
	return writeBundleLocally(ctx, projectDir, env, built)
}

// writeBundleLocally writes the bundle into the machine ledger's OCI layout.
//
// The layout sits BESIDE the records that index it (Store.OCIDir), so a
// bundle's two halves — the row and the blob — cannot end up on different
// machines or in different lifetimes.
func writeBundleLocally(ctx context.Context, projectDir, env string, built bundle.Bundle) (placedBundle, error) {
	store, err := openMachineLedger(projectDir)
	if err != nil {
		return placedBundle{}, err
	}
	layout, err := bundle.NewLocalLayout(store.OCIDir())
	if err != nil {
		return placedBundle{}, err
	}
	digest, err := layout.Write(ctx, built)
	if err != nil {
		return placedBundle{}, err
	}
	return placedBundle{bundleWriteOutcome: bundleWriteOutcome{
		Env: env, Digest: digest, Reference: layout.Reference(digest),
	}}, nil
}

// recordWrittenBundle indexes the bundle in the env's ledger.
//
// An UNDELIVERABLE record warns and returns; a REFUSED one fails the build.
// The split is declarationUndeliverable's, reused rather than re-derived: no
// credential, a transport failure, Unavailable and DeadlineExceeded mean the
// request never got an answer, and the bundle is content-addressed so a retry
// is free (F-3). Any other answer is the control plane having read the request
// and declined it, which is a fact about this env that a green build must not
// hide.
//
// F-15 rides the same path. A control plane that predates bundles answers
// Unimplemented on RecordBundle, which is not a refusal of anything — it is a
// server that cannot hold the record at all — so it warns, says what to do,
// and the build continues.
func recordWrittenBundle(ctx context.Context, projectDir, env string, ledger envLedger,
	record release.BundleRecord, blobs bundleBlobs, placed placedBundle, in bundleBuildInputs,
) (bundleWriteOutcome, error) {
	recorder, err := bundleRecorderForEnv(ctx, projectDir, env, ledger)
	if err != nil {
		// Opening the records half can fail the same way the record call
		// can — no credential, no transport — and it is classified the same
		// way, by the one classifier the declaration and RecordBundle use.
		if declarationUndeliverable(err) {
			fmt.Fprintf(in.errWriter(),
				"[bundle] Warning: env %s's bundle %s was written but not recorded: %v\n"+
					"[bundle]   The build continues; recording is idempotent on (env, digest), so the next build or deploy records it.\n",
				env, shortDigest(placed.Digest), err)
			return placed.bundleWriteOutcome, nil
		}
		return placed.bundleWriteOutcome, err
	}
	stored, created, err := recorder.RecordBundle(ctx, record, blobs)
	switch {
	case err == nil:
		out := placed.bundleWriteOutcome
		out.Recorded, out.Created = true, created
		if stored.Digest != "" && stored.Digest != record.Digest {
			// The server recorded a different digest than the bytes
			// forge pushed. That can only mean the two disagree about
			// what was sent, and every reader downstream would believe
			// the record.
			return out, fmt.Errorf(
				"the control plane recorded bundle %s for env %s, but forge pushed %s — "+
					"the record and the artifact do not describe the same bytes",
				stored.Digest, env, record.Digest)
		}
		return out, nil
	case errors.Is(err, errControlPlanePredatesBundles):
		fmt.Fprintf(in.errWriter(),
			"[bundle] %s predates bundles; upgrade it.\n"+
				"[bundle]   Env %s's bundle %s was written but not recorded; the build continues on the pre-bundle path.\n",
			errControlPlanePredatesBundles.Error(), env, shortDigest(placed.Digest))
		return placed.bundleWriteOutcome, nil
	case declarationUndeliverable(err):
		fmt.Fprintf(in.errWriter(),
			"[bundle] Warning: env %s's bundle %s was written but not recorded: %v\n"+
				"[bundle]   The build continues; recording is idempotent on (env, digest), so the next build or deploy records it.\n",
			env, shortDigest(placed.Digest), err)
		return placed.bundleWriteOutcome, nil
	default:
		return placed.bundleWriteOutcome, fmt.Errorf("record env %s's bundle in %s: %w", env, ledger.Releases.Location(), err)
	}
}

// bundleRecorderFor is the bundle half of an env's ledger: the control plane
// when the env declares one, this machine's ledger otherwise.
//
// It reads the SAME envLedger the release and the promotion came from, rather
// than re-resolving the declaration. A records store chosen by a different
// rule than the ledger it records into could put one env's bundles and its
// promotions in two different places.
// bundleLedgerFor and bundleRecorderForEnv are the env's ledger and its
// bundle half, as vars.
//
// Seams rather than mocked interfaces, because what a test needs to STATE here
// is which BACKEND the env selected — the one decision that routes the bytes —
// and that decision is made by rendering the env's KCL and resolving a
// credential. A test of "a hosted env pushes and records" would otherwise have
// to stand up a control plane to assert a placement rule.
var (
	bundleLedgerFor      = ledgerFor
	bundleRecorderForEnv = bundleRecorderFor
)

func bundleRecorderFor(ctx context.Context, projectDir, env string, ledger envLedger) (bundleRecorder, error) {
	if !ledger.Hosted {
		return recordStoreFor(projectDir)
	}
	entities, err := RenderKCL(ctx, projectDir, env)
	if err != nil {
		return nil, fmt.Errorf("render deploy/kcl/%s to reach its control plane: %w", env, err)
	}
	client, _, err := envDeclarationClient(env, entities)
	if err != nil {
		return nil, err
	}
	return hostedRecordStoreFor(client, hostedEnvRefFor(env, entities).Project), nil
}

// printBundleWrites is the one-line-per-env report.
func printBundleWrites(out io.Writer, written []bundleWriteOutcome) {
	for _, b := range written {
		if b.Skipped {
			fmt.Fprintf(out, "[bundle] env %s: no bundle written (the env could not be projected)\n", b.Env)
			continue
		}
		where := "locally"
		if b.Pushed {
			where = "pushed"
		}
		state := "recorded"
		switch {
		case !b.Recorded:
			state = "NOT recorded"
		case !b.Created:
			state = "already recorded"
		}
		fmt.Fprintf(out, "[bundle] env %s: %s (%d object(s)), %s, %s\n",
			b.Env, shortDigest(b.Digest), b.Objects, where, state)
		fmt.Fprintf(out, "[bundle]   %s\n", b.Reference)
	}
}

// parseBundleEnvs reads --bundle-envs, defaulting to the env being built.
//
// An empty entry is dropped rather than refused: `--bundle-envs prod,` is a
// trailing comma, not a request to bundle an env with no name.
func parseBundleEnvs(flag, env string) []string {
	if strings.TrimSpace(flag) == "" {
		return []string{env}
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range strings.Split(flag, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(out) == 0 {
		return []string{env}
	}
	return out
}

// errWriter is in.errOut, or stderr.
func (in bundleBuildInputs) errWriter() io.Writer {
	if in.errOut != nil {
		return in.errOut
	}
	return progressWriter(true)
}
