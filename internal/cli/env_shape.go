// File: internal/cli/env_shape.go
//
// `forge env shape <env>` — the env's rendered DECLARATION, as a value.
//
// This is the projection half of `forge env render`. Where render prints the
// manifests themselves — two megabytes of YAML for control-plane's prod —
// this prints what they ARE: the env's kind, what runs where, which secrets
// it needs, which domains it binds, and one hash per object. Small enough to
// store per environment, read per view, and diff as a set.
//
// WHY IT IS A COMMAND AND NOT ONLY AN INTERNAL STEP. Three callers need the
// same answer, and all three must get the same bytes:
//
//   - `forge env build` and `forge env deploy` RECORD it on the control
//     plane, which is what lets the Live view answer "what kind, which
//     secrets, which provider" with no checkout and no daemon running;
//   - the reliant daemon exposes this command as `forge.env_shape`, which is
//     what Preview's "Register" sends to EnsureEnvironment to bootstrap an
//     env the control plane has never heard of;
//   - F7's `forge env diff` compares this projection against the recorded
//     one.
//
// They share internal/bundle.ProjectShape, so a shape a user read, a shape
// the control plane stored, and a shape a bundle sealed cannot disagree about
// the same render.
//
// READ-ONLY, AND WATCHED RATHER THAN PROMISED. Like `forge env render`, this
// cannot guarantee purity — KCL evaluates `file.write` during evaluation and
// forge has no hook to suppress a project's own writes. So it does what
// render does and goes one step further: it scans the tree before and after,
// and a render that wrote anything is an ERROR naming the paths, not a
// warning. A caller registering an environment from this output is making a
// durable record, and a record derived from an impure render is a record
// nobody can reproduce.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/bundle"
	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/pkg/release"
)

// envShapeDoc is what the command prints, and what the daemon's
// `forge.env_shape` hook returns.
//
// Project and Env are carried alongside the shape rather than left implicit,
// because the consumer is addressing a control-plane environment, whose
// identity is (org, project, name) — a shape with no project is a shape
// nobody can record. Kind is duplicated out of the shape for the same
// reason a header exists: it is the one field a reader branches on.
type envShapeDoc struct {
	Project    string             `json:"project"`
	Env        string             `json:"env"`
	Kind       string             `json:"kind"`
	Shape      release.Shape      `json:"shape"`
	Provenance release.Provenance `json:"provenance"`
}

func newEnvShapeCmd() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "shape <environment>",
		Short: "Print what an environment DECLARES — kind, workloads, secrets, domains, and one hash per object",
		Args:  cobra.ExactArgs(1),
		Long: `Print the projection of deploy/kcl/<env>/'s render: what the environment is,
rather than the manifests it produces.

` + "`" + Name() + ` env render` + "`" + ` prints the objects themselves — megabytes of YAML for a
real environment. This prints the SHAPE: the environment's kind (persistent,
self-managed or local, derived from what it binds), each workload with its
runtime and cluster, each declared secret by NAME with its provider, the
domains it binds, the clusters it deploys to, and one hash per rendered
object. It is small enough to store per environment and compare as a set,
which is what makes "what changed" answerable without re-rendering anything.

This is the SAME projection ` + "`" + Name() + ` env build` + "`" + ` records on the control plane, so
what you read here is what a Live view shows and what a deploy plan is
computed against.

IT CARRIES NO SECRET VALUE, ever. Declared secrets appear as names and
providers. A rendered ` + "`" + `kind: Secret` + "`" + ` has every value replaced by the hash of
that value before the object is hashed, so a changed secret is still visible
while the value itself is not recorded.

READ-ONLY, AND CHECKED. No cluster is contacted, no image is built, nothing
is pushed. forge cannot promise the render is side-effect-free, because KCL
evaluates ` + "`" + `file.write` + "`" + ` — so this scans the project before and after, and a
render that wrote anything FAILS, naming the paths. A declaration derived
from an impure render is one nobody can reproduce.

Examples:
  ` + Name() + ` env shape prod               # the human summary
  ` + Name() + ` env shape prod --json        # {project, env, kind, shape, provenance}`,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnvShape(cmd, args[0], asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print {project, env, kind, shape, provenance} as JSON (the form the reliant daemon's forge.env_shape hook returns)")
	return cmd
}

// runEnvShape renders the env and prints its declaration. STDOUT CARRIES ONLY
// THE DOCUMENT in --json mode, for the same reason `forge env render` guards
// its stream: the render path prints notes with fmt.Printf in dozens of
// places, and one prose line on stdout makes the JSON unparseable. Diverting
// os.Stdout makes that structural rather than a property of every callee.
func runEnvShape(cmd *cobra.Command, envName string, asJSON bool) error {
	out := cmd.OutOrStdout()
	realStdout := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = realStdout }()

	doc, err := projectEnvShapeFn(cmd.Context(), cmd.ErrOrStderr(), envName)
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(doc)
	}
	writeShapeSummary(out, doc)
	return nil
}

