package doctor

// deploy.go — deployability checks.
//
// The rest of doctor answers "is my dev stack healthy". These checks
// answer the question an SRE asks on day one: "would I let this reach
// production". They render the project's own deploy/kcl/<env> packages
// through the embedded KCL runtime — the same seam `forge env up` and
// `forge ci validate-kcl` use — and assert properties of the manifests
// that will actually be applied, not of the .k source that produces
// them. Rendering is the point: every defect these catch is invisible
// in the source and only exists in the output.
//
// Each check SKIPs when the project declares no environments, so
// `--kind cli` / `--kind library` projects (no deploy/) stay quiet.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/kclrender"
)

// k8sObject is the slice of a rendered manifest these checks reason
// about. Everything else stays as raw JSON so a schema addition
// upstream never breaks the parse.
type k8sObject struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   objectMeta     `json:"metadata"`
	Spec       map[string]any `json:"spec"`
}

type objectMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// manifestRootKey is the top-level KCL variable the deploy contract
// reserves for the applyable manifest list. `kcl run … -S manifests`
// selects it and emits a `---` document stream; `forge env deploy`
// extracts the same key. A render that does not export it has no
// applyable output at all.
const manifestRootKey = "manifests"

// outputRootKey is the sibling root carrying forge's JSON deploy
// contract — the same document `forge build` / `forge env deploy`
// consume. It is where a HOST-deployed service exists, since a host
// service has no manifest.
const outputRootKey = "output"

// parseHostServices reads the host-deployed services out of the `output`
// contract. Anything unparseable answers nil: this is a best-effort
// enrichment of a check that must keep working on a render that predates
// the contract, or whose shape has moved on.
func parseHostServices(raw json.RawMessage) []renderedService {
	if len(raw) == 0 {
		return nil
	}
	var contract struct {
		Services []renderedService `json:"services"`
	}
	if err := json.Unmarshal(raw, &contract); err != nil {
		return nil
	}
	var hosts []renderedService
	for _, s := range contract.Services {
		if s.Deploy.Type == "host" {
			hosts = append(hosts, s)
		}
	}
	return hosts
}

// envRender is one environment's render outcome.
type envRender struct {
	env string
	// hasManifestRoot records whether the render exported the reserved
	// `manifests` key (or was itself a bare list / single object, the
	// hand-written shapes that need no selection).
	hasManifestRoot bool
	// strayRoots are OTHER top-level keys that carry k8s-object-shaped
	// data. They are a silent trap: `-S manifests` never selects them,
	// so those objects render, review clean, and never reach a cluster.
	// The documented `output` sibling (the JSON contract forge build /
	// deploy consume) is NOT k8s-shaped and never lands here.
	strayRoots []string
	// invalid names entries of the manifest list that `kubectl apply`
	// would reject — a document with no apiVersion or no kind.
	invalid []string
	objects []k8sObject
	// hostServices are the env's HOST-deployed services, read from the
	// `output` JSON contract rather than from `manifests`.
	//
	// A host-mode environment runs its app as a process on the developer's
	// machine and gives the cluster only what must be in it. Such a service
	// has real env vars and a real command, but never becomes a container —
	// so a check that reads only manifests cannot see it at all, and would
	// report an env that migrates on every boot as having no way to migrate.
	hostServices []renderedService
	err          error
}

// renderedService is the slice of the `output.services` contract these
// checks reason about. Deliberately minimal: the contract is large and
// evolving, and every field read here is one more thing that can break.
type renderedService struct {
	Name    string        `json:"name"`
	Command []string      `json:"command"`
	EnvVars []renderedEnv `json:"env_vars"`
	Deploy  struct {
		Type string `json:"type"`
		// A host service's environment is composed onto the DEPLOY block,
		// not the service's own env_vars — the host adapter owns it, the
		// way a cluster service's env lands on the container. Reading only
		// the outer list finds an empty slice on every host service.
		EnvVars []renderedEnv `json:"env_vars"`
	} `json:"deploy"`
}

type renderedEnv struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// deployRenders renders every declared environment once and memoises
// the result on the Environment, so the four deploy checks that run in
// doctor's parallel phase share a single KCL evaluation.
func deployRenders(env *Environment) []envRender {
	env.deployOnce.Do(func() {
		env.deployCache = renderDeployEnvs(env.ProjectDir)
	})
	return env.deployCache
}

