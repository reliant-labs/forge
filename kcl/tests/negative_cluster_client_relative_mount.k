"""
Negative test — ClusterClient.mount_path must be ABSOLUTE.

The kubeconfig path inside the derived config_json is computed from
mount_path, so a relative mount_path would yield a relative
kubeconfigPath the workload can't resolve. The check block rejects it.
"""

import forge

_cp = forge.Cluster { name = "cp" }
_workload = forge.Cluster { name = "workload", owner = _cp }

_link = forge.ClusterClient {
    id = "workload"
    cluster = _workload
    in_cluster = _cp
    mount_path = "etc/workload-cluster"  # not absolute
}
