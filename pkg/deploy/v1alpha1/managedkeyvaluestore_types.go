package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KeyValueStoreEngine is a key-value store engine. A closed set with one
// member: Valkey, the BSD-licensed Redis-protocol server. Like [Engine] on a
// database, the field exists so that adding an engine later widens an enum
// instead of migrating a resource that already has rows.
//
// +kubebuilder:validation:Enum=valkey
type KeyValueStoreEngine string

// EngineValkey is Valkey: the only engine. Its version is the platform's
// decision, never declared, and is reported in status.
const EngineValkey KeyValueStoreEngine = "valkey"

// KeyValueStoreMode is what the store promises about its data, and therefore
// what it does when it is full.
//
// +kubebuilder:validation:Enum=cache;durable
type KeyValueStoreMode string

const (
	// KeyValueStoreModeDurable persists every write (AOF, appendfsync
	// everysec, plus RDB snapshots) and never evicts (maxmemory-policy
	// noeviction): a write past maxmemory FAILS with an OOM error rather
	// than silently dropping a queued job. It is the default, because the
	// common reason to declare a store is a queue (Sidekiq, BullMQ, Celery),
	// and a queue that forgets jobs is a broken queue.
	KeyValueStoreModeDurable KeyValueStoreMode = "durable"
	// KeyValueStoreModeCache persists nothing and evicts the least recently
	// used keys when full (allkeys-lru). A restart empties it.
	KeyValueStoreModeCache KeyValueStoreMode = "cache"
)