// projectEnvShapeFn is the projection every caller goes through, as a var so
// the tests that pin the WIRE — which call carries which fields, in which
// order — can supply a fixed shape instead of evaluating KCL. The projection
// itself is tested in internal/bundle, from a literal fixture.
var projectEnvShapeFn = projectEnvShape

// projectEnvShape is the shared projection: render the env read-only, refuse
// an impure render, and project it.
//
// `forge env shape`, `forge env build` and `forge env deploy` all come
// through here, which is the point — the declaration a build records and the
// declaration a user reads are produced by one function, from one render.
func projectEnvShape(ctx context.Context, errOut io.Writer, envName string) (envShapeDoc, error) {
	projectDir := projectDirForKCL()

	// Pin FORGE'S OWN derived state before scanning, so the purity verdict
	// below is about the PROJECT's render and not about forge's tooling.
	// pinFile reverts a path only if the render actually moved it, so an
	// untouched file keeps even its mtime and stays out of the report as a
	// phantom write.
	//
	// Two paths qualify. The resolve_port store is the one `forge env
	// render` already pins. kcl.mod.lock is written by kpm — which forge
	// invokes to resolve the KCL module, and which writes the lock on
	// every resolve — so it appears on the FIRST render of any project and
	// would make this command refuse every env it had not already
	// rendered once. It is derived state by forge's own account
	// (kcl/embed.go), and a declaration's reproducibility does not turn on
	// it.
	//
	// The scan is armed AFTER the pins and BEFORE the dev-stack
	// activation, because deciding the render's purpose is itself a
	// render, and any file.write it fires belongs in the report.
	unpin := pinPaths(
		filepath.Join(projectDir, ".forge", "ports-"+envName+".json"),
		filepath.Join(projectDir, "deploy", "kcl", "kcl.mod.lock"),
	)
	scan := newRenderWriteScan(projectDir, false)
	activateDevStack(ctx, projectDir, envName, renderDeclaration, inspectBlocks)
	defer func() {
		unpin()
		scan.report(errOut, envName)
		reportDeclinedWrites(errOut, kclplugin.SuppressedWrites())
	}()

	doc, err := renderEnvShape(ctx, errOut, projectDir, envName)
	if err != nil {
		return envShapeDoc{}, err
	}

	// The write verdict, taken after the render and before the caller can
	// act on the document. `forge env render` makes this opt-in
	// (--fail-on-write) because its job is to show you the objects even on
	// a project that writes files. A DECLARATION is different: it is
	// recorded, and a record derived from a render nobody can reproduce is
	// worse than no record.
	unpin()
	if changes := scan.changes(); len(changes) > 0 {
		return envShapeDoc{}, fmt.Errorf("refusing to project env %q: the render wrote %d file(s) (see the report on stderr), so this declaration is not reproducible.\n"+
			"  KCL evaluates file.write during evaluation and forge cannot suppress a project's own writes.\n"+
			"  fix: move the generated file out of the render (forge's fp.write_file is materialized by `forge env up`, not by a render)",
			envName, len(changes))
	}
	return doc, nil
}