// renderDeployEnvs discovers deploy/kcl/<env>/main.k and renders each.
func renderDeployEnvs(projectDir string) []envRender {
	kclDir := filepath.Join(projectDir, "deploy", "kcl")
	entries, err := os.ReadDir(kclDir)
	if err != nil {
		return nil
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, statErr := os.Stat(filepath.Join(kclDir, e.Name(), "main.k")); statErr != nil {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	renders := make([]envRender, 0, len(names))
	for _, name := range names {
		// The main.k files resolve their imports (`..components`,
		// `..ingress`) and the kcl.mod vendor path relative to the project
		// root, so the project root is the work dir — same contract
		// `forge ci validate-kcl` uses.
		rel := filepath.Join("deploy", "kcl", name, "main.k")
		// `-D env=<name>` is not optional. It is what `option("env")`
		// reads, and env-conditionals key off it — including forge's own
		// kcl/schema.k, whose RenderedSecretKey check allows an inlined
		// literal secret only when option("env") is dev or e2e. Rendering
		// without it leaves the option Undefined, so that check fails for
		// EVERY environment including the two it permits, and the deploy
		// checks report a project non-applyable over a conditional that
		// would have been satisfied. internal/cli's renderKCLRaw passes the
		// same argument; the two renderers disagreeing is the bug.
		raw, rerr := kclrender.Run(projectDir, rel, []string{"env=" + name})
		if rerr != nil {
			renders = append(renders, envRender{env: name, err: rerr})
			continue
		}
		r := parseRender(raw)
		r.env = name
		renders = append(renders, r)
	}
	return renders
}

// parseRender resolves a rendered KCL result into the objects a deploy
// would actually apply, plus everything about the render's SHAPE that
// decides whether the apply works at all.
//
// The applyable invocation is `kcl run … -S manifests`: KCL always
// emits a module's top-level variables as a mapping (`manifests:` +
// the `output:` JSON contract forge build/deploy consume), and `-S`
// selects one of them and emits it as a `---` document stream. So the
// shape questions that matter are:
//
//   - is there a `manifests` root to select? Without it there is no
//     applyable output, whatever else the render exports.
//   - does every entry under it carry apiVersion + kind? That is
//     literally what kubectl validates.
//   - does any OTHER root carry k8s objects? Those are invisible: no
//     selection reaches them, so they render, review clean, and never
//     deploy.
//
// A bare list at the root, or a single object, is the hand-written
// shape that needs no selection at all — accepted as-is.
func parseRender(raw []byte) envRender {
	var root map[string]json.RawMessage
	if uerr := json.Unmarshal(raw, &root); uerr != nil {
		// Not an object: a bare list of manifests is applyable as-is.
		var list []k8sObject
		if lerr := json.Unmarshal(raw, &list); lerr != nil {
			return envRender{err: fmt.Errorf("render is neither an object nor a list: %w", uerr)}
		}
		return envRender{hasManifestRoot: true, objects: list, invalid: invalidObjects(manifestRootKey, list)}
	}

	// A single k8s object at the root is applyable as-is.
	if _, hasKind := root["kind"]; hasKind {
		var single k8sObject
		if serr := json.Unmarshal(raw, &single); serr != nil {
			return envRender{err: serr}
		}
		list := []k8sObject{single}
		return envRender{hasManifestRoot: true, objects: list, invalid: invalidObjects(manifestRootKey, list)}
	}

	keys := make([]string, 0, len(root))
	for k := range root {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := envRender{}
	out.hostServices = parseHostServices(root[outputRootKey])
	for _, k := range keys {
		var list []k8sObject
		if lerr := json.Unmarshal(root[k], &list); lerr != nil {
			continue // not a list at all — the `output` contract lands here
		}
		if k == manifestRootKey {
			out.hasManifestRoot = true
			out.objects = list
			out.invalid = invalidObjects(k, list)
			continue
		}
		// A non-reserved root that carries k8s objects never gets applied.
		if len(list) > 0 && (list[0].Kind != "" || list[0].APIVersion != "") {
			out.strayRoots = append(out.strayRoots, k)
		}
	}
	return out
}

// invalidObjects names the entries kubectl would reject outright.
func invalidObjects(root string, list []k8sObject) []string {
	var bad []string
	for i, o := range list {
		var missing []string
		if o.APIVersion == "" {
			missing = append(missing, "apiVersion")
		}
		if o.Kind == "" {
			missing = append(missing, "kind")
		}
		if len(missing) > 0 {
			name := o.Metadata.Name
			if name == "" {
				name = "<unnamed>"
			}
			bad = append(bad, fmt.Sprintf("%s[%d] (%s): no %s", root, i, name, strings.Join(missing, ", ")))
		}
	}
	return bad
}

// podTemplateKinds are the workload kinds whose pod template sits
// directly at spec.template.
//
// Job belongs here with the long-running kinds even though it runs to
// completion: its pod carries a ServiceAccount and a full environment,
// which is what the SA-binding and secret checks read. Omitting it made
// a migrate Job that correctly bound its SA report as unbound, and hid a
// plaintext credential on the one workload most likely to carry a
// database URL.
var podTemplateKinds = map[string]bool{
	"Deployment":  true,
	"StatefulSet": true,
	"DaemonSet":   true,
	"ReplicaSet":  true,
	"Job":         true,
}

// containersOf returns (podSpec, containers) for a workload object, or
// (nil, nil) when the object carries no pod template.
func containersOf(o k8sObject) (map[string]any, []map[string]any) {
	podSpec, ok := podSpecOf(o)
	if !ok {
		return nil, nil
	}
	return podSpec, containerList(podSpec, "containers")
}

// podSpecOf locates a workload's pod spec, which sits one level deeper
// for a CronJob: it templates a Job, which templates the pod
// (spec.jobTemplate.spec.template.spec).
func podSpecOf(o k8sObject) (map[string]any, bool) {
	spec := o.Spec
	if o.Kind == "CronJob" {
		jobTmpl, ok := mapAt(spec, "jobTemplate")
		if !ok {
			return nil, false
		}
		if spec, ok = mapAt(jobTmpl, "spec"); !ok {
			return nil, false
		}
	} else if !podTemplateKinds[o.Kind] {
		return nil, false
	}
	tmpl, _ := mapAt(spec, "template")
	return mapAt(tmpl, "spec")
}

// initContainersOf returns a workload's initContainers. Separate from
// [containersOf] on purpose: an init container runs to completion, so
// the probe and (pod-effective) resource checks must NOT descend into
// it — but the secret check must, because a credential leaks the same
// way whichever container carries it.
func initContainersOf(o k8sObject) []map[string]any {
	podSpec, _ := containersOf(o)
	return containerList(podSpec, "initContainers")
}

// containerList extracts a named container list off a pod spec.
func containerList(podSpec map[string]any, key string) []map[string]any {
	if podSpec == nil {
		return nil
	}
	raw, _ := podSpec[key].([]any)
	containers := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		if m, isMap := c.(map[string]any); isMap {
			containers = append(containers, m)
		}
	}
	return containers
}

func mapAt(m map[string]any, key string) (map[string]any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m[key].(map[string]any)
	return v, ok
}

// renderPreamble is the shared skip/fail handling every deploy check
// opens with. It returns a non-nil result when the check cannot run.
func renderPreamble(env *Environment, what string) ([]envRender, *CheckResult) {
	renders := deployRenders(env)
	if len(renders) == 0 {
		r := CheckResult{Status: StatusSkip, Message: "no deploy/kcl/<env>/main.k — nothing to render"}
		return nil, &r
	}
	var broken []string
	for _, r := range renders {
		if r.err != nil {
			broken = append(broken, fmt.Sprintf("%s: %v", r.env, r.err))
		}
	}
	if len(broken) == len(renders) {
		r := CheckResult{
			Status:   StatusFail,
			Message:  fmt.Sprintf("could not render any environment to check %s", what),
			Evidence: strings.Join(broken, "\n"),
		}
		return nil, &r
	}
	return renders, nil
}

// CheckDeployManifests verifies that each environment renders to
// something `kubectl apply -f -` will accept.
//
// This is the gate `forge ci validate-kcl` used to skip: that command
// only asserted the KCL EVALUATES, so it printed "✅ All KCL manifests
// valid" over a render kubectl would reject — a green CI and a dead CD
// path, the worst possible combination. `validate-kcl` now calls this
// check, so the two can no longer disagree.
//
// What "applyable" means concretely: `kcl run … -S manifests` selects
// the `manifests` root and emits it as a `---` document stream. So the
// render must export that root, every entry under it must carry
// apiVersion + kind, and no OTHER root may carry k8s objects — those
// are unreachable by any selection and silently never deploy.
func CheckDeployManifests(_ context.Context, env *Environment) CheckResult {
	renders, early := renderPreamble(env, "manifest shape")
	if early != nil {
		return *early
	}

	var problems []string
	total := 0
	for _, r := range renders {
		if r.err != nil {
			problems = append(problems, fmt.Sprintf("%s: render failed: %v", r.env, r.err))
			continue
		}
		total += len(r.objects)
		if !r.hasManifestRoot {
			problems = append(problems, fmt.Sprintf(
				"%s: render exports no `%s` root — there is nothing for "+
					"`kcl run … -S %s | kubectl apply -f -` to select, so this env cannot deploy",
				r.env, manifestRootKey, manifestRootKey))
		}
		if len(r.invalid) > 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: %d manifest(s) `kubectl apply` would reject:\n    %s",
				r.env, len(r.invalid), strings.Join(r.invalid, "\n    ")))
		}
		if len(r.strayRoots) > 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: top-level key(s) %s carry k8s objects that no deploy applies — "+
					"`-S %s` cannot reach them; move them into `%s`",
				r.env, strings.Join(r.strayRoots, ", "), manifestRootKey, manifestRootKey))
		}
		if len(r.objects) == 0 && r.hasManifestRoot {
			problems = append(problems, fmt.Sprintf("%s: render produced no k8s objects", r.env))
		}
	}

	if len(problems) > 0 {
		return CheckResult{
			Status:   StatusFail,
			Message:  fmt.Sprintf("%d environment(s) render to non-applyable output", len(problems)),
			Evidence: strings.Join(problems, "\n"),
		}
	}
	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d env(s), %d manifest(s) — all applyable", len(renders), total),
	}
}

