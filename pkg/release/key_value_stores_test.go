package release

import (
	"slices"
	"testing"
)

// TestKeyValueStoresCapabilityIsHandAdvertised: the wire string is frozen
// (control planes advertise it verbatim), and it is never claimed by
// BundleDocFeatures, which a control plane advertises on every pin bump:
// decoding a ManagedKeyValueStore is not running one.
func TestKeyValueStoresCapabilityIsHandAdvertised(t *testing.T) {
	if CapabilityManagesKeyValueStores != "manages-key-value-stores" {
		t.Errorf("CapabilityManagesKeyValueStores = %q; the wire string is frozen", CapabilityManagesKeyValueStores)
	}
	if slices.Contains(BundleDocFeatures(), CapabilityManagesKeyValueStores) {
		t.Errorf("BundleDocFeatures claims %s", CapabilityManagesKeyValueStores)
	}
}