// ManagedKeyValueStoreSpec is a dedicated key-value store (Valkey, speaking
// the Redis protocol): a queue backend, a cache, sessions, rate limits. It is
// a forge.KeyValueStore bound to a runtime forge renders (forge.OnCluster) or
// the hosted platform runs (forge.OnHosted): one single-node server with its
// own volume, in the environment's namespace.
//
// The credential lives in the Secret deploy.KeyValueStoreCredentialSecretName
// names ("forge-kvs-<name>"), with one key per [KeyValueStoreCredentialKey];
// a keyValueStoreRef env var reads it.
//
// SIZED BY A CLOSED LIST. MemoryMiB is one of [KeyValueStoreMemoryTiers], the
// pod's memory, and everything else (CPU, maxmemory, the default volume) is
// derived from it by [ManagedKeyValueStoreSpec.Sizing], so the bill can be
// read off the declaration.
//
// SCALARS AND CLOSED ENUMS ONLY. There is no valkey.conf passthrough, no
// eviction-policy override (the mode implies it), no version selector, no
// replicas, no TLS and no backup stanza. A field the tier would have to
// REJECT is a field it does not declare; each can be added later without
// breaking a declaration.
type ManagedKeyValueStoreSpec struct {
	// Engine is the store's engine. Default valkey.
	// +optional
	// +kubebuilder:default=valkey
	Engine KeyValueStoreEngine `json:"engine,omitempty"`

	// Mode is cache (no persistence, evicts when full) or durable (persists
	// every write, refuses writes when full). Default durable.
	// +optional
	// +kubebuilder:default=durable
	Mode KeyValueStoreMode `json:"mode,omitempty"`

	// MemoryMiB is the store's memory, in MiB: the pod's memory request and
	// limit, which is what is billed. One of 128, 256, 512, 1024, 2048, 4096
	// or 8192. Default 256. The dataset gets less than this (maxmemory), so a
	// persistence fork has headroom; Sizing says how much.
	// +optional
	// +kubebuilder:default=256
	// +kubebuilder:validation:Enum=128;256;512;1024;2048;4096;8192
	MemoryMiB int32 `json:"memoryMiB,omitempty"`

	// StorageGiB sizes the durable store's volume. Durable only: a cache
	// persists nothing, so a cache that sets it is refused. Unset derives
	// twice the store's memory (at least 1 GiB); a value below
	// three times its maxmemory is refused, because an AOF rewrite needs room
	// for the old file and the new one at once.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	StorageGiB int32 `json:"storageGiB,omitempty"`

	// DeletionPolicy is what deleting this resource does to the data.
	// Default retain. A cache has no volume, so it has nothing to retain:
	// deleting one always deletes it.
	// +optional
	// +kubebuilder:default=retain
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// KeyValueStoreMemoryTiers is every MemoryMiB a ManagedKeyValueStore may
// declare, smallest first. The KCL schema (kcl/schema.k KeyValueStore) and
// the control plane's admission read the same list.
var KeyValueStoreMemoryTiers = []int32{128, 256, 512, 1024, 2048, 4096, 8192}

// Defaults and ceilings of a ManagedKeyValueStore.
const (
	DefaultKeyValueStoreMemoryMiB int32 = 256
	MaxKeyValueStoreStorageGiB    int32 = 100

	// keyValueStoreMiBPerVCPU is the CPU band: one vCPU per 4 GiB of
	// memory. Valkey runs commands on one thread, so CPU beyond the band
	// buys little.
	keyValueStoreMiBPerVCPU = 4096
	// keyValueStoreCPUFloorMillicores is the smallest CPU request.
	keyValueStoreCPUFloorMillicores int32 = 125
	// keyValueStoreCPULimitCeilingMillicores caps the CPU limit, which is
	// otherwise twice the request (an AOF rewrite or RDB save bursts).
	keyValueStoreCPULimitCeilingMillicores int32 = 2000
	// The share of MemoryMiB the dataset may use (maxmemory). A durable
	// store forks to rewrite its AOF and save snapshots, and the fork's
	// copy-on-write pages need headroom; a cache does neither.
	keyValueStoreCacheMaxMemoryPercent   = 80
	keyValueStoreDurableMaxMemoryPercent = 60
)

// KeyValueStoreSizing is everything a ManagedKeyValueStore's MemoryMiB and
// Mode imply: what its pod requests and what Valkey is configured with. One
// function computes it, so the renderer, the hosted operator's admission and
// the bill agree.
type KeyValueStoreSizing struct {
	// MemoryMiB is the pod's memory request AND limit (Guaranteed QoS: the
	// kernel's OOM killer must never be the eviction policy).
	MemoryMiB int32
	// CPURequestMillicores is the pod's CPU request: one vCPU per 4 GiB,
	// at least 125m.
	CPURequestMillicores int32
	// CPULimitMillicores is twice the request, at most 2000m.
	CPULimitMillicores int32
	// MaxMemoryMiB is Valkey's maxmemory: 80% of MemoryMiB for a cache, 60%
	// for a durable store.
	MaxMemoryMiB int32
	// StorageGiB is the volume a durable store provisions (the declared
	// value, else the derived default). Zero for a cache, which has none.
	StorageGiB int32
	// MinStorageGiB is the smallest volume a durable store may declare:
	// three times its maxmemory, at least 1 GiB. Zero for a cache.
	MinStorageGiB int32
}

// MaxMemoryBytes is MaxMemoryMiB in bytes.
func (z KeyValueStoreSizing) MaxMemoryBytes() int64 { return int64(z.MaxMemoryMiB) << 20 }

// MemoryBytes is MemoryMiB in bytes.
func (z KeyValueStoreSizing) MemoryBytes() int64 { return int64(z.MemoryMiB) << 20 }

// ValidKeyValueStoreMemoryMiB reports whether m is one of
// KeyValueStoreMemoryTiers.
func ValidKeyValueStoreMemoryMiB(m int32) bool {
	for _, t := range KeyValueStoreMemoryTiers {
		if m == t {
			return true
		}
	}
	return false
}

// ceilDiv is a/b rounded up, for positive operands.
func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

// Sizing derives the store's resources from its (defaulted) spec. It does not
// validate: call Validate first, or read a refused spec's numbers knowing they
// describe nothing the platform runs.
func (s ManagedKeyValueStoreSpec) Sizing() KeyValueStoreSizing {
	d := s.WithDefaults()
	z := KeyValueStoreSizing{MemoryMiB: d.MemoryMiB}
	z.CPURequestMillicores = int32(int64(d.MemoryMiB) * 1000 / keyValueStoreMiBPerVCPU)
	if z.CPURequestMillicores < keyValueStoreCPUFloorMillicores {
		z.CPURequestMillicores = keyValueStoreCPUFloorMillicores
	}
	z.CPULimitMillicores = min(2*z.CPURequestMillicores, keyValueStoreCPULimitCeilingMillicores)
	if d.Mode == KeyValueStoreModeCache {
		z.MaxMemoryMiB = int32(int64(d.MemoryMiB) * keyValueStoreCacheMaxMemoryPercent / 100)
		return z
	}
	z.MaxMemoryMiB = int32(int64(d.MemoryMiB) * keyValueStoreDurableMaxMemoryPercent / 100)
	z.MinStorageGiB = int32(max(1, ceilDiv(3*int64(z.MaxMemoryMiB), 1024)))
	z.StorageGiB = d.StorageGiB
	return z
}

// defaultKeyValueStoreStorageGiB is a durable store's volume when none is
// declared: twice its memory, at least 1 GiB.
func defaultKeyValueStoreStorageGiB(memoryMiB int32) int32 {
	if memoryMiB <= 0 {
		return 1
	}
	return int32(max(1, ceilDiv(2*int64(memoryMiB), 1024)))
}

// WithDefaults fills every unset field. A durable store with no StorageGiB
// gets the derived default; a cache's StorageGiB is left as declared, for
// Validate to refuse. Out-of-range values are never clamped.
func (s ManagedKeyValueStoreSpec) WithDefaults() ManagedKeyValueStoreSpec {
	if s.Engine == "" {
		s.Engine = EngineValkey
	}
	if s.Mode == "" {
		s.Mode = KeyValueStoreModeDurable
	}
	if s.MemoryMiB == 0 {
		s.MemoryMiB = DefaultKeyValueStoreMemoryMiB
	}
	if s.Mode == KeyValueStoreModeDurable && s.StorageGiB == 0 {
		s.StorageGiB = defaultKeyValueStoreStorageGiB(s.MemoryMiB)
	}
	if s.DeletionPolicy == "" {
		s.DeletionPolicy = DeletionPolicyRetain
	}
	return s
}

// Validate checks a ManagedKeyValueStore spec, refusing rather than clamping
// anything the platform would not run as declared.
func (s ManagedKeyValueStoreSpec) Validate() error {
	var errs []error
	d := s.WithDefaults()
	if d.Engine != EngineValkey {
		errs = append(errs, fmt.Errorf("engine %q must be valkey", s.Engine))
	}
	if d.Mode != KeyValueStoreModeDurable && d.Mode != KeyValueStoreModeCache {
		errs = append(errs, fmt.Errorf("mode %q must be cache or durable", s.Mode))
	}
	memoryOK := ValidKeyValueStoreMemoryMiB(d.MemoryMiB)
	if !memoryOK {
		errs = append(errs, fmt.Errorf("memoryMiB %d must be one of %s", s.MemoryMiB, keyValueStoreMemoryTierList()))
	}
	switch d.Mode {
	case KeyValueStoreModeCache:
		if s.StorageGiB != 0 {
			errs = append(errs, errors.New(`storageGiB is set on a cache: cache mode persists nothing, so it has no volume; remove storageGiB or set mode "durable"`))
		}
	case KeyValueStoreModeDurable:
		if d.StorageGiB < 1 || d.StorageGiB > MaxKeyValueStoreStorageGiB {
			errs = append(errs, fmt.Errorf("storageGiB %d must be 1-%d", s.StorageGiB, MaxKeyValueStoreStorageGiB))
		} else if memoryOK {
			if floor := d.Sizing().MinStorageGiB; d.StorageGiB < floor {
				errs = append(errs, fmt.Errorf("storageGiB %d is too small for memoryMiB %d: a durable store needs at least %d GiB (three times its %d MiB maxmemory, room for an AOF rewrite beside the current file)", s.StorageGiB, d.MemoryMiB, floor, d.Sizing().MaxMemoryMiB))
			}
		}
	}
	if d.DeletionPolicy != DeletionPolicyRetain && d.DeletionPolicy != DeletionPolicyDelete {
		errs = append(errs, fmt.Errorf("deletionPolicy %q must be retain or delete", s.DeletionPolicy))
	}
	return errors.Join(errs...)
}

func keyValueStoreMemoryTierList() string {
	parts := make([]string, len(KeyValueStoreMemoryTiers))
	for i, t := range KeyValueStoreMemoryTiers {
		parts[i] = strconv.Itoa(int(t))
	}
	return strings.Join(parts, ", ")
}

// MaxKeyValueStoreNameLength is the longest ManagedKeyValueStore
// metadata.name. The name becomes a StatefulSet, its Services and its
// Secret ("forge-kvs-<name>"), and the platform prefixes it, so 40 leaves
// every derived name its room. An over-long name is refused, never
// truncated: two truncated names could address one store.
const MaxKeyValueStoreNameLength = 40

// keyValueStoreNameRE is a ManagedKeyValueStore's name: an RFC-1035 label
// (lowercase letters, digits and hyphens, starting with a letter), because
// the name is also the store's Service, and a Service name may not start
// with a digit.
var keyValueStoreNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ValidateKeyValueStoreName checks a ManagedKeyValueStore's metadata.name: 1
// to MaxKeyValueStoreNameLength lowercase letters, digits and hyphens,
// starting with a letter and ending with a letter or digit.
func ValidateKeyValueStoreName(name string) error {
	if len(name) > MaxKeyValueStoreNameLength || !keyValueStoreNameRE.MatchString(name) {
		return fmt.Errorf("key-value store name %q must be 1-%d lowercase letters, digits and hyphens, starting with a letter and ending with a letter or digit", name, MaxKeyValueStoreNameLength)
	}
	return nil
}

// KeyValueStoreCredentialKey selects one connection fact of a
// ManagedKeyValueStore: exactly the keys of the Secret
// deploy.KeyValueStoreCredentialSecretName names.
//
// +kubebuilder:validation:Enum=url;host;port;username;password
type KeyValueStoreCredentialKey string

const (
	// KeyValueStoreKeyURL is the whole connection URL,
	// redis://default:<password>@<host>:6379/0: what every Redis client
	// takes.
	KeyValueStoreKeyURL KeyValueStoreCredentialKey = "url"
	// KeyValueStoreKeyHost is the store's host name.
	KeyValueStoreKeyHost KeyValueStoreCredentialKey = "host"
	// KeyValueStoreKeyPort is the store's port.
	KeyValueStoreKeyPort KeyValueStoreCredentialKey = "port"
	// KeyValueStoreKeyUsername is the ACL user, "default", so a
	// password-only client connects too.
	KeyValueStoreKeyUsername KeyValueStoreCredentialKey = "username"
	// KeyValueStoreKeyPassword is the user's password.
	KeyValueStoreKeyPassword KeyValueStoreCredentialKey = "password"
)

// KeyValueStoreCredentialKeys is every KeyValueStoreCredentialKey, in the
// order forge.key_value_store_env emits them. The Secret carries all of
// them.
var KeyValueStoreCredentialKeys = []KeyValueStoreCredentialKey{
	KeyValueStoreKeyURL, KeyValueStoreKeyHost, KeyValueStoreKeyPort, KeyValueStoreKeyUsername, KeyValueStoreKeyPassword,
}

// Valid reports whether k is one of KeyValueStoreCredentialKeys.
func (k KeyValueStoreCredentialKey) Valid() bool {
	for _, v := range KeyValueStoreCredentialKeys {
		if k == v {
			return true
		}
	}
	return false
}

// KeyValueStoreRef names a ManagedKeyValueStore, in the workload's own
// namespace, and one connection fact of it to project. Like DatabaseRef it
// NAMES A STORE, NEVER A SECRET, so what it can resolve to is closed by
// construction.
type KeyValueStoreRef struct {
	// Name is the ManagedKeyValueStore's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`
	// Key is the connection fact. Empty means url: unlike a bucket, one
	// fact of a key-value store is the obvious default.
	// +optional
	Key KeyValueStoreCredentialKey `json:"key,omitempty"`
}

// EffectiveKey resolves an unset Key to url.
func (r KeyValueStoreRef) EffectiveKey() KeyValueStoreCredentialKey {
	if r.Key == "" {
		return KeyValueStoreKeyURL
	}
	return r.Key
}

// Validate checks a key-value store reference.
func (r KeyValueStoreRef) Validate() error {
	var errs []error
	if err := ValidateKeyValueStoreName(r.Name); err != nil {
		errs = append(errs, fmt.Errorf("keyValueStoreRef.name: %w", err))
	}
	if !r.EffectiveKey().Valid() {
		errs = append(errs, fmt.Errorf("keyValueStoreRef.key %q must be one of url, host, port, username, password", r.Key))
	}
	return errors.Join(errs...)
}

// ManagedKeyValueStoreStatus is the observed state of a ManagedKeyValueStore.
//
// There is no backup field yet: a durable store's data survives a restart and
// a lapse (its volume is kept), but it is not backed up. When backups land
// they get their own pointer field, nil meaning "not backed up".
type ManagedKeyValueStoreStatus struct {
	// +optional
	Phase Phase `json:"phase,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// StatefulSetName and ServiceName are the objects actually created.
	// RECORDED rather than re-derived, so a later change to the naming rule
	// cannot re-point a name that already holds data.
	// +optional
	StatefulSetName string `json:"statefulSetName,omitempty"`
	// +optional
	ServiceName string `json:"serviceName,omitempty"`

	// SecretName is the credential Secret, in the store's namespace.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// EngineVersion is the Valkey version running. It is read back rather
	// than declared, because the version is a platform decision.
	// +optional
	EngineVersion string `json:"engineVersion,omitempty"`

	// MaxMemoryBytes is the maxmemory the store runs with: the part of its
	// memory the dataset may use.
	// +optional
	MaxMemoryBytes int64 `json:"maxMemoryBytes,omitempty"`

	// ReadyAt is nil until the store has served, with its credential
	// published, at least once.
	// +optional
	ReadyAt *metav1.Time `json:"readyAt,omitempty"`

	// +optional
	Message string `json:"message,omitempty"`

	// Persistence is what the executor observed about a durable store's
	// persistence, nil for a cache. NIL AND "UNHEALTHY" ARE DIFFERENT
	// ANSWERS, as with DatabaseBackupStatus: a cache persists nothing by
	// design, and must not report the state of a durable store whose AOF
	// writes are failing.
	// +optional
	Persistence *KeyValueStorePersistenceStatus `json:"persistence,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// KeyValueStorePersistenceStatus is whether a durable store's writes are
// reaching its volume.
type KeyValueStorePersistenceStatus struct {
	// AOFEnabled is whether the append-only file is on. A durable store
	// with it off is not durable.
	// +optional
	AOFEnabled bool `json:"aofEnabled,omitempty"`
	// LastRDBSaveAt is when the last snapshot completed. Nil means none
	// ever has.
	// +optional
	LastRDBSaveAt *metav1.Time `json:"lastRDBSaveAt,omitempty"`
	// LastBgsaveOK is whether the last snapshot succeeded.
	// +optional
	LastBgsaveOK bool `json:"lastBgsaveOK,omitempty"`
	// LastAOFRewriteOK is whether the last AOF rewrite succeeded.
	// +optional
	LastAOFRewriteOK bool `json:"lastAOFRewriteOK,omitempty"`
	// LastError is the last persistence error, cleared on success, surfaced
	// verbatim.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// ManagedKeyValueStore is a dedicated key-value store (Valkey).
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=mkvs,categories=forge
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Memory",type=integer,JSONPath=`.spec.memoryMiB`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ManagedKeyValueStore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ManagedKeyValueStoreSpec   `json:"spec,omitempty"`
	Status ManagedKeyValueStoreStatus `json:"status,omitempty"`
}

// Validate checks the resource's metadata.name and spec. Consumers validating
// a resource should call this rather than validating its spec alone.
func (s ManagedKeyValueStore) Validate() error {
	var errs []error
	if err := ValidateKeyValueStoreName(s.Name); err != nil {
		errs = append(errs, fmt.Errorf("metadata.name: %w", err))
	}
	if err := s.Spec.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("spec: %w", err))
	}
	return errors.Join(errs...)
}

// ManagedKeyValueStoreList contains a list of ManagedKeyValueStore.
//
// +kubebuilder:object:root=true
type ManagedKeyValueStoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ManagedKeyValueStore `json:"items"`
}
