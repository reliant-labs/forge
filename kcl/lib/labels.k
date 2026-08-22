"""
Forge KCL — the forge-managed label set.

ONE renderer for `app.kubernetes.io/{name,managed-by,part-of}`, shared by
every manifest builder (lib/services.k, lib/rbac.k, lib/gateway.k,
workloads/expand.k).

WHY THIS MODULE EXISTS
======================

It used to be a private `_managed_labels` copied into each builder. The
copies drifted: `part_of` was added to the workloads/expand.k copy — with
a comment explaining that labelling an object with its own name made the
CD selector match nothing — and the lib/services.k copy kept hardcoding
`part-of = name`. lib/services.k is the path a Bundle-based project
renders through, so real deployments carried `part-of=<own name>` and
`kubectl get deployments -l app.kubernetes.io/part-of=<project>` matched
only the workloads whose label the project hardcoded itself. A CD
pipeline's "wait for rollouts" step iterated that empty selector, waited
on nothing, and reported success while three deployments never rolled.

A per-builder copy is what let a fix land in one renderer and not the
others, so the fix is one definition rather than four correct ones.

THE TWO LABELS ARE NOT INTERCHANGEABLE
======================================

  * `app.kubernetes.io/name`    — THIS object's workload.
  * `app.kubernetes.io/part-of` — the PROJECT it belongs to (the k8s
    recommended-labels "higher level application"), i.e. the handle that
    selects every workload of one release.

Collapsing them is precisely the defect above: a selector for the group
returns one member, and a selector for the whole release returns nothing.

`part_of` empty falls back to `name` so a caller with no project context
still emits a well-formed (if ungrouped) label set, and every existing
caller renders byte-identically until it passes one.
"""

managed_labels = lambda name: str, part_of: str = "" -> {str: str} {
    {
        "app.kubernetes.io/name" = name
        "app.kubernetes.io/managed-by" = "forge"
        "app.kubernetes.io/part-of" = part_of if part_of else name
    }
}