// renderEnvShape is the render itself: resolve the env's tag and digests the
// way a deploy would, render the manifests, attribute each document to its
// cluster(s) through the deploy path's own router, and project the result.
func renderEnvShape(ctx context.Context, errOut io.Writer, projectDir, envName string) (envShapeDoc, error) {
	// The project config supplies two facts, and BOTH have a fallback, so a
	// config that does not load is not fatal here. That is deliberate:
	// `forge env build` has always worked against an incomplete forge.yaml,
	// and making the declaration the thing that newly refuses it would
	// break a working command for a reason unrelated to what the user
	// asked for. The project NAME is resolved separately, through
	// hostedProjectName, which tolerates the same failure.
	kclDir, fallbackNamespace := "deploy/kcl", envName
	if store, err := loadProjectStore(); err == nil {
		if declared := store.K8s().KCLDir; declared != "" {
			kclDir = declared
		}
		fallbackNamespace = store.Meta().Name + "-" + envName
	}
	mainK := filepath.Join(kclDir, envName, "main.k")
	if _, serr := os.Stat(mainK); serr != nil {
		envs, _ := ListEnvs(projectDir)
		return envShapeDoc{}, fmt.Errorf("environment %q not found: %s does not exist (declared environments: %s)",
			envName, mainK, strings.Join(envs, ", "))
	}

	entities, kerr := RenderKCL(ctx, projectDir, envName)
	if kerr != nil {
		return envShapeDoc{}, fmt.Errorf("render deploy/kcl/%s/: %w", envName, kerr)
	}

	namespace := k8sClusterFieldFromEntities(entities, "namespace")
	if namespace == "" {
		namespace = fallbackNamespace
	}
	imageTag, _ := renderImageTag(ctx, projectDir, envName, "")

	// The digests a deploy would pin. Unresolvable digests are NOT fatal
	// here, for the reason `forge env render` gives: pinning is an
	// optimisation of which bytes ship, not of what the env declares, and a
	// shape on the mutable tag is more use to a reader than no shape. The
	// per-object ConfigHash is unaffected either way — it normalizes
	// release-bound digests away by construction.
	var digests map[string]string
	if ledger, lerr := ledgerFor(ctx, projectDir, envName); lerr == nil {
		if d, _, derr := resolveDeployDigests(ctx, projectDir, envName, false, ledger.Bindings, ledger.Releases); derr == nil {
			digests = d
		} else {
			fmt.Fprintf(errOut, "warning: image digests unresolved (%v) — projecting on tag %q instead\n", derr, imageTag)
		}
	}

	manifests, rerr := cluster.RenderManifests(ctx, mainK, imageTag, namespace, envName, loadDeployEnvConfigKV(projectDir, envName), digests)
	if rerr != nil {
		return envShapeDoc{}, fmt.Errorf("render %s: %w", mainK, rerr)
	}
	groups, gerr := buildDeployGroupsForEnv(envName, entities, namespace, imageTag, false)
	if gerr != nil {
		return envShapeDoc{}, gerr
	}
	objects, clusters := attributeRenderedObjects(manifests, groups, entities)

	// Helm charts are deliberately NOT templated. A declaration must be
	// derivable with no network and no `helm` on PATH — Preview's Register
	// runs on whatever machine the daemon is on — and a chart's objects are
	// the platform dependency's, not this project's declaration. `forge env
	// render --charts` is where they belong.
	shape, err := bundle.ProjectShape(bundle.ShapeInput{
		Kind:              shapeEnvKindOf(entities),
		Workloads:         shapeWorkloadsOf(entities),
		Secrets:           shapeSecretsOf(entities),
		Domains:           shapeDomainsOf(entities),
		Clusters:          clusters,
		Manifests:         renderShapeStream(objects),
		Images:            digests,
		StatefulWorkloads: shapeStatefulWorkloadsOf(entities),
	})
	if err != nil {
		return envShapeDoc{}, fmt.Errorf("project env %q: %w", envName, err)
	}
	return envShapeDoc{
		Project:    hostedProjectName(),
		Env:        envName,
		Kind:       string(shape.Kind),
		Shape:      shape,
		Provenance: captureBuildProvenance(ctx, projectDir).ForHosted(),
	}, nil
}

// pinPaths pins several paths at once and returns one revert for all of
// them, idempotent so the caller can take the write verdict early and still
// revert in a defer.
func pinPaths(paths ...string) func() {
	unpins := make([]func(), 0, len(paths))
	for _, path := range paths {
		unpins = append(unpins, pinFile(path))
	}
	var done bool
	return func() {
		if done {
			return
		}
		done = true
		for _, unpin := range unpins {
			unpin()
		}
	}
}

