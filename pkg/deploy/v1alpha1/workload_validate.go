package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// cronFieldRE is one field of a 5-field cron expression: digits, *, and
	// the - , / operators, or a three-letter month/day name.
	cronFieldRE    = regexp.MustCompile(`^([0-9*,/\-]+|[A-Za-z]{3}([,\-][A-Za-z]{3})*)$`)
	namedSchedules = map[string]bool{"@hourly": true, "@daily": true, "@weekly": true, "@monthly": true, "@yearly": true, "@annually": true}
	// annotationNameRE / annotationPrefixRE are the Kubernetes annotation
	// key grammar: [prefix/]name.
	annotationNameRE   = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	annotationPrefixRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
)

// kindCapabilities is which kinds may set each kind-specific field. It is
// the Go statement of kcl/workloads/capabilities.k's KIND_CAPABILITIES, for
// the fields that matrix gated. An absent kind is a deliberate claim that
// the field means nothing there, and Validate REFUSES it rather than drop
// it: a dropped declaration is a pod that runs without what its author
// wrote down, and the manifest looks fine because it is the thing that lost
// the information.
var kindCapabilities = map[string]map[WorkloadKind]bool{
	"ports":                     {KindService: true, KindWorker: true, KindOperator: true},
	"replicas":                  {KindService: true, KindWorker: true, KindOperator: true},
	"probes":                    {KindService: true, KindWorker: true},
	"storageGiB":                {KindService: true, KindWorker: true, KindOperator: true},
	"namespacedRBAC":            {KindService: true, KindWorker: true, KindJob: true, KindCron: true},
	"clusterRBAC":               {KindOperator: true},
	"crds":                      {KindOperator: true},
	"group":                     {KindOperator: true},
	"version":                   {KindOperator: true},
	"leaderElection":            {KindOperator: true},
	"schedule":                  {KindCron: true},
	"before":                    {KindJob: true},
	"deployPhase":               {KindJob: true},
	"serviceAccountAnnotations": {KindService: true, KindWorker: true, KindJob: true, KindCron: true, KindOperator: true},
}

// kindCapabilityReasons explains each refusal, naming the alternative.
var kindCapabilityReasons = map[string]string{
	"ports":                     "a batch kind (job/cron) runs to completion and a tool is never scheduled, so there is no pod anything can dial; use kind service or worker",
	"replicas":                  "a batch pod's concurrency belongs to its Job or CronJob and a tool is never scheduled, so Kubernetes has nowhere to put this value",
	"probes":                    "probes gate traffic and restart long-running pods; a batch pod runs to completion, an operator's manager serves its own health, and a tool is never scheduled",
	"storageGiB":                "a ReadWriteOnce volume is mounted by a long-running pod; a batch pod or a tool has nothing to keep it for",
	"namespacedRBAC":            "an operator's permissions are cluster-scoped, so it uses clusterRBAC; a tool is never scheduled",
	"clusterRBAC":               "watching resources across namespaces is what makes a workload an operator; use namespacedRBAC for access in the workload's own namespace, or declare kind operator",
	"crds":                      "crds names the custom resources a controller-runtime manager owns; declare kind operator",
	"group":                     "group is the API group of an operator's CRDs; declare kind operator",
	"version":                   "version is the API version of an operator's CRDs; declare kind operator",
	"leaderElection":            "only an operator's manager elects a leader",
	"schedule":                  "only a cron runs on a schedule; a one-shot is kind job",
	"before":                    "before orders a one-shot job ahead of other workloads; it is only valid for kind job",
	"deployPhase":               "deployPhase places a standalone job in the deploy; it is only valid for kind job",
	"serviceAccountAnnotations": "a tool is never scheduled, so forge renders no ServiceAccount to annotate",
}

