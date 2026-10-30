package servicecatalog

import (
	"encoding/json"
	"sort"
)

// DiagnosticBundleMethodSetsOmitted marks a bundle copy whose code method sets
// were left out (ForBundle): an empty Methods list there means "not carried",
// never "has no methods". gen/ always has them.
const DiagnosticBundleMethodSetsOmitted = "bundle_method_sets_omitted"

// ForBundle reduces c to the copy a bundle carries: what a hosted reader
// serves to a browser, bounded by what its readers use rather than by the
// project's size.
//
//   - Each service's Descriptor keeps only Schemas, the request/response
//     shapes the RPC explorer renders. Methods, Messages, SchemaFiles, enums
//     and Go package data are the generator's own view, and were most of a
//     large project's catalog.
//   - The code method sets of components, implementation candidates and
//     bindings are dropped, and the DiagnosticBundleMethodSetsOmitted
//     diagnostic says so. The topology stays: every component, binding,
//     candidate, provider and client. Method sets are one entry per Go method
//     reachable through the composition graph (reliant: ~3,000 entries,
//     1.2 MB), and no hosted reader uses them.
//   - Idempotency is the closed set Method documents (Idempotency).
//
// The result is still schema v1 and decodes with Decode. Sources are left as
// they are; Locate runs before or after it.
func ForBundle(c *Catalog) {
	for i := range c.Services {
		s := &c.Services[i]
		s.Descriptor = schemasOnly(s.Descriptor)
		for j := range s.Methods {
			s.Methods[j].Idempotency = Idempotency(s.Methods[j].Idempotency)
		}
	}
	omitted := false
	drop := func(ms *[]CodeMethod) {
		if len(*ms) > 0 {
			omitted = true
		}
		*ms = []CodeMethod{}
	}
	for i := range c.Components {
		v := &c.Components[i]
		drop(&v.Methods)
		for j := range v.ImplementationCandidates {
			drop(&v.ImplementationCandidates[j].Methods)
		}
	}
	for i := range c.Bindings {
		drop(&c.Bindings[i].Methods)
	}
	if !omitted {
		return
	}
	for _, d := range c.Diagnostics {
		if d.Code == DiagnosticBundleMethodSetsOmitted {
			return
		}
	}
	c.Diagnostics = append(c.Diagnostics, Diagnostic{Code: DiagnosticBundleMethodSetsOmitted})
	sort.SliceStable(c.Diagnostics, func(i, j int) bool { return c.Diagnostics[i].Code < c.Diagnostics[j].Code })
}

// schemasOnly is descriptor with every key but Schemas removed: {} when it
// has none, or is not an object.
func schemasOnly(descriptor json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(descriptor, &fields); err != nil || fields["Schemas"] == nil {
		return json.RawMessage(`{}`)
	}
	out, err := json.Marshal(map[string]json.RawMessage{"Schemas": fields["Schemas"]})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return out
}

// Idempotency maps a method's protobuf idempotency level (the
// MethodOptions.IdempotencyLevel name, or "" when unset) to the catalog's
// closed set: IDEMPOTENT, NO_SIDE_EFFECTS, else UNKNOWN.
func Idempotency(level string) string {
	switch level {
	case "IDEMPOTENT", "NO_SIDE_EFFECTS":
		return level
	default:
		return "UNKNOWN"
	}
}
