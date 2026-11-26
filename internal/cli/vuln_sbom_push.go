package cli

// `forge vuln sbom push --release <v> [--env <env>] [--sbom <artifact>=<digest>]...`:
// re-send a release's SBOMs from this machine's ledger to the control plane
// that keeps the env's ledger (sbom_push.go does the sending, the same code
// a cut runs).
//
// Exit codes: 0 every applicable artifact's SBOM was sent or the control
// plane already holds one; 2 could not determine (an artifact with no SBOM
// this ledger can prove, an upload that failed, a release the control plane
// does not have); 4 refused (no sign-in, no scope, or a control plane that
// does not take SBOMs).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cloud"
)

// vulnSBOMPushSchema is the --json document's schema
// (`forge project schema vuln-sbom-push`).
const vulnSBOMPushSchema = "forge.dev/vuln-sbom-push/v1"

// vulnSBOMPushDocument is what `forge vuln sbom push --json` prints.
type vulnSBOMPushDocument struct {
	Schema string `json:"schema"`
	// Env is the env whose control plane was sent to.
	Env string `json:"env"`
	// Release is the release whose SBOMs were sent.
	Release string `json:"release"`
	// Endpoint is the control plane.
	Endpoint string `json:"endpoint"`
	// Artifacts is every artifact of the release, with its outcome.
	Artifacts []sbomPushArtifact `json:"artifacts"`
	// Summary counts the outcomes.
	Summary sbomPushCounts `json:"summary"`
	// Notes say what the outcomes cannot (how the mapping was found, what
	// could not be read).
	Notes []string `json:"notes"`
	// ExitCode is the exit code the command chose: 0 every applicable
	// SBOM is on the control plane, 2 could not determine, 4 refused.
	ExitCode int `json:"exit_code"`
	// Error is why the command stopped, when it did.
	Error string `json:"error,omitempty"`
}

type vulnSBOMPushOptions struct {
	release string
	env     string
	sboms   []string
	asJSON  bool
}

func newVulnSBOMPushCmd(deps vulnDeps) *cobra.Command {
	var opts vulnSBOMPushOptions
	cmd := &cobra.Command{
		Use:   "push --release <v> [--env <env>] [--sbom <artifact>=<digest>]...",
		Short: "Re-send a release's SBOMs from this machine's ledger to the env's control plane",
		Long: `Send the SBOMs a release recorded, from this machine's ledger, to the control
plane that keeps the env's ledger (RecordReleaseSBOM): for a release cut while
the control plane did not take SBOMs yet, or whose upload failed. A cut sends
them itself, and re-sends the releases an older control plane refused on the
next cut to it; this sends one release now.

It is safe to run again: the control plane keeps one SBOM per artifact of a
release, answers "already recorded" for the one it holds, and keeps its own
when sent a different one ("differs"). Per artifact it reports sent, already
recorded, differs, missing (no SBOM this ledger can prove describes it),
failed, or not applicable (a git source pin, source maps).

Which SBOM describes an artifact: --sbom; the SBOM the control plane already
names; this machine's index of the release, which every cut writes; and for a
release cut before that index existed, the SBOM files themselves, by their
CycloneDX metadata (component name = the artifact, version = its digest).
A static site's SBOM records its lockfile's digest, not the bundle's, so it
cannot be proven that way: the output lists the SBOMs that name it, and
--sbom <artifact>=<sha256:digest> names the one to send (the bytes are still
verified against the digest, and must name the artifact).

--env defaults to the one env whose ledger is a control plane. An env named
"push" is read with ` + "`forge vuln sbom -- push`" + `.

Exit codes: 0 every applicable artifact's SBOM is on the control plane (sent
or already there); 2 could not determine (an artifact missing an SBOM, an
upload that failed, a release the control plane does not have); 4 refused (no
sign-in or scope, or a control plane that does not take SBOMs).

--json prints forge.dev/vuln-sbom-push/v1 (` + "`forge project schema vuln-sbom-push`" + `).

Examples:
  forge vuln sbom push --release v1.4.0
  forge vuln sbom push --release v1.4.0 --env prod --json
  forge vuln sbom push --release v1.4.0 --sbom registry.example.com/acme/web/static.v1=sha256:…`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runVulnSBOMPush(cmd.Context(), cmd.OutOrStdout(), deps, opts)
		},
	}
	cmd.Flags().StringVar(&opts.release, "release", "", "the release whose SBOMs to send (required)")
	cmd.Flags().StringVar(&opts.env, "env", "", "the env whose control plane to send to (default: the one env whose ledger is a control plane)")
	cmd.Flags().StringArrayVar(&opts.sboms, "sbom", nil, "<artifact>=<sha256:digest>: send this SBOM from this machine's ledger for the artifact (repeatable)")
	cmd.Flags().BoolVar(&opts.asJSON, "json", false, "print the forge.dev/vuln-sbom-push/v1 document")
	_ = cmd.MarkFlagRequired("release")
	return cmd
}