// Validate checks a Workload spec against the invariants every destination
// shares and then against profile p. Every violation is collected and
// returned together.
//
// The structural rules come first, and they hold under EVERY profile: a spec
// that contradicts itself (a schedule on a job, storage on three replicas)
// has no correct rendering anywhere. The profile rules come second, and say
// only what a destination refuses to run.
func (s WorkloadSpec) Validate(p Profile) error {
	var errs []error
	kind := s.EffectiveKind()
	validKind := false
	if _, ok := KindProfiles[kind]; ok {
		validKind = true
	} else {
		errs = append(errs, fmt.Errorf("kind %q must be one of service, worker, job, cron, operator, tool", s.Kind))
	}

	// --- image ---
	if s.Image == "" {
		errs = append(errs, errors.New("image is required"))
	} else if strings.ContainsAny(s.Image, " \t\n") {
		errs = append(errs, fmt.Errorf("image %q contains whitespace", s.Image))
	} else if p == ProfileRestricted {
		// Registry-qualified and pinned only where the platform pulls
		// from a registry the tenant does not control. A cluster the
		// author operates may run a bare name imported into its nodes
		// (k3d image import), and refusing that would break the dev loop
		// for no safety gain.
		if err := ValidateImage(s.Image); err != nil {
			errs = append(errs, err)
		}
	}

	// --- kind capabilities ---
	if validKind {
		for _, field := range []string{
			"ports", "replicas", "probes", "storageGiB", "namespacedRBAC", "clusterRBAC", "crds", "group",
			"version", "leaderElection", "schedule", "before", "deployPhase", "serviceAccountAnnotations",
		} {
			if s.declares(field) && !kindCapabilities[field][kind] {
				errs = append(errs, fmt.Errorf("%s is not supported for kind %q: %s", field, kind, kindCapabilityReasons[field]))
			}
		}
	}

	// --- replicas and storage ---
	if s.Replicas < 0 {
		errs = append(errs, errors.New("replicas must not be negative; omit it for one replica"))
	}
	if s.StorageGiB < 0 {
		errs = append(errs, errors.New("storageGiB must not be negative; omit it for a stateless workload"))
	}
	if s.StorageGiB > 0 && s.Replicas > 1 {
		errs = append(errs, fmt.Errorf("storageGiB requires replicas 1 (got %d): the volume is ReadWriteOnce, so a second replica can never mount it, and the workload rolls out with Recreate", s.Replicas))
	}

	// --- job / cron ---
	if kind == KindJob && len(s.Command) == 0 && len(s.Args) == 0 {
		errs = append(errs, errors.New("kind job requires command or args: a one-shot with nothing to run is not a job"))
	}
	if kind == KindCron {
		if s.Schedule == "" {
			errs = append(errs, errors.New("kind cron requires a schedule; a one-shot is kind job"))
		}
		if len(s.Command) == 0 && len(s.Args) == 0 {
			errs = append(errs, errors.New("kind cron requires command or args: a schedule that runs nothing is not a cron"))
		}
	}
	if s.Schedule != "" && !validSchedule(s.Schedule) {
		errs = append(errs, fmt.Errorf("schedule %q must be a 5-field cron expression or one of @hourly, @daily, @weekly, @monthly, @yearly", s.Schedule))
	}
	errs = append(errs, validateBefore(s.Before)...)
	switch s.DeployPhase {
	case "", DeployPhasePreRollout, DeployPhasePostRollout:
	default:
		errs = append(errs, fmt.Errorf("deployPhase %q must be pre-rollout or post-rollout", s.DeployPhase))
	}
	if s.DeployPhase != "" && len(s.Before) > 0 {
		errs = append(errs, errors.New("deployPhase is only for a standalone job: a job with before runs as an initContainer of the workloads it gates, so pod ordering already is its phase"))
	}

	// --- operator ---
	if kind == KindOperator && len(s.CRDs) == 0 {
		errs = append(errs, errors.New("kind operator must list at least one CRD kind in crds"))
	}

	// --- ports ---
	errs = append(errs, validatePorts(kind, s.Ports)...)

	// --- probes ---
	if s.Probes != nil {
		errs = append(errs, s.Probes.validate()...)
		if s.ProbePort() == 0 {
			errs = append(errs, errors.New("probes need a port: declare one in ports, or set probes.port"))
		}
	}

	// --- env ---
	seenEnv := map[string]bool{}
	for _, e := range s.Env {
		if err := e.Validate(); err != nil {
			errs = append(errs, err)
		}
		if seenEnv[e.Name] {
			errs = append(errs, fmt.Errorf("env var %s is declared twice", e.Name))
		}
		seenEnv[e.Name] = true
	}

	// --- resources ---
	if err := s.Resources.Validate(); err != nil {
		errs = append(errs, err)
	}

	// --- RBAC ---
	for field, rules := range map[string][]PolicyRule{"namespacedRBAC": s.NamespacedRBAC, "clusterRBAC": s.ClusterRBAC} {
		for i, r := range rules {
			if err := r.validate(fmt.Sprintf("%s[%d]", field, i)); err != nil {
				errs = append(errs, err)
			}
		}
	}

	// --- service account annotations ---
	for _, k := range sortedKeys(s.ServiceAccountAnnotations) {
		if !validAnnotationKey(k) {
			errs = append(errs, fmt.Errorf("serviceAccountAnnotations key %q must be [prefix/]name: name at most 63 characters of [A-Za-z0-9-_.] starting and ending alphanumeric, prefix a DNS subdomain (e.g. iam.gke.io/gcp-service-account)", k))
		}
	}

	// --- profile ---
	errs = append(errs, profileViolations(s, p)...)
	return errors.Join(errs...)
}

// declares reports whether the spec sets a kind-specific field. Replicas 1
// is the default and says nothing, so only more than one counts.
func (s WorkloadSpec) declares(field string) bool {
	switch field {
	case "ports":
		return len(s.Ports) > 0
	case "replicas":
		return s.Replicas > 1
	case "probes":
		return s.Probes != nil
	case "storageGiB":
		return s.StorageGiB > 0
	case "namespacedRBAC":
		return len(s.NamespacedRBAC) > 0
	case "clusterRBAC":
		return len(s.ClusterRBAC) > 0
	case "crds":
		return len(s.CRDs) > 0
	case "group":
		return s.Group != ""
	case "version":
		return s.Version != ""
	case "leaderElection":
		return s.LeaderElection != nil
	case "schedule":
		return s.Schedule != ""
	case "before":
		return len(s.Before) > 0
	case "deployPhase":
		return s.DeployPhase != ""
	case "serviceAccountAnnotations":
		return len(s.ServiceAccountAnnotations) > 0
	}
	return false
}

