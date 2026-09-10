package kclplugin

import "testing"

// TestAvailableMatchesRegistration pins the capability contract: Available
// must agree with whether Register actually installed the kcl_plugin.forge
// namespace. The two live in build-tag-split files (register_cgo.go /
// register_nocgo.go) and nothing but this test forces them to stay in sync
// — the failure mode they guard against is precisely a silent
// disagreement, where Available says yes and Register did nothing (or the
// reverse, which would refuse renders on a perfectly good binary).
//
// This test compiles under both tags and asserts the same invariant in
// each; namespaceRegistered is the tag-split ground truth (see
// available_cgo_test.go / available_nocgo_test.go). Build tags cannot be
// flipped inside one test process, so the CGO-free half is only exercised
// by a CGO-free `go test` run — which is exactly what a contributor or CI
// job building without CGO does.
func TestAvailableMatchesRegistration(t *testing.T) {
	Register()

	registered := namespaceRegistered()
	if got := Available(); got != registered {
		t.Fatalf("Available() = %v, but the kcl_plugin.forge namespace is registered = %v; "+
			"the build-tag halves of Register/Available have drifted", got, registered)
	}
}

// TestAvailableIsIdempotentWithRegister guards the calling convention in
// kclrender.Run: Register then Available, on every render. Neither call
// may change the answer on a second pass.
func TestAvailableIsIdempotentWithRegister(t *testing.T) {
	Register()
	first := Available()
	Register()
	if second := Available(); second != first {
		t.Fatalf("Available() changed across repeated Register calls: %v then %v", first, second)
	}
}