// parseSBOMOverrides reads --sbom <artifact>=<digest> flags.
func parseSBOMOverrides(flags []string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range flags {
		i := strings.LastIndex(f, "=")
		if i <= 0 || i == len(f)-1 {
			return nil, fmt.Errorf("--sbom %q: want <artifact>=<sha256:digest>", f)
		}
		name, digest := strings.TrimSpace(f[:i]), strings.TrimSpace(f[i+1:])
		if prev, ok := out[name]; ok && prev != digest {
			return nil, fmt.Errorf("--sbom names two SBOMs for %s (%s, %s)", name, prev, digest)
		}
		out[name] = digest
	}
	return out, nil
}

func runVulnSBOMPush(ctx context.Context, out io.Writer, deps vulnDeps, opts vulnSBOMPushOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	doc := vulnSBOMPushDocument{Schema: vulnSBOMPushSchema, Release: strings.TrimSpace(opts.release), Env: opts.env,
		Artifacts: []sbomPushArtifact{}, Notes: []string{}}
	err := vulnSBOMPush(ctx, deps, opts, &doc)
	if err == nil {
		err = sbomPushExit(doc)
	}
	doc.ExitCode = 0
	if err != nil {
		doc.ExitCode = 1
		var ec *exitCodeError
		if errors.As(err, &ec) {
			doc.ExitCode = ec.code
			if !ec.verdict {
				doc.Error = ec.msg
			}
		} else {
			doc.Error = err.Error()
		}
	}
	if opts.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if werr := enc.Encode(doc); werr != nil {
			return werr
		}
	} else if len(doc.Artifacts) > 0 {
		writeSBOMPush(out, doc)
	}
	return err
}