// CheckDeployProbes verifies every rendered container that serves a
// port declares both a readinessProbe and a livenessProbe.
//
// Without a readinessProbe a rolling update has no gate: a pod that
// binds its port and then wedges is added to the Service endpoints
// immediately, and a bad image reaches every replica at line speed.
// Without a livenessProbe nothing ever restarts a deadlocked process.
//
// The port is the discriminator, not the workload kind. A container
// that declares a containerPort is serving something and can be probed;
// one that declares none (a job that runs to completion, a custom
// binary entrypoint) has no endpoint to probe, and an HTTP probe
// against it would not be a safety net but a guaranteed
// CrashLoopBackOff. Declaring exactly ONE of the two probes fails
// regardless of ports — it is always a half-finished thought.
func CheckDeployProbes(_ context.Context, env *Environment) CheckResult {
	renders, early := renderPreamble(env, "probes")
	if early != nil {
		return *early
	}

	var missing []string
	checked, exempt := 0, 0
	for _, r := range renders {
		for _, o := range r.objects {
			_, containers := containersOf(o)
			for _, c := range containers {
				name, _ := c["name"].(string)
				_, hasReady := c["readinessProbe"]
				_, hasLive := c["livenessProbe"]
				ports, _ := c["ports"].([]any)

				if len(ports) == 0 && !hasReady && !hasLive {
					// Nothing to probe and nothing claimed — not a defect.
					exempt++
					continue
				}
				checked++
				var absent []string
				if !hasReady {
					absent = append(absent, "readinessProbe")
				}
				if !hasLive {
					absent = append(absent, "livenessProbe")
				}
				if len(absent) > 0 {
					missing = append(missing, fmt.Sprintf("%s/%s %s container %q: serves %d port(s) but declares no %s",
						r.env, o.Kind, o.Metadata.Name, name, len(ports), strings.Join(absent, " or ")))
				}
			}
		}
	}

	if checked == 0 && exempt == 0 {
		return CheckResult{Status: StatusSkip, Message: "no workload containers rendered"}
	}
	if len(missing) > 0 {
		return CheckResult{
			Status: StatusFail,
			Message: fmt.Sprintf("%d of %d serving container(s) ship no readiness/liveness probe "+
				"— rollouts have no gate and a wedged pod is never restarted", len(missing), checked),
			Evidence: strings.Join(missing, "\n"),
		}
	}
	msg := fmt.Sprintf("%d container(s) probe readiness + liveness", checked)
	if exempt > 0 {
		msg += fmt.Sprintf(" (%d serve no port — nothing to probe)", exempt)
	}
	return CheckResult{Status: StatusPass, Message: msg}
}

