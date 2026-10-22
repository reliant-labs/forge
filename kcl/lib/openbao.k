"""
Forge KCL - forge.OpenBao, the secret store an env runs itself.

`Bundle.openbao` is an opt-in component, declared like `observability` and
`identity`: unset renders nothing. When set, `render` (called from
kcl/render.k) lowers it to:

  * ONE `forge.dev/v1alpha1` record of kind `OpenBao`, in the applyable
    stream. Go (pkg/deploy/openbao, called by internal/cluster) expands it into
    the server, its TLS, Services, PodDisruptionBudget, NetworkPolicy and the
    two content-addressed converge Jobs - the same way a ManagedDatabase record
    becomes a CNPG Cluster. KCL validates and resolves; Go renders.
  * a resolved `openbao` block in the render output: where the store is, how a
    client reaches it and logs in. This is what the ExternalSecret render (SF5)
    and the release shape (release.RenderedOpenBao) read, so neither re-derives
    an address, an audience or a mount name.
  * the Secrets the store needs before it can start, as prerequisites the
    deploy preflight blocks on, while the bootstrap source is out of band.

WHAT IT DOES NOT DO YET, and says so (each is a later forge change, and each is
a named refusal or warning here rather than a silent omission):

  * the bootstrap source's delivery of the seal key and DB password, the
    recovery escrow, and in-cluster generation of StoreSource.generate values:
    the converge Job already has hook seams for them (Hooks.Escrow,
    Hooks.Generate);
  * the `public` endpoint (a warning: nothing is exposed until it lands);
  * the OIDC writer's role (a warning);
  * the Raft snapshot CronJob (a REFUSAL: a declared backup that does not run
    is worse than none);
  * clusters other than the one the store runs on: the access document already
    carries any number of mounts, but their issuers are registered by
    `forge cluster connect`, which the render does not read yet.

THE THREE PREREQUISITES live here, for every adopter:
  * TLS is on for every listener but a `transitional_reason` one, which warns
    on every render (schema.k OpenBaoListener);
  * no wildcard `read` without a stated reason (schema.k OpenBaoGrant, restated
    in the Go renderer); every reason is listed in `warnings`;
  * escrow of the seal key and recovery output is the bootstrap source's job
    (SF7); until then the Secrets are required prerequisites, not invented.
"""

import regex
import .labels as labels_lib

OPENBAO_KIND = "OpenBao"

# The service account in each namespace whose projected token logs in. It is
# pkg/secretaccess.DefaultServiceAccount; the SecretStore renderer requests its
# token and the roles bind to its subject.
STORE_SERVICE_ACCOUNT = "reliant-secrets"

# The JWT auth mount of a cluster, as pkg/secretaccess.MountName.
mount_name = lambda cluster_id: str -> str {
    "jwt-" + cluster_id
}

# The id of the cluster the store runs on is the caller's (ctx.cluster_id): the
# one lib/external_secret.k derives for the SecretStores that log in to it.
_valid_id = lambda id: str -> bool {
    bool(regex.match(id, r"^[A-Za-z0-9][A-Za-z0-9_-]*$"))
}

# The URL a client in the cluster reaches the store on (the active-node Service).
server_url = lambda ob: any -> str {
    "https://${ob.name}.${ob.namespace}.svc:${ob.listeners[0].port}"
}

# The audience a cluster's SecretStores request and the roles require. It pins a
# registration: a token minted for another audience is refused.
audience = lambda ob: any, id: str -> str {
    ("https://${ob.public.host}" if ob.public else server_url(ob)) + "/c/" + id
}

# The Secrets the store reads at start. Names are the Go renderer's contract
# (pkg/deploy/openbao Spec.UnsealSecret / StorageSecret).
unseal_secret = lambda ob: any -> str {
    ob.name + "-unseal"
}
storage_secret = lambda ob: any -> str {
    ob.name + "-storage"
}

# Names derived from the store's own name.
ca_configmap = lambda ob: any -> str {
    ob.tls.ca_configmap or (ob.name + "-ca")
}

_listener = lambda l: any -> any {
    {
        "port": l.port
        "tls": l.tls
        if l.transitional_reason:
            "transitionalReason": l.transitional_reason
    }
}