func validSchedule(s string) bool {
	if namedSchedules[s] {
		return true
	}
	fields := strings.Fields(s)
	if len(fields) != 5 {
		return false
	}
	for _, f := range fields {
		if !cronFieldRE.MatchString(f) {
			return false
		}
	}
	return true
}

func validateBefore(before []string) []error {
	var errs []error
	seen := map[string]bool{}
	for _, b := range before {
		if b != BeforeAll && !dnsLabelRE.MatchString(b) {
			errs = append(errs, fmt.Errorf("before entry %q must be a workload name (an RFC-1123 label) or %q", b, BeforeAll))
		}
		if seen[b] {
			errs = append(errs, fmt.Errorf("before entry %q is listed twice", b))
		}
		seen[b] = true
	}
	// The wildcard already means every workload. Mixing it with names is
	// a contradiction: the names are redundant, or the author believed the
	// list NARROWED the broadcast, which it does not.
	if seen[BeforeAll] && len(before) > 1 {
		errs = append(errs, fmt.Errorf("before = [%q] already gates every workload; remove the other names or drop the wildcard and enumerate", BeforeAll))
	}
	return errs
}

func validatePorts(kind WorkloadKind, ports []Port) []error {
	var errs []error
	names, numbers := map[string]bool{}, map[string]bool{}
	exposed := 0
	for _, p := range ports {
		if !dnsLabelRE.MatchString(p.Name) || len(p.Name) > 15 {
			errs = append(errs, fmt.Errorf("port name %q must be at most 15 lowercase alphanumerics or '-', starting and ending alphanumeric", p.Name))
		}
		if names[p.Name] {
			errs = append(errs, fmt.Errorf("port name %q is declared twice: Services and probes refer to a port by name", p.Name))
		}
		names[p.Name] = true
		if p.Port < 1 || p.Port > 65535 {
			errs = append(errs, fmt.Errorf("port %s: number %d must be 1-65535", p.Name, p.Port))
		}
		switch p.Protocol {
		case "", ProtocolTCP, ProtocolUDP:
		default:
			errs = append(errs, fmt.Errorf("port %s: protocol %q must be tcp or udp", p.Name, p.Protocol))
		}
		proto := p.Protocol
		if proto == "" {
			proto = DefaultPortProtocol
		}
		num := fmt.Sprintf("%d/%s", p.Port, proto)
		if numbers[num] {
			errs = append(errs, fmt.Errorf("port %s: %s is declared twice", p.Name, num))
		}
		numbers[num] = true
		if p.Expose {
			exposed++
			if kind != KindService {
				errs = append(errs, fmt.Errorf("port %s: expose is only for kind service, which is the only kind with a Service object to route a hostname to", p.Name))
			}
		}
	}
	if exposed > 1 {
		errs = append(errs, fmt.Errorf("%d ports set expose; at most one may, because a workload reports one hostname", exposed))
	}
	return errs
}

func (p Probes) validate() []error {
	var errs []error
	if p.Port < 0 || p.Port > 65535 {
		errs = append(errs, fmt.Errorf("probes.port %d must be 1-65535", p.Port))
	}
	if p.TCP && (p.ReadinessPath != "" || p.LivenessPath != "") {
		errs = append(errs, errors.New("probes.tcp probes with a TCP connect, so readinessPath and livenessPath would be ignored; set one or the other"))
	}
	for field, path := range map[string]string{"readinessPath": p.ReadinessPath, "livenessPath": p.LivenessPath} {
		if path != "" && !strings.HasPrefix(path, "/") {
			errs = append(errs, fmt.Errorf("probes.%s %q must start with '/'", field, path))
		}
	}
	if p.InitialDelaySeconds < 0 || p.PeriodSeconds < 0 || p.TimeoutSeconds < 0 || p.FailureThreshold < 0 {
		errs = append(errs, errors.New("probes timings must not be negative"))
	}
	return errs
}

func (r PolicyRule) validate(field string) error {
	var errs []error
	if len(r.APIGroups) == 0 {
		errs = append(errs, fmt.Errorf(`%s.apiGroups must list at least one group ("" is the core group)`, field))
	}
	if len(r.Resources) == 0 {
		errs = append(errs, fmt.Errorf("%s.resources must list at least one resource", field))
	}
	if len(r.Verbs) == 0 {
		errs = append(errs, fmt.Errorf("%s.verbs must list at least one verb", field))
	}
	return errors.Join(errs...)
}

func validAnnotationKey(k string) bool {
	prefix, name, hasPrefix := strings.Cut(k, "/")
	if !hasPrefix {
		name, prefix = prefix, ""
	}
	if len(name) == 0 || len(name) > 63 || !annotationNameRE.MatchString(name) {
		return false
	}
	return !hasPrefix || (len(prefix) <= 253 && annotationPrefixRE.MatchString(prefix))
}