// CheckDeployResources verifies every rendered container sets CPU and
// memory requests and limits. Without requests the scheduler cannot
// place the pod correctly and it lands in the BestEffort QoS class,
// first to be evicted under node pressure.
func CheckDeployResources(_ context.Context, env *Environment) CheckResult {
	renders, early := renderPreamble(env, "resource limits")
	if early != nil {
		return *early
	}

	var missing []string
	checked := 0
	for _, r := range renders {
		for _, o := range r.objects {
			_, containers := containersOf(o)
			for _, c := range containers {
				checked++
				name, _ := c["name"].(string)
				res, _ := c["resources"].(map[string]any)
				var absent []string
				for _, section := range []string{"requests", "limits"} {
					sec, ok := mapAt(res, section)
					if !ok || len(sec) == 0 {
						absent = append(absent, section)
						continue
					}
					for _, dim := range []string{"cpu", "memory"} {
						if _, has := sec[dim]; !has {
							absent = append(absent, section+"."+dim)
						}
					}
				}
				if len(absent) > 0 {
					missing = append(missing, fmt.Sprintf("%s/%s %s container %q: missing %s",
						r.env, o.Kind, o.Metadata.Name, name, strings.Join(absent, ", ")))
				}
			}
		}
	}

	if checked == 0 {
		return CheckResult{Status: StatusSkip, Message: "no workload containers rendered"}
	}
	if len(missing) > 0 {
		return CheckResult{
			Status:   StatusFail,
			Message:  fmt.Sprintf("%d of %d rendered container(s) missing cpu/memory requests or limits", len(missing), checked),
			Evidence: strings.Join(missing, "\n"),
		}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("%d container(s) set cpu+memory requests and limits", checked)}
}

