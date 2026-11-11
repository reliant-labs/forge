// File: trusted_edge.go — whether the process sits behind the Reliant platform's edge.
//
// On Reliant hosting every request reaches a workload through the platform's
// proxy, which deletes every copy of its edge headers a client sent and then
// stamps its own. Only there are those headers the platform's word; anywhere
// else a client can send them. So a process reads them only when it declares
// that edge in front of it: FORGE_TRUSTED_EDGE=reliant. Unset, or any other
// value, trusts no edge, and the headers are ignored.
//
// forge does not render FORGE_TRUSTED_EDGE for any workload yet, so no edge is
// trusted unless an author sets the variable by hand. forge #2079 renders it
// for forge.OnHosted workloads; #2068 (the edge visitor context) reads the same
// variable under the same names.

package observe

// The trusted-edge declaration.
const (
	// EnvTrustedEdge names the edge that fronts this process. "reliant"
	// (Reliant hosting) makes the edge headers trusted; anything else, or
	// unset, ignores them.
	EnvTrustedEdge = "FORGE_TRUSTED_EDGE"
	// TrustedEdgeReliant is EnvTrustedEdge's value on Reliant hosting.
	TrustedEdgeReliant = "reliant"
)

// TrustedEdge reports whether this process declares the Reliant platform's
// edge in front of it (EnvTrustedEdge=reliant), so the headers that edge
// stamps are the platform's and not a client's. It reads the environment on
// every call; the middleware that consult it read it once, when they are built.
func TrustedEdge() bool { return envValue(EnvTrustedEdge) == TrustedEdgeReliant }
