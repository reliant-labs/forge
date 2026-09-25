// Package v1alpha1 is the ONE declaration of forge's three hosted deploy
// tiers — SimpleBackend, StaticSite and ManagedDatabase — in API group
// forge.dev.
//
// # ONE DECLARATION, TWO DESTINATIONS
//
// These types are the single source of truth for what an app author can say
// about a tier. forge's KCL schemas for the tiers are GENERATED from them
// (kcl/tiers/tiers_gen.k), and the control plane's CRDs are these types
// verbatim. That is the whole point of this package: the same declaration
// deploys self-hosted (forge renders it with deploy.Render and applies it) and
// hosted (the control plane receives it as a CR, calls the same Render, and
// composes its hosted concerns around the result). Before this package there
// were four definitions of SimpleBackend — forge's KCL schema, control-plane's
// CRD, control-plane's deployrender config and a JSONB row struct — using
// three unit systems and three different sets of env channels.
//
// # WHAT IS SPEC, WHAT IS NOT
//
// A spec holds ONLY what the app author declares. Two categories of
// information that the previous definitions put on the spec are absent from it,
// on purpose:
//
//   - HOSTED IDENTITY (org, customer, environment id, deployment id, and the
//     namespace the platform assigns). A spec field is something an author can
//     WRITE, and ownership must never be author-writable. The control plane
//     stamps identity as labels (see the Label* constants) on the CR it
//     publishes. It also makes "same declaration, two destinations" literally
//     true: a spec carrying an org id cannot be deployed to a cluster that has
//     no org.
//   - OBSERVED FACTS (allocated hostname, URL, phase, the digest actually
//     live). Those are status, written by whoever observed them — the
//     operator on hosted, `forge env status` computing it on read when
//     self-hosted.
//
// Cluster and namespace are also absent. Both describe the TARGET, not the
// app, and the target is the environment's deploy block. forge's own KCL
// doc admitted this ("the platform fills these in; they are not an app-facing
// knob"). The renderer takes the namespace as a RenderContext argument.
//
// # THE SCHEMA IS CLOSED, AND THAT IS THE ENFORCEMENT MECHANISM
//
// This is forge's doctrine, and it is kept on merit: a hosted tier that must
// reject configuration it will not honour does so by NOT DECLARING THE FIELD.
// An absent field needs no allowlist, cannot drift out of sync with one, and
// is refused for free — by KCL at author time, and by the API server's
// structural schema at apply time. So there is no replicas, no
// securityContext, no serviceAccount, no RBAC, no image pull secrets, no node
// placement, no command/args, no volumes and no raw-manifest passthrough. An
// app that needs any of those has outgrown the tier and should declare a
// forge.K8sCluster service, which is a different product.
//
// # INVARIANTS LIVE IN GO
//
// Every rule a tier enforces lives in Validate() here, and forge's KCL
// schema carries a generated copy. The previous design kept the rules
// (the resource ratio band, image pinning) only as KCL checks, and the
// hosted path — which never ran forge's KCL — bypassed them. The live dev
// cluster ran a SimpleBackend at 100m/128Mi, off the band the KCL check
// claimed to guarantee. The KCL copy is still useful, because it fails at
// author time with a message naming the field. But it is a convenience, and
// Validate() is the check.
//
// +kubebuilder:object:generate=true
// +groupName=forge.dev
//
//forge:exclude-contract: CRD API types (forge.dev/v1alpha1) with generated deepcopy and a pure Validate()
package v1alpha1
