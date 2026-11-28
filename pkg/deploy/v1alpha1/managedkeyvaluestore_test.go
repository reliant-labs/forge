package v1alpha1

import (
	"strings"
	"testing"
)

func TestManagedKeyValueStoreDefaults(t *testing.T) {
	d := ManagedKeyValueStoreSpec{}.WithDefaults()
	want := ManagedKeyValueStoreSpec{Engine: EngineValkey, Mode: KeyValueStoreModeDurable, MemoryMiB: 256, StorageGiB: 1, DeletionPolicy: DeletionPolicyRetain}
	if d != want {
		t.Fatalf("defaults = %+v, want %+v: durable and retain MUST be the defaults (a queue that forgets jobs is broken)", d, want)
	}
	if err := (ManagedKeyValueStoreSpec{}).Validate(); err != nil {
		t.Fatalf("an empty spec must validate via defaults: %v", err)
	}
	// A cache gets no volume: WithDefaults derives storage for durable only.
	if c := (ManagedKeyValueStoreSpec{Mode: KeyValueStoreModeCache}).WithDefaults(); c.StorageGiB != 0 {
		t.Fatalf("cache defaults = %+v; a cache has no volume", c)
	}
	if err := (ManagedKeyValueStoreSpec{Mode: KeyValueStoreModeCache, MemoryMiB: 8192}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedKeyValueStoreValidate(t *testing.T) {
	mustFail(t, ManagedKeyValueStoreSpec{Engine: "redis"}.Validate(), "must be valkey")
	mustFail(t, ManagedKeyValueStoreSpec{Mode: "persistent"}.Validate(), "cache or durable")
	mustFail(t, ManagedKeyValueStoreSpec{MemoryMiB: 300}.Validate(), "128, 256, 512, 1024, 2048, 4096, 8192")
	mustFail(t, ManagedKeyValueStoreSpec{MemoryMiB: 16384}.Validate(), "memoryMiB 16384")
	mustFail(t, ManagedKeyValueStoreSpec{Mode: KeyValueStoreModeCache, StorageGiB: 1}.Validate(), "cache mode persists nothing")
	mustFail(t, ManagedKeyValueStoreSpec{StorageGiB: MaxKeyValueStoreStorageGiB + 1}.Validate(), "must be 1-100")
	mustFail(t, ManagedKeyValueStoreSpec{StorageGiB: -1}.Validate(), "must be 1-100")
	// Below the floor (three times maxmemory) is refused with the floor named.
	mustFail(t, ManagedKeyValueStoreSpec{MemoryMiB: 8192, StorageGiB: 14}.Validate(), "at least 15 GiB")
	if err := (ManagedKeyValueStoreSpec{MemoryMiB: 8192, StorageGiB: 15}).Validate(); err != nil {
		t.Fatalf("the floor itself must validate: %v", err)
	}
	mustFail(t, ManagedKeyValueStoreSpec{DeletionPolicy: "purge"}.Validate(), "deletionPolicy")
	// Every violation at once.
	err := ManagedKeyValueStoreSpec{Engine: "redis", MemoryMiB: 3, DeletionPolicy: "purge"}.Validate()
	for _, want := range []string{"engine", "memoryMiB", "deletionPolicy"} {
		mustFail(t, err, want)
	}
}

// TestKeyValueStoreSizingTable pins every tier's derived numbers: what the
// pod requests is what is billed, so a change here is a price change.
func TestKeyValueStoreSizingTable(t *testing.T) {
	type row struct {
		mem, cpuReq, cpuLim, cacheMax, durableMax, storage, floor int32
	}
	rows := []row{
		{128, 125, 250, 102, 76, 1, 1},
		{256, 125, 250, 204, 153, 1, 1},
		{512, 125, 250, 409, 307, 1, 1},
		{1024, 250, 500, 819, 614, 2, 2},
		{2048, 500, 1000, 1638, 1228, 4, 4},
		{4096, 1000, 2000, 3276, 2457, 8, 8},
		{8192, 2000, 2000, 6553, 4915, 16, 15},
	}
	if len(rows) != len(KeyValueStoreMemoryTiers) {
		t.Fatalf("table has %d rows, KeyValueStoreMemoryTiers %d", len(rows), len(KeyValueStoreMemoryTiers))
	}
	for i, r := range rows {
		if KeyValueStoreMemoryTiers[i] != r.mem {
			t.Fatalf("tier %d = %d, want %d", i, KeyValueStoreMemoryTiers[i], r.mem)
		}
		cache := ManagedKeyValueStoreSpec{Mode: KeyValueStoreModeCache, MemoryMiB: r.mem}.Sizing()
		if want := (KeyValueStoreSizing{MemoryMiB: r.mem, CPURequestMillicores: r.cpuReq, CPULimitMillicores: r.cpuLim, MaxMemoryMiB: r.cacheMax}); cache != want {
			t.Errorf("cache %d: %+v, want %+v", r.mem, cache, want)
		}
		durable := ManagedKeyValueStoreSpec{MemoryMiB: r.mem}.Sizing()
		if want := (KeyValueStoreSizing{MemoryMiB: r.mem, CPURequestMillicores: r.cpuReq, CPULimitMillicores: r.cpuLim, MaxMemoryMiB: r.durableMax, StorageGiB: r.storage, MinStorageGiB: r.floor}); durable != want {
			t.Errorf("durable %d: %+v, want %+v", r.mem, durable, want)
		}
		if durable.StorageGiB < durable.MinStorageGiB {
			t.Errorf("durable %d: the default volume %d GiB is below its own floor %d", r.mem, durable.StorageGiB, durable.MinStorageGiB)
		}
		if err := (ManagedKeyValueStoreSpec{MemoryMiB: r.mem}).Validate(); err != nil {
			t.Errorf("durable %d with the default volume must validate: %v", r.mem, err)
		}
		if durable.MemoryBytes() != int64(r.mem)<<20 || durable.MaxMemoryBytes() != int64(r.durableMax)<<20 {
			t.Errorf("durable %d: bytes %d / %d", r.mem, durable.MemoryBytes(), durable.MaxMemoryBytes())
		}
	}
	// A declared volume is the one provisioned.
	if z := (ManagedKeyValueStoreSpec{MemoryMiB: 256, StorageGiB: 20}).Sizing(); z.StorageGiB != 20 {
		t.Errorf("declared storage = %d, want 20", z.StorageGiB)
	}
}

func TestValidateKeyValueStoreName(t *testing.T) {
	for _, ok := range []string{"q", "queue", "cache-1", strings.Repeat("a", MaxKeyValueStoreNameLength)} {
		if err := ValidateKeyValueStoreName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	// The name is also a Service name, which must start with a letter.
	for _, bad := range []string{"", "1queue", "Queue", "-q", "q-", "q.ueue", "q_ueue", strings.Repeat("a", MaxKeyValueStoreNameLength+1)} {
		mustFail(t, ValidateKeyValueStoreName(bad), "key-value store name")
	}
}

func TestManagedKeyValueStoreResourceValidate(t *testing.T) {
	for _, name := range []string{"q", "queue", "cache-1", strings.Repeat("a", MaxKeyValueStoreNameLength)} {
		store := ManagedKeyValueStore{}
		store.Name = name
		if err := store.Validate(); err != nil {
			t.Errorf("valid resource %q: %v", name, err)
		}
	}
	for _, name := range []string{"", "1queue", "Queue", "-q", "q-", "q.ueue", "q_ueue", strings.Repeat("a", MaxKeyValueStoreNameLength+1)} {
		store := ManagedKeyValueStore{}
		store.Name = name
		mustFail(t, store.Validate(), "metadata.name")
	}
	store := ManagedKeyValueStore{}
	store.Name = "queue"
	store.Spec.Engine = "redis"
	mustFail(t, store.Validate(), "spec: engine")
	// Report both resource and spec violations, not just the first one.
	store.Name = "1queue"
	err := store.Validate()
	mustFail(t, err, "metadata.name")
	mustFail(t, err, "spec: engine")
}

// TestEnvVarKeyValueStoreRef: keyValueStoreRef is a channel of its own —
// validated, defaulted to url, counted against "one channel per entry", and
// allowed on the restricted profile.
func TestEnvVarKeyValueStoreRef(t *testing.T) {
	ok := EnvVar{Name: "REDIS_URL", KeyValueStoreRef: &KeyValueStoreRef{Name: "queue"}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if k := ok.KeyValueStoreRef.EffectiveKey(); k != KeyValueStoreKeyURL {
		t.Fatalf("unset key = %q, want url", k)
	}
	for _, k := range KeyValueStoreCredentialKeys {
		if err := (EnvVar{Name: "X", KeyValueStoreRef: &KeyValueStoreRef{Name: "queue", Key: k}}).Validate(); err != nil {
			t.Errorf("key %s: %v", k, err)
		}
	}
	mustFail(t, EnvVar{Name: "X", KeyValueStoreRef: &KeyValueStoreRef{Name: "queue", Key: "uri"}}.Validate(), "keyValueStoreRef.key")
	mustFail(t, EnvVar{Name: "X", KeyValueStoreRef: &KeyValueStoreRef{Name: "1q"}}.Validate(), "keyValueStoreRef.name")
	mustFail(t, EnvVar{Name: "X", Value: "v", KeyValueStoreRef: &KeyValueStoreRef{Name: "queue"}}.Validate(), "more than one")

	w := WorkloadSpec{Kind: KindService, Image: "ghcr.io/acme/api:v1", Env: []EnvVar{ok, {Name: "REDIS_PASSWORD", KeyValueStoreRef: &KeyValueStoreRef{Name: "queue", Key: KeyValueStoreKeyPassword}}}}
	if err := w.Validate(ProfileRestricted); err != nil {
		t.Fatalf("a keyValueStoreRef must be admissible on the hosted (restricted) profile: %v", err)
	}
}
