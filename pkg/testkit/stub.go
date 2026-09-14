package testkit

import "fmt"

// StubNotConfigured is what a forge-synthesized test stub returns from a
// method whose ONLY result is `error`.
//
// # Why these methods, and only these
//
// The generated test harness auto-stubs every required interface-typed
// Deps field so NewTest<Service>(t) constructs without the test wiring
// each collaborator by hand. Each stub method returns its results' zero
// values — which is right for a method that returns a VALUE: the zero of
// []string is "no rows", of *Doc is "not found", and a caller can inspect
// it and act.
//
// A method returning only `error` has no such value. Its zero is nil, and
// nil from an error-only method is not an empty answer — it is an
// affirmative claim that the operation SUCCEEDED, which a stub that did
// nothing has not earned. Where that method is a policy check, nil means
// PERMITTED, so a test that never overrode the field ran with the check
// disabled and asserted against nothing.
//
// That is not hypothetical. A dogfood run wrote a test asserting that a
// caller without the required role could not obtain an upload URL. It
// failed: the handler returned the URL, because the auto-stub had
// authorized the call. The application's own authorization code was
// correct and the harness had switched it off, silently, with no output
// anywhere saying so.
//
// # The same default forge already makes for mocks
//
// contractkit.MockNotSet is this error's twin: a forge-generated
// MockService returns it when a method is invoked whose Func field the
// test never assigned. Both say the same thing — "this double was never
// configured for this call" — and both turn a silent wrong answer into a
// loud, named failure. The two generated doubles projected from one
// contract previously disagreed about which way to fail; they no longer
// do.
//
// The pair is deliberately NOT one function. contractkit is imported by
// generated production-adjacent mock code; testkit is the test-harness
// library. Sharing would couple them for a one-line format string.
//
// # What a test does about it
//
// Override the field. The harness generates a With<Service>Deps option
// for exactly this:
//
//	svc := documents.NewTestDocuments(t,
//	    documents.WithDocumentsDeps(func(d *documents.Deps) {
//	        d.Policy = &policy.MockService{RequireRoleFunc: allowEditor}
//	    }))
//
// The error names the stub type and the method, so a test that trips it
// is told precisely which collaborator to configure.
func StubNotConfigured(stubType, method string) error {
	return fmt.Errorf("%s.%s: forge test stub not configured — this stub returns only error, "+
		"so returning nil would claim a success it never performed (for a policy check, nil means ALLOW). "+
		"Supply a real implementation for this dependency via the generated With<Service>Deps option",
		stubType, method)
}