// sensitiveEnvNames are env-var name fragments whose value must never
// be a literal in a manifest. Matching is on the rendered NAME, so it
// survives any renaming of the config field that produced it.
var sensitiveEnvNames = []string{
	"PASSWORD", "PASSWD", "SECRET", "TOKEN", "PRIVATE_KEY", "API_KEY",
	"ACCESS_KEY", "CREDENTIAL", "DATABASE_URL", "DB_URL", "DSN", "CONNECTION_STRING",
}

// secretReferenceSuffixes name a variable that POINTS AT a credential
// rather than carrying one. The name of a Secret is not secret — a
// manifest has to carry it in the clear for a pod to reference it at all
// — and neither is the path a credential is mounted at.
//
// This is a narrow exemption on the SUFFIX, not a substring: it exempts
// IMAGE_PULL_SECRET_NAME and TOKEN_PATH, and does not exempt
// SECRET_NAME_ENCRYPTION_KEY, whose name merely mentions a name. Without
// it these were 68 of 89 findings on a real project — enough noise to
// bury the genuine plaintext credentials in the same report, and a check
// whose output is mostly false is one people learn to skip.
var secretReferenceSuffixes = []string{"_NAME", "_PATH", "_FILE", "_KEY_REF", "_SECRET_REF"}

// literalSecretEnvs are the environments allowed to carry a credential as
// a literal. It MIRRORS the allowance in forge's kcl/schema.k
// (RenderedSecretKey: `option("env") in ["dev", "e2e"]`) — the two must
// agree, or a project is failed here for what the schema permits.
//
// Matched on the exact environment name, like the schema's own check: a
// near-miss such as "dev-k8s" deploys to a real cluster and is NOT
// exempt.
var literalSecretEnvs = map[string]bool{"dev": true, "e2e": true}

