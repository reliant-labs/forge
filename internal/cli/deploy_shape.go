package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/doctor"
	"github.com/reliant-labs/forge/pkg/deploy"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// deployShapeOf is doctor's DeployShaper: it classifies one environment's raw
// render the way `forge env deploy <env>` does, so `forge ci validate-kcl`
// and `forge doctor --signal deploy` judge an env by the path that actually
// ships it.
//
// Nothing here is a second opinion. The destination is destinationOf (the
// `forge env topology` vocabulary, which shares its hosted/local split with
// buildDeployGroups and e58aa032's runtimeTargetFor), the hosted refusal is
// buildDeployGroups' own, and admission is deploytarget.PreflightHosted —
// the deploy's plan with placeholder digests. A rule added to any of them
// reaches CI with no change here.
//
// For a hosted env the platform's objects are rendered through
// pkg/deploy.Render, the function the control plane's tier operators call,
// into a placeholder namespace (the platform allocates the real one). They
// carry no hosted decoration (runtime class, pull policy, routes): that is
// platform policy the author does not declare and cannot change.
func deployShapeOf(env string, render []byte) (doctor.DeployShape, error) {
	entities, err := parseKCLEntities(render)
	if err != nil {
		return doctor.DeployShape{}, fmt.Errorf("read the deploy contract: %w", err)
	}
	if !rendersDeployContract(render) {
		// A hand-written render with no `output` contract: nothing to
		// classify from, so doctor keeps the strict manifest reading.
		return doctor.DeployShape{}, nil
	}
	shape := doctor.DeployShape{Destinations: destinationKindsOf(entities)}
	if destinationOf(entities) != destinationHosted {
		return shape, nil
	}
	shape.Hosted = true

	groups, err := buildDeployGroups(env, entities, "")
	if err != nil {
		shape.Refusal = err
		return shape, nil
	}
	var admitted []deploytarget.HostedPreflightItem
	for _, g := range groups {
		items, perr := deploytarget.PreflightHosted(g)
		if perr != nil {
			shape.Refusal = perr
			return shape, nil
		}
		admitted = append(admitted, items...)
	}
	shape.Workloads = len(admitted)

	objects, err := platformObjectsOf(admitted)
	if err != nil {
		// Admitted by the plan and still unrenderable is exactly the
		// state the platform would discover on its own, one workload at a
		// time, after the publish succeeded.
		shape.Refusal = err
		return shape, nil
	}
	shape.PlatformObjects = objects
	return shape, nil
}

// rendersDeployContract reports whether a render carries forge's `output`
// deploy contract. parseKCLEntities answers an empty entity set for a render
// without one, which is indistinguishable from an env that declares nothing
// — and the two must be judged differently.
func rendersDeployContract(render []byte) bool {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(render, &root); err != nil {
		return false
	}
	out, ok := root["output"]
	return ok && len(out) > 0 && string(out) != "null"
}

// destinationKindsOf is every destination kind the env deploys to — the
// per-kind half of destinationOf, which collapses several into "mixed".
// doctor needs the set: "does anything here go to a cluster" is the question
// an empty manifest stream has to answer.
func destinationKindsOf(e *KCLEntities) []string {
	if d := destinationOf(e); d != destinationMixed {
		return []string{d}
	}
	kinds := map[string]bool{}
	for _, s := range e.Services {
		switch s.Deploy.Type {
		case "cluster", "simple-backend":
			kinds[destinationCluster] = true
		case "compose":
			kinds[destinationCompose] = true
		case "host", "host-infra":
			kinds[destinationHost] = true
		case "external":
			kinds[destinationExternal] = true
		}
	}
	if len(e.Operators) > 0 || len(e.CronJobs) > 0 || len(e.Databases) > 0 {
		kinds[destinationCluster] = true
	}
	for _, f := range e.Frontends {
		if f.Deploy == nil {
			continue
		}
		switch f.Deploy.Type {
		case "firebase", "static-site":
			kinds[destinationStatic] = true
		case "cluster":
			kinds[destinationCluster] = true
		}
	}
	out := make([]string, 0, len(kinds))
	for k := range kinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// platformNamespace stands in for the namespace the control plane allocates.
// It only has to be well-formed: the objects it lands on are read, never
// applied.
const platformNamespace = "hosted-platform"

// platformObjectsOf renders the admitted tier workloads into the Kubernetes
// objects the platform runs for them, as one JSON list.
//
// A backend's workloadURL env vars are resolved to a placeholder first,
// exactly as the operator resolves them before it calls Render (which refuses
// a spec that still carries a reference). PreflightHosted already proved
// every reference resolvable; the value the platform allocates is not
// knowable here and nothing downstream reads it.
func platformObjectsOf(items []deploytarget.HostedPreflightItem) ([]byte, error) {
	var objs []any
	var errs []string
	for _, it := range items {
		var obj runtime.Object
		switch {
		case it.Backend != nil:
			spec := *it.Backend
			env, err := deploy.ResolveEnvWorkloadURLs(spec.Env, func(name string) (string, error) {
				return "https://" + name + ".hosted.invalid", nil
			})
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", it.Name, err))
				continue
			}
			spec.Env = env
			b := &v1alpha1.SimpleBackend{Spec: spec}
			b.Name = it.Name
			obj = b
		case it.Database != nil:
			db := &v1alpha1.ManagedDatabase{Spec: *it.Database}
			db.Name = it.Name
			obj = db
		default:
			// A StaticSite renders no Kubernetes objects: its executor is
			// the release planner, not an apply.
			continue
		}
		rendered, err := deploy.Render(obj, deploy.Context{Namespace: platformNamespace})
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", it.Name, err))
			continue
		}
		for _, r := range rendered {
			objs = append(objs, r.Object)
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("the platform could not render %d admitted workload(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
	}
	return json.Marshal(objs)
}
