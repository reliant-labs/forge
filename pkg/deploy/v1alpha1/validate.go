package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Validation is ALL-OR-NOTHING and runs BEFORE anything renders: every
// violation is collected and returned together (errors.Join), so an author
// fixes a spec in one pass rather than one error per deploy.
//
// These are the invariants that hold on EVERY destination. Hosted-only
// policy — the shape band, the registry allowlist, quota, the customer-prefix
// rule on SecretRef — is not here; see deploy.CheckShapeBand and the control
// plane.

var (
	envNameRE    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	sha256RE     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	hostnameRE   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$`)
	validDBKeys  = map[DatabaseCredentialKey]bool{DatabaseKeyURI: true, DatabaseKeyHost: true, DatabaseKeyPort: true, DatabaseKeyDBName: true, DatabaseKeyUsername: true, DatabaseKeyPassword: true}
	validNetwork = map[Network]bool{"": true, NetworkPublic: true, NetworkPrivate: true, NetworkNone: true}
)

// ValidateImage enforces forge's image invariant: an explicit registry host and
// a pin (tag or digest).
//
// Explicit registry because forge does not build this image and must not
// resolve a bare name against a registry the author does not control.
// Pinned because an unpinned image makes a redeploy silently change what
// runs. `:latest` counts as a tag, as it did in forge's KCL check. Refusing
// it is a separate policy decision, and making it here would widen the
// merge beyond what either side enforced.
func ValidateImage(image string) error {
	if image == "" {
		return errors.New("image is required")
	}
	if strings.ContainsAny(image, " \t\n") {
		return fmt.Errorf("image %q contains whitespace", image)
	}
	host, _, found := strings.Cut(image, "/")
	if !found || !(strings.ContainsAny(host, ".:") || host == "localhost") {
		return fmt.Errorf("image %q must name its registry host explicitly (e.g. ghcr.io/acme/app:v1): forge neither builds this image nor prefixes a registry onto it", image)
	}
	last := image[strings.LastIndex(image, "/")+1:]
	if !strings.Contains(image, "@sha256:") && !strings.Contains(last, ":") {
		return fmt.Errorf("image %q must be pinned to a tag or digest (':v1.2.3' or '@sha256:…'): an unpinned image makes a redeploy silently change what runs", image)
	}
	return nil
}

// Validate checks one env var: a legal name and at most one channel.
func (e EnvVar) Validate() error {
	var errs []error
	if !envNameRE.MatchString(e.Name) {
		errs = append(errs, fmt.Errorf("env var name %q must match %s", e.Name, envNameRE))
	}
	set := 0
	if e.Value != "" {
		set++
	}
	if e.SecretRef != nil {
		set++
		if e.SecretRef.Name == "" || e.SecretRef.Key == "" {
			errs = append(errs, fmt.Errorf("env var %s: secretRef needs both name and key", e.Name))
		}
	}
	if e.ManagedSecret != "" {
		set++
		if !envNameRE.MatchString(e.ManagedSecret) {
			errs = append(errs, fmt.Errorf("env var %s: managedSecret %q must be a bare logical name matching %s", e.Name, e.ManagedSecret, envNameRE))
		}
	}
	if e.DatabaseRef != nil {
		set++
		if !dnsLabelRE.MatchString(e.DatabaseRef.Name) {
			errs = append(errs, fmt.Errorf("env var %s: databaseRef.name %q must be the ManagedDatabase's name (an RFC-1123 label)", e.Name, e.DatabaseRef.Name))
		}
		if !validDBKeys[e.DatabaseRef.EffectiveKey()] {
			errs = append(errs, fmt.Errorf("env var %s: databaseRef.key %q must be one of uri, host, port, dbname, username, password", e.Name, e.DatabaseRef.Key))
		}
	}
	if set > 1 {
		errs = append(errs, fmt.Errorf("env var %s sets more than one of value / secretRef / managedSecret / databaseRef: a spec that says two things has no correct reading", e.Name))
	}
	return errors.Join(errs...)
}

// Validate checks the request/limit pairs. Zero means "use the default" and
// is valid. Negative values and a limit below its request are refused:
// Kubernetes itself rejects the latter at admission, and control-plane
// silently RAISED the limit, which hid the authoring mistake.
func (r Resources) Validate() error {
	var errs []error
	for name, v := range map[string]int64{
		"cpuRequestMillicores": r.CPURequestMillicores, "cpuLimitMillicores": r.CPULimitMillicores,
		"memoryRequestBytes": r.MemoryRequestBytes, "memoryLimitBytes": r.MemoryLimitBytes,
	} {
		if v < 0 {
			errs = append(errs, fmt.Errorf("resources.%s must not be negative", name))
		}
	}
	d := r.WithDefaults()
	if d.CPULimitMillicores < d.CPURequestMillicores {
		errs = append(errs, fmt.Errorf("resources.cpuLimitMillicores (%d) must be at least the request (%d)", d.CPULimitMillicores, d.CPURequestMillicores))
	}
	if d.MemoryLimitBytes < d.MemoryRequestBytes {
		errs = append(errs, fmt.Errorf("resources.memoryLimitBytes (%d) must be at least the request (%d)", d.MemoryLimitBytes, d.MemoryRequestBytes))
	}
	return errors.Join(errs...)
}

// Validate checks probe bounds.
func (h HealthCheck) Validate() error {
	var errs []error
	if h.Port < 1 || h.Port > 65535 {
		errs = append(errs, fmt.Errorf("healthCheck.port %d must be 1-65535", h.Port))
	}
	if h.Path != "" && !strings.HasPrefix(h.Path, "/") {
		errs = append(errs, fmt.Errorf("healthCheck.path %q must start with '/'", h.Path))
	}
	if h.InitialDelaySeconds < 0 || h.PeriodSeconds < 0 || h.TimeoutSeconds < 0 || h.FailureThreshold < 0 {
		errs = append(errs, errors.New("healthCheck timings must not be negative"))
	}
	return errors.Join(errs...)
}

func validateDomains(field string, domains []string) error {
	var errs []error
	seen := map[string]bool{}
	for _, d := range domains {
		if !hostnameRE.MatchString(d) || len(d) > 253 {
			errs = append(errs, fmt.Errorf("%s: %q is not a lowercase DNS hostname", field, d))
		}
		if seen[d] {
			errs = append(errs, fmt.Errorf("%s: %q is listed twice", field, d))
		}
		seen[d] = true
	}
	return errors.Join(errs...)
}

// Validate checks a SimpleBackend spec. It is the one Go home of every rule
// that used to live only in forge's KCL.
func (s SimpleBackendSpec) Validate() error {
	var errs []error
	if err := ValidateImage(s.Image); err != nil {
		errs = append(errs, err)
	}
	if !validNetwork[s.Network] {
		errs = append(errs, fmt.Errorf("network %q must be public, private or none", s.Network))
	}
	seenPorts := map[int32]bool{}
	for _, p := range s.Ports {
		if p < 1 || p > 65535 {
			errs = append(errs, fmt.Errorf("port %d must be 1-65535", p))
		}
		if seenPorts[p] {
			errs = append(errs, fmt.Errorf("port %d is listed twice", p))
		}
		seenPorts[p] = true
	}
	// A reachable backend that listens on nothing cannot be dialed, and a
	// "none" backend's ports would describe an address that does not exist.
	if s.ServesTraffic() && len(s.Ports) == 0 {
		errs = append(errs, fmt.Errorf("network %q requires at least one port: a reachable backend that listens on nothing cannot be dialed", s.EffectiveNetwork()))
	}
	if !s.ServesTraffic() && len(s.Ports) > 0 {
		errs = append(errs, errors.New("network none must declare no ports: no Service is rendered, so a port would describe an address that does not exist"))
	}
	if len(s.Domains) > 0 && !s.IsPublic() {
		errs = append(errs, errors.New("domains are only meaningful for network public: nothing serves a hostname for a private or none backend"))
	}
	if err := validateDomains("domains", s.Domains); err != nil {
		errs = append(errs, err)
	}
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
	if err := s.Resources.Validate(); err != nil {
		errs = append(errs, err)
	}
	if s.HealthCheck != nil {
		if err := s.HealthCheck.Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.StorageGiB < 0 {
		errs = append(errs, errors.New("storageGiB must not be negative; omit it for a stateless backend"))
	}
	return errors.Join(errs...)
}

// Validate checks a StaticSite spec.
func (s StaticSiteSpec) Validate() error {
	var errs []error
	if s.Bucket == "gs://" {
		errs = append(errs, errors.New("bucket 'gs://' has no bucket name after the scheme"))
	}
	if s.BasePath != "" && !strings.HasPrefix(s.BasePath, "/") {
		errs = append(errs, fmt.Errorf("basePath %q must start with '/'", s.BasePath))
	}
	for field, d := range map[string]string{"liveDigest": s.LiveDigest, "previousDigest": s.PreviousDigest} {
		if d != "" && !sha256RE.MatchString(d) {
			errs = append(errs, fmt.Errorf("%s %q must be a sha256:<64 hex> digest, never a tag", field, d))
		}
	}
	for _, d := range s.RetainedDigests {
		if !sha256RE.MatchString(d) {
			errs = append(errs, fmt.Errorf("retainedDigests entry %q must be a sha256:<64 hex> digest", d))
		}
	}
	if s.KeepReleases != nil && *s.KeepReleases != 0 && *s.KeepReleases < MinKeepReleases {
		errs = append(errs, fmt.Errorf("keepReleases %d must be 0 (retain everything) or at least %d: fewer would delete the artifact a rollback needs", *s.KeepReleases, MinKeepReleases))
	}
	for _, p := range s.Entrypoints {
		if !strings.HasPrefix(p, "/") {
			errs = append(errs, fmt.Errorf("entrypoint %q must be an absolute site path", p))
		}
	}
	if s.CDN != nil {
		if s.CDN.URLMap == "" {
			errs = append(errs, errors.New("cdn.urlMap is required: it names the existing URL map invalidations are sent to"))
		}
		switch s.CDN.Invalidate {
		case "", InvalidateEntrypoints, InvalidateNone, InvalidateAll:
		default:
			errs = append(errs, fmt.Errorf("cdn.invalidate %q must be entrypoints, none or all", s.CDN.Invalidate))
		}
		for _, p := range s.CDN.ExtraInvalidatePaths {
			if !strings.HasPrefix(p, "/") {
				errs = append(errs, fmt.Errorf("cdn.extraInvalidatePaths entry %q must be an absolute site path", p))
			}
		}
	}
	if err := validateDomains("domains", s.Domains); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Validate checks a ManagedDatabase spec. Out-of-range values are REFUSED, not
// clamped (see WithDefaults).
func (s ManagedDatabaseSpec) Validate() error {
	var errs []error
	d := s.WithDefaults()
	if d.Engine != EnginePostgres {
		errs = append(errs, fmt.Errorf("engine %q is not supported; the only engine is postgres", s.Engine))
	}
	if d.Instances < 1 || d.Instances > MaxDatabaseInstances {
		errs = append(errs, fmt.Errorf("instances %d must be 1-%d: a larger topology is a different destination, not a bigger number", s.Instances, MaxDatabaseInstances))
	}
	if d.StorageGiB < 1 || d.StorageGiB > MaxDatabaseStorageGiB {
		errs = append(errs, fmt.Errorf("storageGiB %d must be 1-%d", s.StorageGiB, MaxDatabaseStorageGiB))
	}
	if d.DeletionPolicy != DeletionPolicyRetain && d.DeletionPolicy != DeletionPolicyDelete {
		errs = append(errs, fmt.Errorf("deletionPolicy %q must be retain or delete", s.DeletionPolicy))
	}
	return errors.Join(errs...)
}

// ValidateDatabaseName checks a ManagedDatabase's metadata.name. That name
// becomes the Cluster name unchanged and, with hyphens folded to underscores,
// the Postgres database and role names. Postgres silently TRUNCATES an
// identifier longer than 63 bytes, so two long names could address the same
// database. 40 characters leaves room for the "_app" role suffix, and an
// over-long name is refused, never truncated.
func ValidateDatabaseName(name string) error {
	if !dnsLabelRE.MatchString(name) || len(name) > 40 {
		return fmt.Errorf("database name %q must be an RFC-1123 label of at most 40 characters", name)
	}
	return nil
}