func isSensitiveEnvName(name string) bool {
	upper := strings.ToUpper(name)
	matched := false
	for _, frag := range sensitiveEnvNames {
		if strings.Contains(upper, frag) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, suffix := range secretReferenceSuffixes {
		if strings.HasSuffix(upper, suffix) {
			return false
		}
	}
	return true
}

// isNonSecretLiteral rules out the values a credential can never take.
// It exists so a name-fragment match on a policy toggle
// (CORS_ALLOW_CREDENTIALS=false) does not read as a leaked secret: a
// boolean or a bare number is a switch, not a credential. An EMPTY
// value is deliberately NOT excluded — an empty literal is the same
// plaintext wiring, and it is precisely the slot an operator fills in
// with a real password.
func isNonSecretLiteral(v any) bool {
	s, ok := v.(string)
	if !ok {
		// A rendered non-string (bool, number) is a toggle by construction.
		return v != nil
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "false", "yes", "no", "on", "off":
		return true
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// CheckDeploySecrets verifies no rendered container carries a
// credential-shaped env var as a literal `value:`.
//
// A literal means the credential lives in a git-tracked .k file and in
// `kubectl get deployment -o yaml` for anyone with read access to the
// namespace. The correct shape — valueFrom.secretKeyRef — is what a
// config field marked `sensitive: true` in proto/config/v1/config.proto
// projects to; a credential-bearing field left unmarked is what puts a
// password back in the manifest.
//
// initContainers are scanned too: the migration step runs with the same
// env as the app, so a leak there is the same leak.
//
// An empty literal still fails: it is the same wiring, and it is what
// an operator will fill in with a real password.
func CheckDeploySecrets(_ context.Context, env *Environment) CheckResult {
	renders, early := renderPreamble(env, "secret handling")
	if early != nil {
		return *early
	}

	var leaks []string
	checked := 0
	for _, r := range renders {
		// dev and e2e may inline a credential, because forge's own KCL
		// schema says so: RenderedSecretKey permits `from = "literal"`
		// exactly when option("env") is dev or e2e, and refuses it in every
		// other environment. Failing a project here for the thing the schema
		// allows put the two halves of forge in disagreement, and left a
		// project with permanently-red findings it could not fix — which is
		// how a check stops being read, taking the prod findings in the same
		// list with it.
		//
		// A dev credential is `postgres:postgres` against a database bound to
		// the developer's own machine; an e2e credential is a throwaway whose
		// other half lives in the test's assertions. Neither is a secret in
		// any sense a Secret would protect.
		if literalSecretEnvs[r.env] {
			continue
		}
		for _, o := range r.objects {
			_, containers := containersOf(o)
			containers = append(containers, initContainersOf(o)...)
			for _, c := range containers {
				cname, _ := c["name"].(string)
				rawEnv, _ := c["env"].([]any)
				for _, e := range rawEnv {
					entry, ok := e.(map[string]any)
					if !ok {
						continue
					}
					name, _ := entry["name"].(string)
					if !isSensitiveEnvName(name) {
						continue
					}
					literalValue, literal := entry["value"]
					if literal && isNonSecretLiteral(literalValue) {
						continue
					}
					checked++
					if _, fromRef := entry["valueFrom"]; fromRef {
						continue
					}
					if !literal {
						continue
					}
					leaks = append(leaks, fmt.Sprintf("%s/%s %s container %q: env %s is a literal value: "+
						"— use valueFrom.secretKeyRef", r.env, o.Kind, o.Metadata.Name, cname, name))
				}
			}
		}
	}

	if checked == 0 {
		return CheckResult{Status: StatusPass, Message: "no credential-shaped env vars in the rendered manifests"}
	}
	if len(leaks) > 0 {
		return CheckResult{
			Status: StatusFail,
			Message: fmt.Sprintf("%d of %d credential-shaped env var(s) are plaintext in the manifest, not a Secret ref",
				len(leaks), checked),
			Evidence: strings.Join(leaks, "\n"),
		}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("%d credential env var(s), all sourced from a Secret", checked)}
}

// migrationJobHint matches the name of an object, or the command of a
// container, that plausibly applies schema migrations. Deliberately
// loose: this check asks "is there ANY migration step", so a false
// negative (failing a project that does migrate, by some name this
// misses) is the only expensive outcome.
func looksLikeMigration(s string) bool {
	return strings.Contains(strings.ToLower(s), "migrat")
}

// containerLooksLikeMigration reports whether a container is a
// migration step — by NAME or by any word of its command.
//
// Name OR command, deliberately: forge's own render names the init
// container "migrate" AND runs `<binary> db migrate up`, but a project
// that hand-writes the step may only match one.
func containerLooksLikeMigration(c map[string]any) bool {
	if name, _ := c["name"].(string); looksLikeMigration(name) {
		return true
	}
	cmd, _ := c["command"].([]any)
	for _, part := range cmd {
		if s, ok := part.(string); ok && looksLikeMigration(s) {
			return true
		}
	}
	return false
}

// hasMigrationStep reports whether a render carries anything that
// applies schema migrations before or alongside the rollout: a
// migration Job, a migration initContainer, or a container whose
// command says so.
//
// An initContainer only counts when it LOOKS like a migration. Any
// init container used to satisfy this — a wait-for-dependency sleep, a
// config-templating step — which made the check pass for projects with
// no migration path at all.
func hasMigrationStep(objects []k8sObject) bool {
	for _, o := range objects {
		if (o.Kind == "Job" || o.Kind == "CronJob") && looksLikeMigration(o.Metadata.Name) {
			return true
		}
		_, containers := containersOf(o)
		for _, c := range append(containers, initContainersOf(o)...) {
			if containerLooksLikeMigration(c) {
				return true
			}
		}
	}
	return false
}

// hostMigrationStep reports whether any HOST-deployed service applies
// migrations — either by running a migrate command, or by carrying
// AUTO_MIGRATE=true.
//
// Same two mechanisms the manifest path accepts, read off the `output`
// contract instead of a pod spec, so a host env and a cluster env are
// judged by the same standard.
func hostMigrationStep(services []renderedService) bool {
	for _, s := range services {
		if looksLikeMigration(s.Name) {
			return true
		}
		for _, word := range s.Command {
			if looksLikeMigration(word) {
				return true
			}
		}
		// Both lists: a host service's env is composed onto the deploy
		// block, while the service's own env_vars carry anything declared
		// directly on it.
		for _, e := range append(append([]renderedEnv{}, s.EnvVars...), s.Deploy.EnvVars...) {
			if e.Name == "AUTO_MIGRATE" && strings.EqualFold(strings.TrimSpace(e.Value), "true") {
				return true
			}
		}
	}
	return false
}

// autoMigrateEnabled reports whether every workload in the render runs
// its migrations at startup (AUTO_MIGRATE=true).
func autoMigrateEnabled(objects []k8sObject) bool {
	seen := false
	for _, o := range objects {
		_, containers := containersOf(o)
		for _, c := range containers {
			rawEnv, _ := c["env"].([]any)
			for _, e := range rawEnv {
				entry, ok := e.(map[string]any)
				if !ok {
					continue
				}
				if name, _ := entry["name"].(string); name != "AUTO_MIGRATE" {
					continue
				}
				value, _ := entry["value"].(string)
				if !strings.EqualFold(strings.TrimSpace(value), "true") {
					return false
				}
				seen = true
			}
		}
	}
	return seen
}

// CheckDeployMigrations verifies that an environment which ships SQL
// migrations has SOME way of applying them.
//
// The generated cloud envs set AUTO_MIGRATE=false — correct, because
// three replicas racing to migrate on startup is not a migration
// strategy. What replaces it is the scaffolded `migrate` workload: a
// one-shot job (`kind = "job"`, `before = [fw.BEFORE_ALL]`) running
// `<binary> db migrate up` over the migrations EMBEDDED in the image,
// serialized across replicas by a postgres advisory lock, and lowered to
// an initContainer so it is ordered before the app container by
// Kubernetes itself.
//
// The check does not care WHICH mechanism an env uses — a migration
// Job, an initContainer, a migrate command, or AUTO_MIGRATE=true all
// satisfy it. It cares that a project shipping .sql has SOMETHING, so a
// schema-changing release cannot deploy new code against the old schema
// and let the first request that touches a new column be the discovery
// mechanism.
func CheckDeployMigrations(_ context.Context, env *Environment) CheckResult {
	migrationsDir := filepath.Join(env.ProjectDir, "db", "migrations")
	entries, err := os.ReadDir(migrationsDir)
	if err != nil {
		return CheckResult{Status: StatusSkip, Message: "no db/migrations — nothing to apply"}
	}
	sqlCount := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			sqlCount++
		}
	}
	if sqlCount == 0 {
		return CheckResult{Status: StatusSkip, Message: "db/migrations is empty — nothing to apply"}
	}

	renders, early := renderPreamble(env, "migrations")
	if early != nil {
		return *early
	}

	var unmigrated []string
	for _, r := range renders {
		if r.err != nil || len(r.objects) == 0 {
			continue
		}
		if hasMigrationStep(r.objects) || autoMigrateEnabled(r.objects) {
			continue
		}
		// A HOST-deployed service migrates just as effectively as a
		// container does, and it is the normal shape for a dev env: the app
		// runs on the developer's machine with AUTO_MIGRATE=true while the
		// cluster holds only the pieces that must be in it. That service
		// never becomes a container, so reading manifests alone reports an
		// env that migrates on every boot as one with no way to migrate.
		if hostMigrationStep(r.hostServices) {
			continue
		}
		unmigrated = append(unmigrated, fmt.Sprintf(
			"%s: AUTO_MIGRATE is off and the render carries no migration Job, initContainer or migrate command "+
				"— a schema-changing release deploys new code against the old schema", r.env))
	}

	if len(unmigrated) > 0 {
		return CheckResult{
			Status: StatusFail,
			Message: fmt.Sprintf("%d of %d environment(s) ship %d SQL migration(s) with no way to apply them",
				len(unmigrated), len(renders), sqlCount),
			Evidence: strings.Join(unmigrated, "\n") +
				"\nfix: declare the migration as a one-shot workload in deploy/kcl/workloads.k —\n" +
				"      migrate = fw.Workload {\n" +
				"          name = \"migrate\"\n" +
				"          kind = \"job\"\n" +
				"          command = [\"/app/<project>\", \"db\", \"migrate\", \"up\"]\n" +
				"          before = [fw.BEFORE_ALL]\n" +
				"      }\n" +
				"    and add it to this environment's workload list. `before = [fw.BEFORE_ALL]` gates " +
				"EVERY workload without naming any, so it keeps holding when a workload is added later; " +
				"it renders an initContainer running the image's `db migrate up` (embedded migrations, " +
				"advisory-locked) before the app container starts. If this environment applies migrations " +
				"out of band instead, that is a legitimate answer — this check reads the render, so wire " +
				"the step you actually use.",
		}
	}
	return CheckResult{
		Status:  StatusPass,
		Message: fmt.Sprintf("%d SQL migration(s), applied by every environment", sqlCount),
	}
}

// CheckDeployServiceAccount verifies that a rendered ServiceAccount is
// actually bound to the workloads in its namespace.
//
// Emitting a ServiceAccount + Role + RoleBinding trio and then leaving
// serviceAccountName off the pod spec is worse than emitting nothing:
// the manifests read as if RBAC is scoped, while every pod runs as the
// namespace `default` SA with whatever that account can do.
func CheckDeployServiceAccount(_ context.Context, env *Environment) CheckResult {
	renders, early := renderPreamble(env, "service accounts")
	if early != nil {
		return *early
	}

	var unbound []string
	declared := 0
	for _, r := range renders {
		accounts := map[string]bool{}
		for _, o := range r.objects {
			if o.Kind == "ServiceAccount" {
				accounts[o.Metadata.Namespace+"/"+o.Metadata.Name] = false
				declared++
			}
		}
		if len(accounts) == 0 {
			continue
		}
		for _, o := range r.objects {
			podSpec, containers := containersOf(o)
			if len(containers) == 0 {
				continue
			}
			sa, _ := podSpec["serviceAccountName"].(string)
			if sa == "" {
				continue
			}
			key := o.Metadata.Namespace + "/" + sa
			if _, known := accounts[key]; known {
				accounts[key] = true
			}
		}
		names := make([]string, 0, len(accounts))
		for key, bound := range accounts {
			if !bound {
				names = append(names, key)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			unbound = append(unbound, fmt.Sprintf("%s: ServiceAccount %s is rendered but no pod spec sets "+
				"serviceAccountName — the workload runs as the namespace `default` SA", r.env, n))
		}
	}

	if declared == 0 {
		return CheckResult{Status: StatusSkip, Message: "no ServiceAccount rendered"}
	}
	if len(unbound) > 0 {
		return CheckResult{
			Status:   StatusFail,
			Message:  fmt.Sprintf("%d of %d rendered ServiceAccount(s) are never bound to a pod", len(unbound), declared),
			Evidence: strings.Join(unbound, "\n"),
		}
	}
	return CheckResult{Status: StatusPass, Message: fmt.Sprintf("%d ServiceAccount(s), all bound to a workload", declared)}
}