_storage = lambda s: any -> any {
    {
        "type": s.type
        if s.type == "raft":
            "raft": {
                "sizeGiB": s.size_gib
                if s.storage_class:
                    "storageClass": s.storage_class
            }
        if s.type == "postgres":
            "postgres": {
                if s.cloudsql_instance:
                    "cloudSQLInstance": s.cloudsql_instance
                    # The instance has no public IP in the platform's own use;
                    # the proxy dials the private address. A host is dialled as is.
                    "privateIP": True
                if s.host:
                    "host": s.host
                "port": s.port
                "database": s.database
                "role": s.role
                if s.gcp_service_account:
                    "gcpServiceAccount": s.gcp_service_account
                "maxParallel": s.max_parallel
            }
    }
}

_grant = lambda g: any -> any {
    {
        "path": g.path
        "capabilities": g.capabilities
        if g.wildcard_read_reason:
            "wildcardReadReason": g.wildcard_read_reason
    }
}

_consumer = lambda c: any -> any {
    {
        "name": c.name
        "workload": c.workload
        "namespace": c.namespace
        if c.grants:
            "grants": [_grant(g) for g in c.grants]
        if c.delegated:
            "delegated": True
    }
}

_writer = lambda w: any -> any {
    {"type": w.type, "serviceAccount": w.service_account} if w.type == "service_account" else {"type": w.type, "role": w.role}
}

# The ExternalSecrets that read from the store, grouped by namespace: the names
# each namespace's ExternalSecrets reference. Exactly these become exact-path
# policies (pkg/secretaccess); a namespace reading nothing gets no binding.
_bindings = lambda sources: [any], id: str -> [any] {
    _namespaces = sorted({s.namespace: 0 for s in sources})
    [b for b in [{
        "cluster": id
        "namespace": n
        "names": sorted({v: 0 for s in sources if s.namespace == n for _k, v in s.source.data})
    } for n in _namespaces] if len(b["names"]) > 0]
}

# The prerequisites the store needs before it can start, while the bootstrap
# source is out of band. Under a GCPSecretManager source the bootstrap
# component delivers them, so they are not declared here.
_prereq = lambda ob, name: str, key: str, reason: str -> any {
    {
        "type": "secret"
        "name": name
        "namespace": ob.namespace
        "keys": [key]
        "reason": reason
        "value_group": None
    }
}

_required = lambda ob, bootstrap_source: any -> [any] {
    ([
        _prereq(ob, unseal_secret(ob), "static-key", "OpenBao's static seal key (32 bytes, hex), read as BAO_STATIC_KEY. Escrow it OUTSIDE the cluster: it is also what makes backups readable. Provision it with: kubectl -n ${ob.namespace} create secret generic ${unseal_secret(ob)} --from-literal=static-key=\"$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \\n')\". A bootstrap_source that generates and escrows it is the declarative alternative")
    ] + ([
        _prereq(ob, storage_secret(ob), "connection-url", "OpenBao's PostgreSQL storage DSN (database ${ob.storage.database}, role ${ob.storage.role}), read as BAO_PG_CONNECTION_URL. Through the Cloud SQL proxy sidecar it dials 127.0.0.1:5432 with sslmode=disable")
    ] if ob.storage.type == "postgres" else [])) if (not bootstrap_source or bootstrap_source.type == "out_of_band") else []
}