// renderShapeStream is the annotated stream internal/bundle parses: the same
// bytes `forge env render` prints, header included.
//
// Handing the PRINTED form across the package boundary rather than a parsed
// structure is what keeps the routing in one place. internal/bundle has no
// opinion about which cluster a document lands on; it reads the answer that
// internal/cluster's own router computed. A second model of those rules in
// the projection would eventually disagree with the deploy, and a shape that
// disagrees with the deploy describes a deploy that never happens.
func renderShapeStream(objects []renderedObject) string {
	var b strings.Builder
	if err := writeRenderedStream(&b, objects); err != nil {
		// writeRenderedStream only fails on its writer, and a
		// strings.Builder cannot fail.
		panic("render the shape stream: " + err.Error())
	}
	return b.String()
}

// ─── The declaration half of the projection ─────────────────────────────────

// shapeEnvKindOf is the env's kind in pkg/release vocabulary. An env that
// declares no control plane is still LOCAL to a shape: the file ledger (F3)
// records it, and a shape has to say how the env runs whoever stores it.
func shapeEnvKindOf(e *KCLEntities) release.EnvKind {
	if name := hostedControlPlaneKindName(hostedEnvKindOf(e)); name != "" {
		return release.EnvKind(name)
	}
	if runsOnOwnCluster(e) {
		return release.EnvSelfManaged
	}
	return release.EnvLocal
}

// shapeWorkloadsOf projects every workload: its name, the runtime it resolved
// to, the cluster it lands on, the release artifact its image comes from, and
// its replica count.
//
// EVERY workload, of every runtime — host and build-only included. "which
// workloads does this env declare" is the question, and a projection that
// listed only the cluster ones would describe control-plane's dev, where most
// services run on the host, as nearly empty.
func shapeWorkloadsOf(e *KCLEntities) []release.ShapeWorkload {
	if e == nil {
		return nil
	}
	out := make([]release.ShapeWorkload, 0, len(e.Workloads)+len(e.Frontends)+len(e.Databases))
	for i := range e.Workloads {
		w := &e.Workloads[i]
		item := release.ShapeWorkload{Name: w.Name, Runtime: w.Runtime.Type, Artifact: w.Image}
		if c := w.Runtime.Cluster; c != nil {
			item.Cluster = c.Cluster
		}
		if r := w.Spec.Replicas; r > 0 {
			replicas := int(r)
			item.Replicas = &replicas
		}
		out = append(out, item)
	}
	for _, f := range e.Frontends {
		out = append(out, release.ShapeWorkload{Name: f.Name, Runtime: f.Runtime.Type, Artifact: f.Name})
	}
	// A managed database is a runnable thing the env declares, so it is a
	// workload in the shape even though it is not a WorkloadEntity: it
	// occupies a cluster, it holds data, and a reader asking "what does
	// this env run" must be told about it.
	for _, d := range e.Databases {
		out = append(out, release.ShapeWorkload{Name: d.Name, Runtime: d.Runtime, Cluster: d.Cluster})
	}
	return out
}

// shapeSecretsOf projects the env's declared secrets: NAMES, their provider,
// and which workloads read each one.
//
// This is what #353's set-secret form reads, which is why declared_by matters
// — "DATABASE_URL, needed by admin-server and the workers" is the sentence a
// user needs when the form asks them for a value, and it is a fact only the
// render knows.
//
// Keyed on the SECRET name (ev.SecretRef), not the env-var name: one Secret
// typically carries several keys, and the provider stores a Secret.
func shapeSecretsOf(e *KCLEntities) []release.ShapeSecret {
	if e == nil {
		return nil
	}
	provider := shapeSecretProviderOf(e)
	readers := map[string]map[string]bool{}
	for i := range e.Workloads {
		w := &e.Workloads[i]
		for _, ref := range secretRefsForService(w) {
			if readers[ref.SecretName] == nil {
				readers[ref.SecretName] = map[string]bool{}
			}
			readers[ref.SecretName][w.Name] = true
		}
	}
	// A declared external prerequisite is part of the env's secret surface
	// even when no workload in this render reads it: forge does not create
	// it, and "this env needs a Secret nobody has set" is exactly what a
	// declaration exists to say.
	for _, req := range e.RequiredSecrets {
		if readers[req.Name] == nil {
			readers[req.Name] = map[string]bool{}
		}
	}
	names := make([]string, 0, len(readers))
	for name := range readers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]release.ShapeSecret, 0, len(names))
	for _, name := range names {
		declaredBy := make([]string, 0, len(readers[name]))
		for workload := range readers[name] {
			declaredBy = append(declaredBy, workload)
		}
		sort.Strings(declaredBy)
		out = append(out, release.ShapeSecret{Name: name, Provider: provider, DeclaredBy: declaredBy})
	}
	return out
}

