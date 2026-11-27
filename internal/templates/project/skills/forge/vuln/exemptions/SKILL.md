---
name: exemptions
description: Accepting vulnerability advisories with time-boxed, scoped exemptions; package, artifact and location matching, hosted capability requirements and fail-closed KCL edits.
---

# Vulnerability exemptions

**Accepting an advisory.** `forge vuln exempt` writes a time-boxed
`forge.VulnExemption` into the env's `deploy/kcl/<env>/main.k` (or its env document),
renders the env, and keeps the edit only if the rendered policy carries it:

```bash
forge vuln exempt prod CVE-2026-1234 --reason "unreachable: we never call the parser" --expires 2026-12-31
```

The reason is one line a reviewer can check; `--expires` is the last day it applies
(not in the past); a CVE id covers the GHSA of the same advisory. Running it again
with the same values changes nothing, with new ones replaces the entry. Review and
commit it.

**Scoping an exemption.** Unscoped, an exemption covers the advisory in every package
of every artifact of the env. `--package`, `--artifact` and `--location` (each
repeatable; the `packages`, `artifacts` and `locations` fields of
`forge.VulnExemption`) narrow it: a finding is exempt only where every one given names
it (AND across fields, OR within one).

| Flag | Matches | Example |
|------|---------|---------|
| `--package` | the package name, or its purl exactly or by a prefix ending at `@`, `/`, `?` or `#` | `stdlib`, `pkg:golang/stdlib` |
| `--artifact` | the release artifact's name, exactly | `api` |
| `--location` | an absolute path inside the artifact, exactly, or everything under one ending in `/` | `/usr/local/bin/grype` |

A Go standard-library advisory is the case `--location` exists for: every Go binary's
package is `stdlib` (`pkg:golang/stdlib@go1.x`), so only the path tells an unreachable
copy in a vendored tool from a reachable one in your own binary:

```bash
forge vuln exempt prod GO-2026-6859 --reason "grype never parses untrusted archives" \
  --expires 2026-12-31 --artifact api --location /usr/local/bin/grype
```

`forge vuln list <env> --json` lists each finding's `occurrences` (artifact and path),
which is where the values come from. One finding found in several places (two
artifacts, two binaries of one image) is exempt only when **every** place is covered,
by one exemption or several; anywhere else it still counts. A scope the scan cannot
answer (a finding with no path, against a `--location` exemption) covers nothing.
OS packages list their database and doc files as locations, so scope those by
`--package` and `--artifact` instead.

An exemption's identity is its id **and** its scope: the same id with another scope is
a second entry, added beside the first; the same id and scope with a new reason or
expiry replaces the one entry. The render records scoped exemptions in the shape's
`vuln_gate.scoped_exemptions`, apart from `exemptions`, so a reader that predates
scopes never reads one as an exemption of the advisory everywhere. Hosted declaration
and bundle recording refuse a scoped policy unless the control plane advertises both
`shape-vuln-scoped-exemptions` (decoding) and `evaluates-scoped-vuln-exemptions`
(occurrence-aware evaluation in promotion and vulnerability reads). Upgrade the
control plane before using scoped policies there; decoding alone is not support.
Local evaluation supports scopes. `ci.vuln_scan.exemptions` (forge.yaml) stays unscoped.

The edit is made through KCL's own parser, not text matching: it finds the literal
`VulnExemption`, `VulnPolicy`, `DeliveryPolicy` or `Bundle` the env declares and
splices one line in, leaving the rest of the file byte for byte. It **fails closed**:
when it cannot be sure which declaration is effective (two policies, a computed
`vuln_gate` or exemptions list, the id appearing somewhere it cannot read, a file
that does not parse), it writes nothing and prints the exact lines to add by hand.