# Everything one OpenBao declaration lowers to. `ctx` carries what only the
# Bundle knows:
#
#   project, env       the owner stamp and the KV directory (org, env)
#   target             the ClusterTarget the store runs on
#   cluster_id         that cluster's id in the store (see lib/external_secret.k)
#   secrets            Bundle.required_secrets
#   eso_namespaces     namespaces of the ESO installs on the same cluster
#   bootstrap_source   Bundle.bootstrap_source
#
# -> {record, block, required, namespace, cluster}
render = lambda ob: any, ctx: any -> any {
    _target = ctx.target
    assert _target, "Bundle.openbao has no cluster to run on. Set `runtime = forge.OnCluster {target = <ClusterTarget>}` on the OpenBao, or give the Bundle a cluster_target (or a workload bound to one): the store runs where its cluster_target says"
    assert not (ob.storage.type == "raft" and ob.storage.snapshot), \
        "OpenBao.storage.snapshot is declared, but the Raft snapshot CronJob is not rendered yet, and a backup that is declared and does not run is worse than none. Remove `snapshot` and back the PVCs up some other way (your CSI VolumeSnapshots), or use OpenBaoPostgres storage, which rides the database's own backups"

    assert ob.listeners[0].tls, "OpenBao.listeners[0] (port ${ob.listeners[0].port}) is plaintext. The first listener carries the cluster port and is the address clients and the converge Job use, so it must be the TLS one: put forge.OpenBaoListener {} first and any transitional plaintext listener after it"
    _id = ctx.cluster_id
    assert _valid_id(_id), "Bundle.openbao runs on cluster '${_id}', which is not usable as a store mount id (letters, digits, '_' and '-'). Register the cluster under a plain name: `connected_cluster` on its forge.ClusterTarget, or Bundle.connected_clusters = {\"${_target.cluster}\" = \"<name>\"}"
    _env = ctx.env or "default"
    _sources = [s for s in ctx.secrets if s.source and s.source.store.type == "openbao"]
    _bindings_ = _bindings(_sources, _id)
    _binding_ns = [b["namespace"] for b in _bindings_]
    _aud = audience(ob, _id)
    _spec = {
        "name": ob.name
        "namespace": ob.namespace
        "image": ob.image
        "replicas": ob.replicas
        "owner": "${ctx.project}.${_env}"
        "storage": _storage(ob.storage)
        "listeners": [_listener(l) for l in ob.listeners]
        "tls": {
            if ob.tls.secret:
                "secret": ob.tls.secret
            "caConfigMap": ca_configmap(ob)
            "duration": ob.tls.duration
        }
        "seal": {"keyID": ob.seal.key_id}
        "recoverySecret": ob.name + "-recovery"
        "kv": {"mount": ob.kv.mount, "maxVersions": ob.kv.max_versions}
        "ha": {
            "pdb": ob.ha.pdb
            "zoneSpread": ob.ha.zone_spread
            if ob.ha.priority_class:
                "priorityClass": ob.ha.priority_class
        }
        "audit": {"stdout": ob.audit.stdout, "file": ob.audit.file, "filePath": ob.audit.file_path}
        if ob.consumers:
            "consumers": [_consumer(c) for c in ob.consumers]
        "writer": _writer(ob.writer)
        "access": {
            "org": ctx.project
            "env": _env
            "serviceAccount": STORE_SERVICE_ACCOUNT
            "selfCluster": _id
            "clusters": [{"id": _id, "audience": _aud}]
            if _bindings_:
                "bindings": _bindings_
        }
        # Namespaces whose pods may reach the API: ESO's, and every namespace
        # that holds a SecretStore for this store. The converge Job publishes
        # the CA ConfigMap into the latter, so a SecretStore there can name it.
        "ingressNamespaces": sorted({n: 0 for n in ctx.eso_namespaces + _binding_ns})
        "caNamespaces": sorted({n: 0 for n in _binding_ns})
    }

    _warnings = [
        "OpenBao listener ${l.port} is PLAINTEXT (transitional): ${l.transitional_reason}. Clients that send a service-account token to it expose that token; drop the listener once they speak https" for l in ob.listeners if not l.tls
    ] + [
        "OpenBao consumer '${c.name}' reads ${g.path} by glob: ${g.wildcard_read_reason}" for c in ob.consumers for g in c.grants if g.wildcard_read_reason
    ] + (["OpenBao.public is declared (${ob.public.host}) but the public endpoint is not rendered by this forge yet: nothing is exposed, and clusters other than this one cannot reach the store"] if ob.public else []) \
      + (["OpenBao.writer is an OIDC writer (role '${ob.writer.role}'): its login role is not rendered by this forge yet, so no one can write values through it. Use the default forge.OpenBaoSAWriter {} until it lands"] if ob.writer.type == "oidc" else [])

    {
        "record": {
            apiVersion = "forge.dev/v1alpha1"
            kind = OPENBAO_KIND
            metadata = {
                name = ob.name
                namespace = ob.namespace
                labels = labels_lib.managed_labels(ob.name, ctx.project) | {"forge.dev/cluster" = _target.cluster}
            }
            spec = _spec
        }
        "block": {
            "name": ob.name
            "namespace": ob.namespace
            "image": ob.image
            "cluster": _target.cluster
            "connected_cluster": _target.connected_cluster or None
            # How a client reaches the store and logs in (SecretStore inputs).
            "server_url": server_url(ob)
            "ca_configmap": ca_configmap(ob)
            "kv_mount": ob.kv.mount
            "self_cluster": _id
            "auth_mount": mount_name(_id)
            "audience": _aud
            "service_account": STORE_SERVICE_ACCOUNT
            "org": ctx.project
            "env": _env
            "warnings": _warnings
        }
        "required": _required(ob, ctx.bootstrap_source)
        "namespace": ob.namespace
        "cluster": _target.cluster
    }
}