// shapeSecretProviderOf is WHERE this env's secret values come from, in
// forge's provider vocabulary. "hosted" is normalized to the control plane's
// own word for it so a reader does not have to know forge's spelling.
func shapeSecretProviderOf(e *KCLEntities) string {
	if e == nil || e.SecretProvider == nil {
		return ""
	}
	return strings.ToLower(e.SecretProvider.Type)
}

// shapeDomainsOf is every hostname the env binds: a Gateway's own host, each
// of its listeners' hostname overrides, and every route's override. Sorted
// and de-duplicated by Shape.Canonical.
func shapeDomainsOf(e *KCLEntities) []string {
	if e == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(host string) {
		if host != "" && !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	for _, g := range e.Gateways {
		add(g.Host)
		for _, l := range g.Listeners {
			add(l.Hostname)
		}
	}
	for _, r := range e.HTTPRoutes {
		add(r.Host)
	}
	for _, r := range e.GRPCRoutes {
		add(r.Host)
	}
	return out
}

// shapeStatefulWorkloadsOf names the workloads whose objects hold data their
// KIND does not announce — the env's declared ManagedDatabases, each of which
// expands into a Postgres cluster plus its Services and volumes, all stamped
// with the database's name as their workload.
//
// The well-known stateful kinds (PVC, StatefulSet, Secret, Namespace, CRD)
// are recognised by pkg/release without being listed. This covers the case
// that would otherwise be invisible: a CloudNativePG `Cluster` is a custom
// resource like any other, and deleting it takes the volumes with it.
func shapeStatefulWorkloadsOf(e *KCLEntities) []string {
	if e == nil {
		return nil
	}
	out := make([]string, 0, len(e.Databases))
	for _, d := range e.Databases {
		out = append(out, d.Name)
	}
	return out
}

// ─── Recording ──────────────────────────────────────────────────────────────

// recordEnvDeclaration ensures the env on its control plane WITH its rendered
// shape and the provenance of that render.
//
// THIS IS THE WRITE THE WHOLE SLICE EXISTS FOR. After it, the control plane
// knows what the project declares for this env — so the Live view answers
// "what kind, which secrets, which provider" with no checkout, no daemon and
// no render, and a never-deployed env is "declared, not built yet" rather
// than absent.
//
// It runs BEFORE anything is built, pushed or cut, deliberately: a build that
// fails half-way has still told the control plane what the env is, which is
// the fact every other surface needs, and it costs one idempotent RPC.
//
// An env that declares no control plane records nothing and returns no error.
// That is not a failure — its ledger is the file ledger (F3), and refusing
// here would make `forge env build` depend on a control plane that a purely
// local env has no reason to have.
func recordEnvDeclaration(ctx context.Context, envName string, entities *KCLEntities) error {
	if entities == nil || entities.ControlPlane == nil {
		return nil
	}
	client, endpoint, err := envDeclarationClient(envName, entities)
	if err != nil {
		return err
	}
	doc, err := projectEnvShapeFn(ctx, os.Stderr, envName)
	if err != nil {
		return err
	}
	ref := hostedEnvRefFor(envName, entities)
	ref.Shape, ref.DeclaredBy = &doc.Shape, &doc.Provenance
	if _, err := ensureHostedEnv(ctx, client, ref); err != nil {
		// F-15, stated loudly. A control plane that predates the shape
		// IGNORES the field (connect's JSON codec discards unknown
		// fields), so there is nothing to special-case for an old
		// server. An InvalidArgument, though, means the server read the
		// shape and REFUSED it — and silently continuing would leave
		// every Live surface reading a stale declaration while the
		// build reported success.
		return fmt.Errorf("record env %q's declaration on the control plane at %s: %w", envName, endpoint, err)
	}
	fmt.Printf("[declare] env %s: %s, %d workload(s), %d secret(s), %d object(s) recorded at %s\n",
		envName, doc.Kind, len(doc.Shape.Workloads), len(doc.Shape.Secrets), len(doc.Shape.Objects), endpoint)
	return nil
}

// envDeclarationClient resolves the control-plane client for an env that
// declares one, from the env's own declaration — the same resolution the
// hosted ledger and the hosted deploy perform.
//
// A var so a test can point the declaration path at a fake caller. It
// resolves its own client rather than reusing the ledger's, because the
// declaration is recorded for an env that may have no ledger yet: a
// never-built env's whole purpose here is to become known BEFORE anything
// else exists for it.
var envDeclarationClient = func(envName string, entities *KCLEntities) (cloudCaller, string, error) {
	decl := declarationFromEntities(entities)
	ep, err := cloud.ResolveEndpoint(envName, decl)
	if err != nil {
		return nil, "", err
	}
	cred, err := cloud.ResolveCredential("", ep)
	if err != nil {
		return nil, "", fmt.Errorf("env %q records its declaration on the control plane at %s: %w", envName, ep.URL, err)
	}
	return cloud.NewClient(ep, cred), ep.URL, nil
}

// ─── Text rendering ─────────────────────────────────────────────────────────

// writeShapeSummary is the human form: the few lines someone reads to answer
// "what is this environment" before deciding anything.
func writeShapeSummary(w io.Writer, doc envShapeDoc) {
	fmt.Fprintf(w, "env %s (project %s): %s\n", doc.Env, emptyAs(doc.Project, "(unnamed)"), doc.Kind)
	fmt.Fprintf(w, "  clusters:  %s\n", describeClusters(doc.Shape.Clusters))
	fmt.Fprintf(w, "  workloads: %d\n", len(doc.Shape.Workloads))
	for _, workload := range doc.Shape.Workloads {
		where := workload.Runtime
		if workload.Cluster != "" {
			where += " on " + workload.Cluster
		}
		fmt.Fprintf(w, "    %s (%s)\n", workload.Name, where)
	}
	if len(doc.Shape.Secrets) > 0 {
		fmt.Fprintf(w, "  secrets:   %d (names only; no value is ever recorded)\n", len(doc.Shape.Secrets))
		for _, secret := range doc.Shape.Secrets {
			line := secret.Name
			if secret.Provider != "" {
				line += " [" + secret.Provider + "]"
			}
			if len(secret.DeclaredBy) > 0 {
				line += " ← " + strings.Join(secret.DeclaredBy, ", ")
			}
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	if len(doc.Shape.Domains) > 0 {
		fmt.Fprintf(w, "  domains:   %s\n", strings.Join(doc.Shape.Domains, ", "))
	}
	fmt.Fprintf(w, "  objects:   %d\n", len(doc.Shape.Objects))
	fmt.Fprintf(w, "  rendered from: %s\n", describeShapeProvenance(doc.Provenance))
}

// describeShapeProvenance is the one-line "where this came from": the commit,
// the branch, whether the tree was dirty, and the forge that rendered it. A
// render is a function of (forge, KCL, config), so the forge version is part
// of the answer and not a footnote.
func describeShapeProvenance(p release.Provenance) string {
	if p.Commit == "" {
		return "no git source (forge " + emptyAs(p.ForgeVersion, "unknown") + ")"
	}
	parts := []string{shortCommit(p.Commit)}
	if p.Branch != "" {
		parts = append(parts, "on "+p.Branch)
	}
	if p.Dirty {
		parts = append(parts, "DIRTY")
	}
	if p.Unhashed() {
		parts = append(parts, "tree unhashed")
	}
	return strings.Join(parts, " ") + " (forge " + emptyAs(p.ForgeVersion, "unknown") + ")"
}

func shortCommit(commit string) string {
	if len(commit) <= 12 {
		return commit
	}
	return commit[:12]
}
