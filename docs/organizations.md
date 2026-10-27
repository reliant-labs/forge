# Choosing an organization

A control-plane machine token acts in exactly one organization. To work in two
organizations on one endpoint, keep two logins:

```sh
forge login --org <org-a-id>
forge login --org <org-b-id>
forge org use <org-b-id>
forge env deploy prod
```

`--org` takes an **organization id**, not a name or slug. Omit it at login to
ask the server for your default organization. The OAuth approval page chooses
among your memberships; forge checks that its answer matches an explicit
`--org` request. The server's `ListMyOrganizations` and
`SetDefaultOrganization` are human-session APIs, not APIs a forge machine token
can call. Use the console to inspect memberships or change that server default.

Login stores credentials; it **never changes a project's target**. `org use`
validates the selected org with a matching credential, then records it in
`.forge/organizations.json`, keyed by normalized endpoint, below the project
selected by the current directory or `--project-dir`. This is gitignored local
state, not a global current org. Use `--endpoint URL` when a project declares
several endpoints. `forge org use default` clears the project's choice; it does
not change your server-side default.

Hosted deploys, registry authentication, releases, secrets, gates and observation
resolve credentials for that project choice. An unpinned project never picks
the first or last stored org, even if there is only one. Select one with
`forge org use`; a configured host credential helper can instead supply its
session's authoritative default. `forge cloud status <env>` shows the choice.

The shared `credentials.json` format is version 2. Organization credentials are
keyed by endpoint, org and client. Re-login rotates only the same org/client;
`forge logout --org <org-id>` removes only that entry. Without `--org`, logout
uses the project's selection, and refuses to guess if org-bound entries exist.
Logout forgets the local token; it does not revoke it on the server. Existing
version 1 entries are retained without assigning them an invented org. Old
binaries refuse the new format instead of destroying its entries; upgrade both
CLIs that share the file.

Flag/env tokens retain precedence. Before a scoped operation, forge checks an
opaque token's org against the control plane and refuses a mismatch. With
`login --token --no-verify --org`, the declared org is stored but is still
verified before use. Claimable deploy credentials remain in their separate
project/env client namespaces; ordinary lookup and logout cannot adopt or
remove them. Clear a project org selection before an anonymous claimable flow.

## Credential-helper contract

`pkg/cloudcred.Request`, `Response` and `Token` carry `org_id`. An explicit org
request must be answered with exactly that org, including after refresh. Both
`Serve` and `Exec` reject an absent or mismatched org in that case. Helper memos
are keyed by endpoint, org and command; refresh never replaces another org's
entry. An older helper that cannot report org identity fails closed for explicit
orgs. The host-side helper must be upgraded separately (M1c).

The RPC request-field contract is control-plane M1a (#1365): the scoped deploy,
domain, cluster, browser-ingest and access-token requests carry `orgId` on the
Connect JSON wire. Machine-only token exchange and unscoped calls do not.