// vulnSBOMPush fills doc; an error is why it stopped (a refusal, or a
// release it could not read).
func vulnSBOMPush(ctx context.Context, deps vulnDeps, opts vulnSBOMPushOptions, doc *vulnSBOMPushDocument) error {
	if doc.Release == "" {
		return fmt.Errorf("--release is required")
	}
	overrides, err := parseSBOMOverrides(opts.sboms)
	if err != nil {
		return err
	}
	projectDir := deps.projectDir()
	env, err := sbomPushEnv(ctx, deps, projectDir, opts.env)
	if err != nil {
		return err
	}
	doc.Env, doc.Endpoint = env.Name, env.Endpoint
	endpoint := env.Endpoint
	if env.decl != nil {
		if ep, err := projectEndpoint(env.Name, env.decl); err == nil {
			endpoint = ep.URL
			doc.Endpoint = ep.URL
		}
	}
	if deps.hostedClient == nil {
		return undeterminedf("env %q keeps its ledger on the control plane at %s, which this command cannot reach", env.Name, env.Endpoint)
	}
	client, err := deps.hostedClient(ctx, env)
	if err != nil {
		return err
	}
	f, err := readHostedVulnFeatures(ctx, client)
	if err != nil {
		return sbomPushReadError(env, "ask it which vulnerability features it serves", err)
	}
	if !f.acceptsUploads() {
		return &exitCodeError{code: exitRefused, msg: fmt.Sprintf("refused: the control plane at %s does not take SBOMs yet (it advertises no %s capability); nothing was sent. "+
			"Release %s's SBOMs stay in this machine's ledger: run this again once it does", env.Endpoint, hostedVulnFeatureSBOMUpload, doc.Release)}
	}
	rel, err := hostedLedger(client, env.Endpoint, env.Project, "").Releases.Get(ctx, doc.Release)
	if err != nil {
		return sbomPushReadError(env, "read release "+doc.Release, err)
	}
	if rel == nil {
		return undeterminedf("release %s is not on the control plane at %s (project %q): nothing to send its SBOMs to", doc.Release, env.Endpoint, env.Project)
	}
	var remote map[string]string
	if f.Findings {
		rv, err := hostedVulnReader{client: client, project: env.Project}.releaseVulns(ctx, doc.Release, "", false)
		if err == nil {
			remote = map[string]string{}
			for _, a := range rv.Artifacts {
				if a.SBOMDigest != "" {
					remote[a.ArtifactName] = a.SBOMDigest
				}
			}
		} else {
			doc.Notes = append(doc.Notes, fmt.Sprintf("could not read which SBOMs the control plane records (%v): each SBOM is sent, and its answer says whether it already had one", err))
		}
	} else {
		doc.Notes = append(doc.Notes, "the control plane does not say which SBOMs it records (no "+hostedVulnFeatureFindings+" capability): each SBOM is sent, and its answer says whether it already had one")
	}
	store, err := deps.machineLedger(projectDir)
	if err != nil {
		return undeterminedf("open this machine's ledger: %v", err)
	}
	for name := range overrides {
		if _, ok := rel.Artifacts[name]; !ok {
			return fmt.Errorf("--sbom %s: release %s has no such artifact", name, doc.Release)
		}
	}
	res := pushReleaseSBOMs(ctx, sbomUploader{client: client, endpoint: endpoint, project: env.Project}, store, sbomPushRun{
		Version: doc.Release, Project: env.Project, CutAt: rel.CreatedAt, Artifacts: rel.Artifacts,
		Overrides: overrides, Remote: remote, Fallback: true,
	})
	doc.Artifacts = res.Artifacts
	doc.Summary = countSBOMPush(res.Artifacts)
	doc.Notes = append(doc.Notes, res.Notes...)
	if from := sbomPushSources(res.Artifacts); from != "" {
		doc.Notes = append(doc.Notes, "SBOMs found by: "+from)
	}
	switch {
	case res.Unimplemented:
		return &exitCodeError{code: exitRefused, msg: fmt.Sprintf("refused: the control plane at %s does not store SBOMs yet (RecordReleaseSBOM is Unimplemented); release %s's SBOMs stay in this machine's ledger", env.Endpoint, doc.Release)}
	case res.Refused != nil:
		return sbomPushReadError(env, "send release "+doc.Release+"'s SBOMs", res.Refused)
	case res.NotFound:
		return undeterminedf("the control plane at %s does not know release %s: nothing was recorded", env.Endpoint, doc.Release)
	}
	return nil
}

// sbomPushEnv is --env, else the one env whose ledger is a control plane.
func sbomPushEnv(ctx context.Context, deps vulnDeps, projectDir, name string) (vulnEnv, error) {
	if name != "" {
		env, err := deps.resolveEnv(ctx, projectDir, name)
		if err != nil {
			return env, undeterminedf("env %q: %v", name, err)
		}
		if !env.hosted() {
			return env, &exitCodeError{code: exitRefused, msg: fmt.Sprintf("refused: env %q keeps its ledger on this machine, whose releases already hold their SBOMs; there is no control plane to send them to", name)}
		}
		return env, nil
	}
	list := deps.listEnvs
	if list == nil {
		list = ListEnvs
	}
	names, err := list(projectDir)
	if err != nil {
		return vulnEnv{}, undeterminedf("list this project's envs: %v", err)
	}
	var hosted []vulnEnv
	for _, n := range names {
		env, err := deps.resolveEnv(ctx, projectDir, n)
		if err == nil && env.hosted() {
			hosted = append(hosted, env)
		}
	}
	switch len(hosted) {
	case 1:
		return hosted[0], nil
	case 0:
		return vulnEnv{}, &exitCodeError{code: exitRefused, msg: "refused: no env of this project keeps its ledger on a control plane, so there is nowhere to send SBOMs (name one with --env)"}
	}
	var hn []string
	for _, e := range hosted {
		hn = append(hn, e.Name)
	}
	sort.Strings(hn)
	return vulnEnv{}, fmt.Errorf("several envs keep their ledger on a control plane (%s): name one with --env", strings.Join(hn, ", "))
}

// sbomPushReadError classifies a failed call: a refusal (no sign-in, no
// scope) is exit 4, anything else could not determine (exit 2).
func sbomPushReadError(env vulnEnv, what string, err error) error {
	var ec *exitCodeError
	if errors.As(err, &ec) {
		return err
	}
	if hostedErrorHasCode(err, cloud.CodePermissionDenied) || hostedErrorHasCode(err, cloud.CodeUnauthenticated) {
		return &exitCodeError{code: exitRefused, msg: fmt.Sprintf("refused: the control plane at %s would not let this caller %s (env %q): %v", env.Endpoint, what, env.Name, err)}
	}
	return undeterminedf("could not %s on the control plane at %s (env %q): %v", what, env.Endpoint, env.Name, err)
}

// sbomPushExit is the exit code of a push that ran: 2 when an applicable
// artifact has no SBOM on the control plane afterwards.
func sbomPushExit(doc vulnSBOMPushDocument) error {
	c := doc.Summary
	if c.Missing+c.Failed == 0 {
		return nil
	}
	var need []string
	for _, a := range doc.Artifacts {
		if a.NeedsOverride {
			need = append(need, a.Artifact)
		}
	}
	msg := fmt.Sprintf("could not determine: %d artifact(s) of release %s have no SBOM on the control plane (%d missing, %d failed)", c.Missing+c.Failed, doc.Release, c.Missing, c.Failed)
	if len(need) > 0 {
		msg += "; name an SBOM with --sbom <artifact>=<digest> for " + strings.Join(need, ", ")
	}
	return &exitCodeError{code: exitUndetermined, verdict: true, msg: msg}
}

// sbomPushSources names where the sent SBOMs were found, "index ×3,
// metadata ×2".
func sbomPushSources(artifacts []sbomPushArtifact) string {
	n := map[string]int{}
	for _, a := range artifacts {
		if a.From != "" {
			n[a.From]++
		}
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s ×%d", strings.ReplaceAll(k, "_", " "), n[k]))
	}
	return strings.Join(parts, ", ")
}

// writeSBOMPush prints the document as text.
func writeSBOMPush(w io.Writer, doc vulnSBOMPushDocument) {
	_, _ = fmt.Fprintf(w, "release %s → %s (env %s)\n", doc.Release, doc.Endpoint, doc.Env)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, a := range doc.Artifacts {
		detail := a.SBOMDigest
		if a.From != "" {
			detail += " (" + strings.ReplaceAll(a.From, "_", " ") + ")"
		}
		if a.Reason != "" {
			detail = strings.TrimSpace(detail + " " + a.Reason)
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", strings.ReplaceAll(a.Status, "_", " "), a.Artifact, detail)
		for _, c := range a.Candidates {
			_, _ = fmt.Fprintf(tw, "  \t\t  candidate %s\n", c)
		}
	}
	_ = tw.Flush()
	c := doc.Summary
	_, _ = fmt.Fprintf(w, "%d sent, %d already recorded, %d differ, %d missing, %d failed, %d not applicable\n",
		c.Sent, c.Already, c.Differs, c.Missing, c.Failed, c.NotApplicable)
	for _, n := range doc.Notes {
		_, _ = fmt.Fprintf(w, "note: %s\n", n)
	}
}
